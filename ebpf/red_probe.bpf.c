// SPDX-License-Identifier: GPL-2.0
//
// red_probe.bpf.c — ZeroWatch Tier-1 zero-code RED instrumentation.
//
// What it does
// ------------
// For every process the user-space supervisor puts into `target_pids`, we
// watch the read/write family of syscalls and recognise plain-text HTTP/1.x:
//
//   1. inbound  read()/recvfrom() whose buffer starts with an HTTP method
//      ("GET ", "POST ", ...)  -> remember {start_ns, method, path} keyed by
//      (pid_tgid, fd) in `inflight`.
//   2. outbound write()/sendto() on the same (pid_tgid, fd) whose buffer starts
//      with "HTTP/1." -> parse the 3-digit status code, compute latency
//      = now - start_ns, emit one `struct red_event` on the `events` ring
//      buffer and delete the in-flight entry.
//
// User space (loader_linux_ebpf.go) folds events into per-route
// Requests / Errors / Latency datums, i.e. the RED method, without any
// change to the application, its image or its config.
//
// Why syscall tracepoints and not kprobes/uprobes?
//   * tracepoints are a stable ABI across kernel versions (no BTF relocations
//     needed for the arguments we read), so one object works on 4.18+ (5.8+
//     for the ring buffer map type);
//   * syscalls see traffic of every language runtime (Go, JVM, Node, Python)
//     as long as it is not TLS-encrypted.
//
// TLS (OpenSSL 1.1 / 3.x, dynamically linked)
//   The loader attaches uprobes to SSL_read/SSL_read_ex (entry + return) and
//   SSL_write/SSL_write_ex (entry) in each target's libssl. They feed the
//   plaintext buffers into the same on_inbound/on_outbound parser, keyed by
//   (tgid, SSL*) instead of (tgid, fd): a request and its response always go
//   through the same SSL object, so no SSL* -> fd mapping is needed.
//   Events from this path carry RED_F_TLS in flags.
//
// Limitations (honest list)
//   * HTTP/1.x only; HTTP/2 and gRPC frames are binary (ROADMAP).
//   * TLS: only OpenSSL's SSL_read*/SSL_write* API. Go crypto/tls, BoringSSL,
//     GnuTLS, NSS, rustls and kTLS offload are not seen.
//   * readv/writev, sendmsg/recvmsg and io_uring are not hooked.
//   * Pipelined requests on one connection overwrite each other's start time.
//   * The first bytes of the request must land in one read() (true in
//     practice: request lines are far smaller than socket buffers).
//
// Build (see ebpf/README.md):
//   clang -O2 -g -Wall -target bpf -D__TARGET_ARCH_x86 \
//         -I/usr/include/$(uname -m)-linux-gnu -c red_probe.bpf.c -o red_probe.bpf.o
//
// The program only needs <linux/bpf.h> and libbpf's helper headers; it does
// not depend on vmlinux.h. Tracepoint argument structs are declared locally
// below (their layout is fixed by the syscall tracepoint format). The few
// kernel structs used for PID-namespace translation are declared with
// preserve_access_index, so their offsets are CO-RE relocated against the
// running kernel's BTF (/sys/kernel/btf/vmlinux) at load time.

#include <linux/bpf.h>
#include <linux/types.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

// ---------------------------------------------------------------------------
// Sizes. Keep in sync with decodeEvent() in loader_linux_ebpf.go.
// ---------------------------------------------------------------------------
#define METHOD_LEN 8
#define PATH_LEN 64
#define COMM_LEN 16
#define HEAD_LEN 80 /* bytes copied from the user buffer to sniff a request */

// Event delivered to user space. Field order chosen so there is no implicit
// padding: 8+8+4+2+2(pad)+8+64+16 = 112 bytes.
struct red_event {
	__u64 ts_ns;       // bpf_ktime_get_ns() at response time
	__u64 latency_ns;  // response time - request time
	__u32 pid;         // tgid in the loader's PID namespace (what user space calls a PID)
	__u16 status;      // HTTP status code, 0 if unparsable
	__u16 flags;       // RED_F_*
	char method[METHOD_LEN];
	char path[PATH_LEN];
	char comm[COMM_LEN];
};

// A request that has been read but not yet answered.
struct inflight_req {
	__u64 start_ns;
	char method[METHOD_LEN];
	char path[PATH_LEN];
};

// red_event.flags: the request/response plaintext came from the TLS uprobes.
#define RED_F_TLS 1

// Key of an in-flight request: the global tgid plus either the file
// descriptor (syscall path, ssl = 0) or the SSL* (TLS path, fd = 0).
// We use the tgid half only so worker-thread handoff inside one process
// (common in Go and JVM servers) still pairs request and response.
// 16 bytes, no padding holes (keys are hashed bytewise).
struct conn_key {
	__u32 tgid;
	__u32 fd;
	__u64 ssl;
};

// Per-thread scratch state between sys_enter_* and sys_exit_* for read():
// the buffer pointer is only known at entry, the byte count only at exit.
struct read_args {
	__u64 buf;
	__u32 fd;
};

// ---------------------------------------------------------------------------
// Maps
// ---------------------------------------------------------------------------

// Set of PIDs (tgid) to trace. Written by the loader each supervisor cycle.
// An empty map means "trace nothing" — we never trace the whole host by
// accident, which keeps overhead bounded (§ self-healing watchdog).
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024);
	__type(key, __u32);   // tgid
	__type(value, __u8);  // 1 = enabled
} target_pids SEC(".maps");

// PID namespace the loader lives in, index 0: the inode of its
// /proc/self/ns/pid. 0 = the initial namespace, so the global tgid is used.
// Otherwise target_pids holds PIDs as seen in that namespace (that is what
// the supervisor reads from its /proc), and each task's tgid is translated
// to that namespace before the lookup. Without this, a supervisor running in
// a container (or anywhere under a nested PID namespace, e.g. Docker Desktop
// even with --pid=host) never matches a single target.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u32);
} pidns_ino SEC(".maps");

// 1-in-N sampling knob, index 0. Written by the loader when the watchdog
// throttles us. 0 or 1 = keep everything.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u32);
} sample_every SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);  // pid_tgid
	__type(value, struct read_args);
} active_reads SEC(".maps");

// SSL_read/SSL_read_ex arguments between uprobe and uretprobe. Separate from
// active_reads because SSL_read itself issues read() on the same thread.
// LRU so a missed return (thread killed mid-call) cannot leak entries forever.
struct ssl_read_args {
	__u64 ssl;
	__u64 buf;
	__u64 readbytes;  // size_t * for SSL_read_ex, 0 for SSL_read
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u64);  // pid_tgid
	__type(value, struct ssl_read_args);
} active_ssl_reads SEC(".maps");

// LRU so leaked entries (connection closed before responding) age out
// instead of filling the map.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, struct conn_key);
	__type(value, struct inflight_req);
} inflight SEC(".maps");

// Per-CPU scratch for the request head (too large for the 512-byte stack
// together with everything else).
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, char[HEAD_LEN]);
} scratch SEC(".maps");

// 1 MiB ring buffer of red_event. User space reads it with ringbuf.Reader.
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 20);
} events SEC(".maps");

// ---------------------------------------------------------------------------
// Tracepoint argument layouts (from /sys/kernel/tracing/events/syscalls/*/format)
// ---------------------------------------------------------------------------

struct trace_enter_rw {
	__u64 __unused_common;  // common_type/flags/preempt_count/pid
	__s32 __syscall_nr;
	__u32 __pad;
	__u64 fd;
	__u64 buf;
	__u64 count;
};

struct trace_exit {
	__u64 __unused_common;
	__s32 __syscall_nr;
	__u32 __pad;
	__s64 ret;
};

// ---------------------------------------------------------------------------
// Kernel structs for PID-namespace translation (CO-RE: only the named fields
// are used, offsets come from the running kernel's BTF).
// ---------------------------------------------------------------------------

struct ns_common {
	unsigned int inum;
} __attribute__((preserve_access_index));

struct pid_namespace {
	struct ns_common ns;
} __attribute__((preserve_access_index));

struct upid {
	int nr;
	struct pid_namespace *ns;
} __attribute__((preserve_access_index));

struct pid {
	unsigned int level;
	struct upid numbers[1];
} __attribute__((preserve_access_index));

struct task_struct {
	struct task_struct *group_leader;
	struct pid *thread_pid;
} __attribute__((preserve_access_index));

// Deepest PID-namespace level we walk (the kernel allows 32; containers in
// containers rarely go past 3).
#define MAX_PIDNS_LEVEL 8

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// tgid of the current task as seen from the PID namespace with inode ino.
// Returns 0 if the task is not visible there (outside that namespace subtree).
static __always_inline int ns_tgid(__u32 ino, __u32 *out)
{
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	struct task_struct *leader = BPF_CORE_READ(task, group_leader);
	struct pid *p = BPF_CORE_READ(leader, thread_pid);
	if (!p)
		return 0;
	unsigned int level = BPF_CORE_READ(p, level);
	__u64 base = (__u64)p + bpf_core_field_offset(struct pid, numbers);
	__u64 usz = bpf_core_type_size(struct upid);
	__u64 nr_off = bpf_core_field_offset(struct upid, nr);
	__u64 ns_off = bpf_core_field_offset(struct upid, ns);
	// Bounded loop (kernel 5.3+), not unrolled: the body has early exits.
	for (int i = 0; i < MAX_PIDNS_LEVEL; i++) {
		if (i > level)
			break;
		struct pid_namespace *ns = 0;
		if (bpf_probe_read_kernel(&ns, sizeof(ns), (void *)(base + i * usz + ns_off)) < 0 || !ns)
			return 0;
		if (BPF_CORE_READ(ns, ns.inum) != ino)
			continue;
		int nr = 0;
		if (bpf_probe_read_kernel(&nr, sizeof(nr), (void *)(base + i * usz + nr_off)) < 0)
			return 0;
		*out = nr;
		return 1;
	}
	return 0;
}

// If the current process is a target, stores its tgid as user space knows it
// (see pidns_ino) and returns 1.
static __always_inline int target_tgid(__u64 pid_tgid, __u32 *tgid)
{
	__u32 zero = 0;
	__u32 *ino = bpf_map_lookup_elem(&pidns_ino, &zero);
	*tgid = pid_tgid >> 32;
	if (ino && *ino && !ns_tgid(*ino, tgid))
		return 0;
	return bpf_map_lookup_elem(&target_pids, tgid) != 0;
}

// Returns the method length (incl. trailing space) if buf starts with a
// known HTTP method, else 0. Unrolled byte compares keep the verifier happy.
static __always_inline int http_method_len(const char *b)
{
	if (b[0] == 'G' && b[1] == 'E' && b[2] == 'T' && b[3] == ' ')
		return 4;
	if (b[0] == 'P' && b[1] == 'U' && b[2] == 'T' && b[3] == ' ')
		return 4;
	if (b[0] == 'P' && b[1] == 'O' && b[2] == 'S' && b[3] == 'T' && b[4] == ' ')
		return 5;
	if (b[0] == 'H' && b[1] == 'E' && b[2] == 'A' && b[3] == 'D' && b[4] == ' ')
		return 5;
	if (b[0] == 'P' && b[1] == 'A' && b[2] == 'T' && b[3] == 'C' && b[4] == 'H' && b[5] == ' ')
		return 6;
	if (b[0] == 'D' && b[1] == 'E' && b[2] == 'L' && b[3] == 'E' && b[4] == 'T' && b[5] == 'E' && b[6] == ' ')
		return 7;
	if (b[0] == 'O' && b[1] == 'P' && b[2] == 'T' && b[3] == 'I' && b[4] == 'O' && b[5] == 'N' && b[6] == 'S' && b[7] == ' ')
		return 8;
	return 0;
}

// Sample decision: keep 1 in N responses (N from sample_every[0]).
static __always_inline int sampled_out(__u64 ts)
{
	__u32 k = 0;
	__u32 *n = bpf_map_lookup_elem(&sample_every, &k);
	if (!n || *n <= 1)
		return 0;
	return (ts % *n) != 0;
}

// Called with a successfully read plaintext buffer (read exit or SSL_read
// return): if it starts an HTTP request, record it as in flight under key.
static __always_inline void on_inbound(const struct conn_key *key, const void *ubuf, __s64 n)
{
	if (n < 16)  // shortest real request line: "GET / HTTP/1.1\r\n"
		return;
	__u32 zero = 0;
	char *head = bpf_map_lookup_elem(&scratch, &zero);
	if (!head)
		return;
	if (bpf_probe_read_user(head, HEAD_LEN, ubuf) < 0)
		return;
	int mlen = http_method_len(head);
	if (mlen == 0)
		return;

	struct inflight_req req = {};
	req.start_ns = bpf_ktime_get_ns();
	// Method without the trailing space. mlen <= 8 so this is bounded.
#pragma unroll
	for (int i = 0; i < METHOD_LEN; i++) {
		if (i < mlen - 1)
			req.method[i] = head[i];
	}
	// Path: from after the method to the next space / '?' / CR, bounded by PATH_LEN-1.
#pragma unroll
	for (int i = 0; i < PATH_LEN - 1; i++) {
		int j = mlen + i;
		if (j >= HEAD_LEN)
			break;
		char c = head[j];
		if (c == ' ' || c == '?' || c == '\r' || c == '\n' || c == 0)
			break;
		req.path[i] = c;
	}
	bpf_map_update_elem(&inflight, key, &req, BPF_ANY);
}

// Called on write entry (write/sendto or SSL_write*): if key has an
// in-flight request and the buffer is an HTTP status line, emit the
// completed event. tgid is the PID as user space knows it.
static __always_inline void on_outbound(const struct conn_key *key, __u32 tgid, const void *ubuf, __u64 count)
{
	if (count < 12)  // "HTTP/1.1 200"
		return;
	struct inflight_req *req = bpf_map_lookup_elem(&inflight, key);
	if (!req)
		return;
	char sl[12];
	if (bpf_probe_read_user(sl, sizeof(sl), ubuf) < 0)
		return;
	if (!(sl[0] == 'H' && sl[1] == 'T' && sl[2] == 'T' && sl[3] == 'P' && sl[4] == '/' && sl[5] == '1'))
		return;

	__u64 now = bpf_ktime_get_ns();
	if (sampled_out(now)) {
		bpf_map_delete_elem(&inflight, key);
		return;
	}
	struct red_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)  // ring full: user space is behind; drop rather than block the app
		return;
	e->ts_ns = now;
	e->latency_ns = now - req->start_ns;
	e->pid = tgid;  // as user space knows it, see target_tgid()
	e->flags = key->ssl ? RED_F_TLS : 0;
	__u16 status = 0;
	// "HTTP/1.x NNN": digits at offsets 9..11.
	if (sl[9] >= '0' && sl[9] <= '9' && sl[10] >= '0' && sl[10] <= '9' && sl[11] >= '0' && sl[11] <= '9')
		status = (sl[9] - '0') * 100 + (sl[10] - '0') * 10 + (sl[11] - '0');
	e->status = status;
	__builtin_memcpy(e->method, req->method, METHOD_LEN);
	__builtin_memcpy(e->path, req->path, PATH_LEN);
	bpf_get_current_comm(e->comm, COMM_LEN);
	bpf_ringbuf_submit(e, 0);
	bpf_map_delete_elem(&inflight, key);
}

// ---------------------------------------------------------------------------
// Programs: inbound side (read / recvfrom). Buffer pointer at entry, length at exit.
// ---------------------------------------------------------------------------

static __always_inline int enter_read_common(struct trace_enter_rw *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid;
	if (!target_tgid(id, &tgid))
		return 0;
	struct read_args a = {.buf = ctx->buf, .fd = (__u32)ctx->fd};
	bpf_map_update_elem(&active_reads, &id, &a, BPF_ANY);
	return 0;
}

static __always_inline int exit_read_common(struct trace_exit *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct read_args *a = bpf_map_lookup_elem(&active_reads, &id);
	if (!a)
		return 0;
	if (ctx->ret > 0) {
		struct conn_key key = {.tgid = id >> 32, .fd = a->fd, .ssl = 0};
		on_inbound(&key, (const void *)a->buf, ctx->ret);
	}
	bpf_map_delete_elem(&active_reads, &id);
	return 0;
}

SEC("tracepoint/syscalls/sys_enter_read")
int tp_enter_read(struct trace_enter_rw *ctx) { return enter_read_common(ctx); }

SEC("tracepoint/syscalls/sys_exit_read")
int tp_exit_read(struct trace_exit *ctx) { return exit_read_common(ctx); }

SEC("tracepoint/syscalls/sys_enter_recvfrom")
int tp_enter_recvfrom(struct trace_enter_rw *ctx) { return enter_read_common(ctx); }

SEC("tracepoint/syscalls/sys_exit_recvfrom")
int tp_exit_recvfrom(struct trace_exit *ctx) { return exit_read_common(ctx); }

// ---------------------------------------------------------------------------
// Programs: outbound side (write / sendto). Everything is known at entry.
// ---------------------------------------------------------------------------

static __always_inline int enter_write_common(struct trace_enter_rw *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid;
	if (!target_tgid(id, &tgid))
		return 0;
	struct conn_key key = {.tgid = id >> 32, .fd = (__u32)ctx->fd, .ssl = 0};
	on_outbound(&key, tgid, (const void *)ctx->buf, ctx->count);
	return 0;
}

SEC("tracepoint/syscalls/sys_enter_write")
int tp_enter_write(struct trace_enter_rw *ctx) { return enter_write_common(ctx); }

SEC("tracepoint/syscalls/sys_enter_sendto")
int tp_enter_sendto(struct trace_enter_rw *ctx) { return enter_write_common(ctx); }

// ---------------------------------------------------------------------------
// Programs: TLS plaintext via OpenSSL uprobes. Attached per target process by
// the loader (tls_linux_ebpf.go) to the libssl the process maps.
//
//   int SSL_read(SSL *s, void *buf, int num)                        -> bytes
//   int SSL_read_ex(SSL *s, void *buf, size_t num, size_t *readbytes) -> 1 ok
//   int SSL_write(SSL *s, const void *buf, int num)
//   int SSL_write_ex(SSL *s, const void *buf, size_t num, size_t *written)
//
// In OpenSSL 1.1.1 and 3.x the plain and _ex variants both call
// ssl_read_internal/ssl_write_internal and never each other, so hooking all
// four does not double count.
// ---------------------------------------------------------------------------

// x86-64 user registers as saved in the kernel's struct pt_regs (the uprobe
// context). Only the System V argument and return registers are used; their
// offsets are part of the kernel/user ABI. Declared here because the uapi
// <asm/ptrace.h> picks the i386 layout under -target bpf.
struct x86_regs {
	unsigned long r15, r14, r13, r12, rbp, rbx, r11, r10, r9, r8;
	unsigned long rax, rcx, rdx, rsi, rdi;
};
#define UARG1(c) (((struct x86_regs *)(c))->rdi)
#define UARG2(c) (((struct x86_regs *)(c))->rsi)
#define UARG3(c) (((struct x86_regs *)(c))->rdx)
#define UARG4(c) (((struct x86_regs *)(c))->rcx)
#define URET(c) (((struct x86_regs *)(c))->rax)

static __always_inline int ssl_read_enter(void *ctx, int ex)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid;
	if (!target_tgid(id, &tgid))
		return 0;
	struct ssl_read_args a = {.ssl = UARG1(ctx), .buf = UARG2(ctx), .readbytes = ex ? UARG4(ctx) : 0};
	bpf_map_update_elem(&active_ssl_reads, &id, &a, BPF_ANY);
	return 0;
}

static __always_inline int ssl_read_return(void *ctx, int ex)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct ssl_read_args *ap = bpf_map_lookup_elem(&active_ssl_reads, &id);
	if (!ap)
		return 0;
	struct ssl_read_args a = *ap;
	bpf_map_delete_elem(&active_ssl_reads, &id);
	int rc = (int)URET(ctx);
	__s64 n = 0;
	if (!ex) {
		n = rc;
	} else if (rc == 1 && a.readbytes) {
		__u64 got = 0;
		if (bpf_probe_read_user(&got, sizeof(got), (const void *)a.readbytes) < 0)
			return 0;
		n = (__s64)got;
	}
	if (n > 0) {
		struct conn_key key = {.tgid = id >> 32, .fd = 0, .ssl = a.ssl};
		on_inbound(&key, (const void *)a.buf, n);
	}
	return 0;
}

static __always_inline int ssl_write_enter(void *ctx, int ex)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid;
	if (!target_tgid(id, &tgid))
		return 0;
	__s64 n = ex ? (__s64)UARG3(ctx) : (__s64)(int)UARG3(ctx);
	if (n <= 0)
		return 0;
	struct conn_key key = {.tgid = id >> 32, .fd = 0, .ssl = UARG1(ctx)};
	on_outbound(&key, tgid, (const void *)UARG2(ctx), (__u64)n);
	return 0;
}

SEC("uprobe/SSL_read")
int up_ssl_read(void *ctx) { return ssl_read_enter(ctx, 0); }

SEC("uretprobe/SSL_read")
int uret_ssl_read(void *ctx) { return ssl_read_return(ctx, 0); }

SEC("uprobe/SSL_read_ex")
int up_ssl_read_ex(void *ctx) { return ssl_read_enter(ctx, 1); }

SEC("uretprobe/SSL_read_ex")
int uret_ssl_read_ex(void *ctx) { return ssl_read_return(ctx, 1); }

SEC("uprobe/SSL_write")
int up_ssl_write(void *ctx) { return ssl_write_enter(ctx, 0); }

SEC("uprobe/SSL_write_ex")
int up_ssl_write_ex(void *ctx) { return ssl_write_enter(ctx, 1); }
