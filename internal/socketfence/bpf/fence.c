// SPDX-License-Identifier: GPL-2.0
// Minimal CO-RE declarations: offsets are relocated against the running kernel.
typedef unsigned long long __u64;
typedef unsigned int __u32;
typedef unsigned char __u8;
#define SEC(name) __attribute__((section(name), used))
#ifndef FENCE_TEST
#define CORE __attribute__((preserve_access_index))
#else
#define CORE
#endif
#define __uint(name, value) int (*name)[value]
#define __type(name, value) value *name
#define READ(dst, src) probe_read(&(dst), sizeof(dst), &(src))
struct kernfs_node { __u64 id; } CORE;
struct cgroup { struct kernfs_node *kn; } CORE;
struct sock_cgroup_data { struct cgroup *cgroup; } CORE;
struct sock { struct sock_cgroup_data sk_cgrp_data; } CORE;
struct super_block { __u32 s_dev; } CORE;
struct inode { unsigned long i_ino; struct super_block *i_sb; } CORE;
struct dentry { struct inode *d_inode; } CORE;
struct path { struct dentry *dentry; } CORE;
struct unix_sock { struct sock sk; struct path path; } CORE;
struct config { __u64 cgroup_id; __u64 generation; __u8 identity[16]; __u32 ready; __u32 pad; };
struct socket_id { __u64 inode; __u32 device; __u32 pad; };
struct { __uint(type, 2); __uint(max_entries, 1); __type(key, __u32); __type(value, struct config); } policy SEC(".maps");
struct { __uint(type, 1); __uint(max_entries, 256); __type(key, struct socket_id); __type(value, __u32); } allowed SEC(".maps");
#ifndef FENCE_TEST
static void *(*lookup)(void *, const void *) = (void *)1;
static long (*probe_read)(void *, __u32, const void *) = (void *)113;
static __u64 (*current_cgroup_id)(void) = (void *)80;
#else
static void *lookup(void *, const void *);
static long probe_read(void *, __u32, const void *);
static __u64 current_cgroup_id(void);
#endif

// Called with the resolved peer, after pathname/alias resolution and before any
// connection or datagram reaches it. Existing LSM errors must be preserved.
static __attribute__((always_inline)) inline int check(struct sock *other, int ret) {
    if (ret) return ret;
    __u32 zero = 0;
    struct config *cfg = lookup(&policy, &zero);
    if (!cfg || current_cgroup_id() != cfg->cgroup_id) return 0;
    if (!cfg->ready) return -1;
    // Sockets created within this runtime are
    // permitted without trusting any caller-supplied pathname or environment.
    struct cgroup *cg = 0;
    struct kernfs_node *kn = 0;
    __u64 id = 0;
    READ(cg, other->sk_cgrp_data.cgroup);
    if (cg) {
        READ(kn, cg->kn);
        if (kn) READ(id, kn->id);
        if (id == cfg->cgroup_id) return 0;
    }
    struct dentry *de = 0;
    struct inode *ino = 0;
    struct super_block *sb = 0;
    struct socket_id key = {};
    READ(de, ((struct unix_sock *)other)->path.dentry);
    if (de) READ(ino, de->d_inode);
    if (!ino) return -1; // Host abstract/unnamed sockets are not enrolled.
    READ(key.inode, ino->i_ino);
    READ(sb, ino->i_sb);
    if (!sb) return -1;
    READ(key.device, sb->s_dev);
    return lookup(&allowed, &key) ? 0 : -1;
}
SEC("lsm/unix_stream_connect")
int fence_stream(__u64 *ctx) { return check((struct sock *)ctx[1], (int)ctx[3]); }
SEC("lsm/unix_may_send")
int fence_datagram(__u64 *ctx) { return check((struct sock *)ctx[1], (int)ctx[2]); }
char LICENSE[] SEC("license") = "GPL";
#ifdef FENCE_SOURCE_SHA
const char source_digest[] SEC(".note.fence") = FENCE_SOURCE_SHA;
#endif
