# ZeroWatch eBPF probe (Tier 1: zero-code RED)

| File | Build | Purpose |
|---|---|---|
| `red_probe.bpf.c` | clang → `red_probe.bpf.o` | Kernel side: syscall tracepoints and HTTP/1.x request/response pairing, emitting events to a ring buffer |
| `probe/probe.go` | always | `Probe` interface, route normalisation, RED aggregator |
| `probe/procprobe.go` | always | `/proc/net/tcp{,6}` fallback. It produces connection-level proxies (`ZeroWatch/Net`), not true RED |
| `probe/new_default.go` | `!(linux && ebpf)` | Default build ships only the fallback |
| `probe/loader_linux_ebpf.go` | `linux && ebpf` | cilium/ebpf loader: loads the `.o` at runtime, attaches the tracepoints, reads the ring buffer and folds events into `ZeroWatch/RED` Requests/Errors/Latency. If loading fails it falls back to `/proc` at runtime |
| `probe/tls_linux_ebpf.go` | `linux && ebpf` | Attaches the OpenSSL uprobes to each target that maps `libssl` and detaches them when the target goes away |
| `probe/tlslib.go` | always | Pure `/proc` parsing for the above (`libssl` in `maps`, start time in `stat`), unit tested in the default build |
| `probe/*_linux_ebpf_test.go` | `linux && ebpf` | Object spec, decode, in-kernel load (`TestKernelLoad`) and end-to-end RED tests, plaintext (`TestKernelREDEndToEnd`) and TLS (`TestKernelTLSEndToEnd`) |

## How it works

1. The supervisor discovers listening processes and calls `SetTargets`. The loader writes their PIDs into the `target_pids` BPF map. No PID means nothing is traced; the probe never traces the whole host.
2. PIDs are the ones the supervisor sees in its own `/proc`. If the loader is not in the initial PID namespace (any container, and Docker Desktop even with `--pid=host`), it writes the inode of `/proc/self/ns/pid` into `pidns_ino`, and the BPF side translates each task's tgid into that namespace before the lookup. It walks `task->group_leader->thread_pid->numbers[]` with CO-RE relocations against `/sys/kernel/btf/vmlinux`, up to 8 namespace levels. Processes outside the loader's namespace subtree are never targets.
3. `sys_enter/exit_read` and `recvfrom` capture the first 80 bytes of each inbound read. If they start with an HTTP method, `{start_ns, method, path}` is stored in the `inflight` LRU map, keyed by `(tgid, fd)`.
4. `sys_enter_write` and `sendto` on the same `(tgid, fd)` check for `HTTP/1.x NNN`. On a match, the probe parses the status, computes the latency and submits a 112-byte `struct red_event` to the `events` ring buffer.
5. User space aggregates the events every 10 s. Routes are normalised (`/users/42` becomes `/users/:id`) and capped at 2000 routes, with the excess going to `OTHER`. Only 5xx statuses count as Errors.
6. The watchdog's `SetSampleRate(r)` writes `1/r` into `sample_every`, so 1 response in N is kept.

### TLS (OpenSSL uprobes)

7. Every 5 s, and right after each `SetTargets`, the loader reads `/proc/<pid>/maps` of each target and looks for `libssl.so[.N…]` (OpenSSL 1.1 and 3.x). It opens the library through `/proc/<pid>/root/<path>`, so a target in a container resolves to its own file. If no `libssl` is mapped, the target's `/proc/<pid>/exe` is checked once for statically linked OpenSSL symbols. Targets without `libssl` are re-checked on every pass, which catches a late `dlopen` (CPython only loads it on `import ssl`).
8. Uprobes are attached per process (`UprobeOptions.PID`), so other processes that share the same `libssl` never trap: `SSL_read` and `SSL_read_ex` at entry and return, and `SSL_write` and `SSL_write_ex` at entry. Symbols the library lacks are skipped (1.1.0 has no `_ex`).
9. The plaintext buffers go through the same HTTP/1.x parser as the syscall path. The in-flight key is `(tgid, SSL*)` instead of `(tgid, fd)`. A request and its response always use the same `SSL` object, so no `SSL*`→fd mapping (`SSL_get_fd`/`rbio`) is needed. `SSL_read` keeps its own per-thread argument map, because OpenSSL calls `read()` inside `SSL_read` on the same thread. Events from this path carry `RED_F_TLS` (`REDEvent.TLS`). `Status.TLSTargets` and `Status.TLSEvents` count them. The syscall path sees only ciphertext on those sockets, so nothing is counted twice.
10. When a target leaves the set, exits or its PID is reused (detected by start time in `/proc/<pid>/stat`), its uprobes are closed on the next pass. On shutdown, every uprobe link is closed before the programs.

Known limits:
- HTTP/1.x only; HTTP/2 and gRPC are on the roadmap.
- TLS covers only **dynamically linked OpenSSL** through its `SSL_read*`/`SSL_write*` API. Not covered: **Go `crypto/tls`** (a Go HTTPS server is invisible; it would need uprobes on `crypto/tls.(*Conn).Read/Write` with Go's register ABI and goroutine-aware keys), **BoringSSL** (Envoy, Chromium; usually static and stripped), **GnuTLS, NSS, rustls, Java JSSE, Node's bundled static OpenSSL**, and kTLS offload or `SSL_sendfile`. The static-exe fallback works only if the binary keeps `SSL_*` in its symbol table; it is not tested.
- A `libssl` loaded *after* another one was already attached in the same process is not picked up (rare). Deleted-on-disk libraries (upgraded while running) cannot be attached by path.
- x86-64 only: the uprobe programs read System V argument registers from `pt_regs`.
- `readv`/`writev`/`sendmsg`/`recvmsg`/io_uring are not hooked.
- Pipelined requests on one connection overwrite each other.
- Attaching the tracepoints needs tracefs (`/sys/kernel/tracing` or `/sys/kernel/debug/tracing`) to resolve tracepoint IDs. Containers usually do not have it mounted; without it, attach fails and the loader falls back to `/proc`.

## Build

Requirements:
- Linux 5.8+ with `CONFIG_BPF_SYSCALL`, `CONFIG_BPF_EVENTS`, `CONFIG_FTRACE_SYSCALLS` and `CONFIG_DEBUG_INFO_BTF` (BTF is needed for the CO-RE PID-namespace walk).
- clang ≥ 11 and `libbpf-dev`, which provides `<bpf/bpf_helpers.h>` and `<bpf/bpf_core_read.h>`.

There is no Makefile target; build the object directly:

```bash
sudo apt-get install -y clang llvm libbpf-dev
clang -O2 -g -Wall -target bpf -D__TARGET_ARCH_x86       -I/usr/include/$(uname -m)-linux-gnu       -c ebpf/red_probe.bpf.c -o ebpf/red_probe.bpf.o
go build -tags ebpf -o bin/zerowatch ./cmd/zerowatch
sudo ./bin/zerowatch --role=all --bpf-object=ebpf/red_probe.bpf.o
# or grant capabilities instead of running as root:
sudo setcap cap_bpf,cap_perfmon,cap_sys_resource+ep bin/zerowatch
```

The compiled `red_probe.bpf.o` is committed and kept in sync with the source. It is not embedded: the `-tags ebpf` binary loads it at runtime from `--bpf-object` (default `ebpf/red_probe.bpf.o`, env `ZW_BPF_OBJECT`) with `ebpf.LoadCollectionSpec`, so no `bpf2go` code generation is needed. The default build (no tag) ships only the `/proc` fallback. Tracepoint argument structs are declared in the C file and kernel structs are CO-RE relocated, so `vmlinux.h` is not needed.

## Tests

```bash
go test ./ebpf/...                         # default build: normaliser, aggregator, /proc fold
go test -tags ebpf ./ebpf/...              # + object spec parse, event decode (kernel tests skip without root)
sudo -E go test -tags ebpf -run TestKernel ./ebpf/probe/   # verifier load + end-to-end RED (root, tracefs)
```

`ZW_BPF_OBJ` points the tests at another object (default `../red_probe.bpf.o`).

- `TestKernelLoad` loads every program and map into the kernel, so it passes only if the verifier accepts them.
- `TestKernelREDEndToEnd` starts two HTTP servers as child processes: a Go `net/http` server (re-exec of the test binary, uses `read`/`write`) and, if `python3` is on `PATH`, a `http.server` (uses `recvfrom`/`sendto`). It registers their PIDs as targets, runs the real probe (`Run`), and sends each server 20 × `GET /users/N` (200), 5 × `/missing` (404), 4 × `/boom` (500) and 3 × `/slow` (200 after 30 ms). It then asserts the emitted `ZeroWatch/RED` datums: 32 Requests per service, the right per-route counts, Errors = 4 (only 5xx), and `GET /slow` latency ≥ 30 ms. It skips without root or tracefs.
- `TestKernelTLSEndToEnd` starts a CPython `http.server` wrapped in `ssl` (`SSL_read_ex`/`SSL_write_ex`, HTTP/1.0, one connection per request) and, if installed, a single-process nginx with TLS (`SSL_read`/`SSL_write`, keep-alive, many requests per `SSL*`). Both use a self-signed certificate generated by the test. It waits until both have uprobes attached, then sends the same request mix over HTTPS with a Go client, plus 3 × `GET /curl/N` per server with `curl -k` if it is installed. It asserts exact per-route counts and Errors = 4 (and `/slow` ≥ 30 ms for Python). It also checks that `Events == TLSEvents ==` the number of requests, so every event came from the TLS path and none was double counted. Finally it checks that clearing the targets detaches the uprobes. Go TLS servers are not part of the test because `crypto/tls` does not use OpenSSL.

### Verified procedure (Docker Desktop on WSL2)

From Windows, in a privileged throwaway container (nothing on the host changes). In Git Bash, set `MSYS_NO_PATHCONV=1` first.

```bash
docker run --rm --privileged -v "D:\home\zerowatch:/src" -v zw-gomod:/go/pkg/mod   -v zw-gocache:/root/.cache/go-build -w /src golang:1.23 bash -c '
  apt-get update -qq && apt-get install -y -qq clang llvm libbpf-dev python3 nginx curl >/dev/null
  mount -t tracefs nodev /sys/kernel/tracing
  clang -O2 -g -Wall -target bpf -D__TARGET_ARCH_x86 -I/usr/include/$(uname -m)-linux-gnu         -c ebpf/red_probe.bpf.c -o ebpf/red_probe.bpf.o
  go test -tags ebpf -count=1 -v ./ebpf/...'
```

Result on 2026-10-16:
- Kernel `6.18.54.1-microsoft-standard-WSL2` (x86_64), BTF present. Config has `CONFIG_BPF_SYSCALL`, `CONFIG_BPF_JIT`, `CONFIG_BPF_EVENTS`, `CONFIG_FTRACE_SYSCALLS`, `CONFIG_DEBUG_INFO_BTF`, `CONFIG_KPROBE_EVENTS` and `CONFIG_UPROBE_EVENTS`.
- Toolchain: Debian clang 14.0.6, libbpf-dev 1.1.2, Go 1.23.12, cilium/ebpf v0.16.0.
- The object compiles with no warnings under `-Wall`, and libbpf (`bpftool prog loadall`) loads all six programs too.
- `TestObjectSpec`, `TestKernelLoad`, `TestDecodeEvent` and `TestKernelREDEndToEnd` pass, both with `--pid=host` and in the container's own PID namespace. Both servers show 32 requests and 4 errors, with `/slow` at about 30.4 ms and the other routes at 0.03 to 0.4 ms.
- As a non-root user, both kernel tests skip.
- TLS, same day and setup, with Python 3.11, OpenSSL 3.0 (Debian `libssl3`), nginx 1.22 and curl from bookworm: `TestKernelTLSEndToEnd` passes, both in the container's own PID namespace and with `--pid=host`, and under `-race`. `e2e-python-tls` shows 35 requests (20 `/users/:id`, 5 `/missing`, 4 `/boom`, 3 `/slow` at about 30.4 ms, 3 `/curl/:id`). `e2e-nginx-tls` shows 32 requests (no `/slow`). Each has 4 errors; `events=67 tlsEvents=67 tlsTargets=2`. Each server got 6 uprobes on `/proc/<pid>/root/usr/lib/x86_64-linux-gnu/libssl.so.3`.

Not verified:
- A target in a *deeper* PID namespace than the loader, for example the supervisor on the host and the app in a container. The namespace walk is written for it, but no test covers it.
- Kernels other than this WSL2 6.18 kernel.
- TLS: OpenSSL 1.1.1, a statically linked OpenSSL, a TLS target in a different mount namespace from the loader (the path goes through `/proc/<pid>/root`, but the test has loader and servers in one container), and multi-threaded OpenSSL servers (uprobes filter by process, but only single-threaded servers were tested).

Next (not done):
- HTTP/2 cleartext (h2c) and gRPC: parse HEADERS frames and HPACK `:path`/`:status`, at least for static-table and literal entries, keyed by stream ID.
- `readv`/`writev`/`sendmsg`/`recvmsg` (first iovec) and io_uring.
- Go `crypto/tls` uprobes.
- The earlier build VM, whose kernel lacked `CONFIG_BPF_EVENTS` and rejected every tracing program with `EINVAL`. There the loader logs a warning and uses the `/proc` probe.
