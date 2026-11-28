/* Test-only crash-cut fixture. Never installed or configured by production.
 * Real fsync succeeds first. The Python owner independently verifies all raw
 * evidence while this thread is parked, then kills only its owned process. */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static int (*native_fsync)(int);
static pthread_once_t loaded = PTHREAD_ONCE_INIT;
static atomic_int parked;

static void load_native(void) { native_fsync = dlsym(RTLD_NEXT, "fsync"); }

static char *read_bounded(int fd, size_t limit) {
    struct stat info;
    if (fstat(fd, &info) || !S_ISREG(info.st_mode) || info.st_size < 0 ||
        (unsigned long long)info.st_size > limit) return NULL;
    size_t size = (size_t)info.st_size;
    char *bytes = malloc(size + 1);
    if (!bytes) return NULL;
    size_t offset = 0;
    while (offset < size) {
        ssize_t count = pread(fd, bytes + offset, size - offset, (off_t)offset);
        if (count < 0 && errno == EINTR) continue;
        if (count <= 0) { free(bytes); return NULL; }
        offset += (size_t)count;
    }
    bytes[size] = 0;
    return bytes;
}

int fsync(int fd) {
    pthread_once(&loaded, load_native);
    if (!native_fsync) { errno = ENOSYS; return -1; }
    int result = native_fsync(fd);
    int saved_errno = errno;
    if (result || atomic_load(&parked)) return result;
    const char *control = getenv("AGENT_WORKFLOW_TEST_FSYNC_CUT_CONTROL");
    const char *ready = getenv("AGENT_WORKFLOW_TEST_FSYNC_CUT_READY_FD");
    if (!control || !ready) return result;
    int input = open(control, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (input < 0) return result;
    char *rollout = read_bounded(input, PATH_MAX - 1);
    close(input);
    if (!rollout) return result;
    rollout[strcspn(rollout, "\n")] = 0;
    char descriptor[64], observed[PATH_MAX], journal[PATH_MAX];
    snprintf(descriptor, sizeof descriptor, "/proc/self/fd/%d", fd);
    ssize_t length = readlink(descriptor, observed, sizeof observed - 1);
    if (length < 0) { free(rollout); return result; }
    observed[length] = 0;
    if (strcmp(observed, rollout)) { free(rollout); return result; }
    size_t size = strlen(rollout);
    if (size < 6 || strcmp(rollout + size - 6, ".jsonl") || size + 32 >= sizeof journal) {
        free(rollout); return result;
    }
    memcpy(journal, rollout, size - 6);
    strcpy(journal + size - 6, ".managed-broker.json");
    free(rollout);
    input = open(journal, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (input < 0) return result;
    char *sealed = read_bounded(input, 16 * 1024 * 1024);
    close(input);
    char *raw = read_bounded(fd, 64 * 1024 * 1024);
    int accepted = sealed && strstr(sealed, "\"phase\":\"resultsAccepted\"");
    const char *first = raw ? strstr(raw, "\"type\":\"function_call_output\"") : NULL;
    int complete = first && strstr(first + 1, "\"type\":\"function_call_output\"");
    free(sealed); free(raw);
    if (accepted && complete && !atomic_exchange(&parked, 1)) {
        char *end;
        long target = strtol(ready, &end, 10);
        if (*end || target < 3 || target > INT_MAX) _exit(125);
        const char signal[] = "real-fsync-complete\n";
        if (write((int)target, signal, sizeof signal - 1) != sizeof signal - 1) _exit(125);
        for (;;) pause();
    }
    errno = saved_errno;
    return result;
}
