// Native deterministic harness for the production C decision function. This
// substitutes kernel reads/map helpers; it does not certify LSM attachment.
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#define FENCE_TEST
#include "fence.c"
static struct config cfg = { .cgroup_id = 42, .ready = 1 };
static struct socket_id enrolled = { .inode = 123, .device = 7 };
static __u64 current = 42;
static int prior_error;
static void *lookup(void *map, const void *key) {
    if (map == &policy) return &cfg;
    if (map == &allowed && !memcmp(key, &enrolled, sizeof(enrolled))) return &cfg;
    return NULL;
}
static long probe_read(void *dst, __u32 n, const void *src) {
    // A prior LSM rejection must return before either hook decodes its peer.
    if (prior_error) abort();
    memcpy(dst, src, n);
    return 0;
}
static __u64 current_cgroup_id(void) { return current; }
int main(int argc, char **argv) {
    if (argc != 8) return 125;
    current = strtoull(argv[1], NULL, 10);
    struct kernfs_node kn = { .id = strtoull(argv[2], NULL, 10) };
    struct cgroup cg = { .kn = &kn };
    struct super_block sb = { .s_dev = strtoul(argv[4], NULL, 10) };
    struct inode ino = { .i_ino = strtoull(argv[3], NULL, 10), .i_sb = &sb };
    struct dentry de = { .d_inode = &ino };
    struct unix_sock peer = { .sk = { .sk_cgrp_data = { .cgroup = &cg } }, .path = { .dentry = &de } };
    cfg.ready = strtoul(argv[5], NULL, 10);
    if (atoi(argv[7])) peer.path.dentry = NULL;
    int prior = atoi(argv[6]);
    prior_error = prior;
    __u64 stream[4] = { 0, (__u64)&peer, 0, (__u64)prior };
    // unix_may_send receives a socket wrapper; stream_connect receives sock.
    struct socket datagram_peer = { .sk = &peer.sk };
    __u64 datagram[3] = { 0, (__u64)&datagram_peer, (__u64)prior };
    printf("%d %d\n", fence_stream(stream), fence_datagram(datagram));
    return 0;
}
