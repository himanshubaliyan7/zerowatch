//go:build linux && ebpf

package probe

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// tlsUprobes maps OpenSSL symbols to the BPF programs attached at entry and,
// for reads, at return (see the TLS section of red_probe.bpf.c).
var tlsUprobes = []struct{ sym, entry, ret string }{
	{"SSL_read", "up_ssl_read", "uret_ssl_read"},
	{"SSL_read_ex", "up_ssl_read_ex", "uret_ssl_read_ex"},
	{"SSL_write", "up_ssl_write", ""},
	{"SSL_write_ex", "up_ssl_write_ex", ""},
}

// tlsProc is the uprobe state of one target process.
type tlsProc struct {
	start   string          // /proc/<pid>/stat starttime, to detect PID reuse
	tried   map[string]bool // ELF paths already attempted (attached or not)
	links   []link.Link
	exeDone bool // the main executable was checked for a static OpenSSL
}

func (p *tlsProc) close() {
	for _, l := range p.links {
		_ = l.Close()
	}
	p.links = nil
}

// tlsAttacher attaches the OpenSSL uprobes to target processes. Uprobes are
// per process (UprobeOptions.PID), so untargeted processes that share the
// same libssl never trap. It is driven by kernelProbe from a single
// goroutine and is not safe for concurrent use.
type tlsAttacher struct {
	log      *slog.Logger
	coll     *cebpf.Collection
	procRoot string
	procs    map[uint32]*tlsProc
}

// newTLSAttacher returns nil when the object predates the TLS programs.
func newTLSAttacher(log *slog.Logger, coll *cebpf.Collection) *tlsAttacher {
	for _, u := range tlsUprobes {
		for _, name := range []string{u.entry, u.ret} {
			if name != "" && coll.Programs[name] == nil {
				log.Warn("bpf object has no TLS uprobe programs, TLS not traced (rebuild red_probe.bpf.o)", "missing", name)
				return nil
			}
		}
	}
	return &tlsAttacher{log: log, coll: coll, procRoot: "/proc", procs: map[uint32]*tlsProc{}}
}

// sync attaches to every target that maps an OpenSSL libssl (or statically
// links one) and detaches from processes that exited, were replaced under the
// same PID or are no longer targets. Targets without libssl are re-checked
// on every call, so a library loaded later with dlopen is picked up. It
// returns the number of targets with uprobes attached.
func (a *tlsAttacher) sync(pids []uint32) int {
	want := make(map[uint32]bool, len(pids))
	for _, pid := range pids {
		want[pid] = true
	}
	for pid, p := range a.procs {
		if !want[pid] || a.startTime(pid) != p.start {
			p.close()
			delete(a.procs, pid)
		}
	}
	n := 0
	for pid := range want {
		p := a.procs[pid]
		if p == nil {
			st := a.startTime(pid)
			if st == "" {
				continue // gone, or not visible from our /proc
			}
			p = &tlsProc{start: st, tried: map[string]bool{}}
			a.procs[pid] = p
		}
		if len(p.links) == 0 {
			a.scan(pid, p)
		}
		if len(p.links) > 0 {
			n++
		}
	}
	return n
}

// scan looks for libssl in the target's mappings and attaches to each new
// one. Library paths are opened through /proc/<pid>/root so a target in
// another mount namespace (a container) resolves to its own file.
func (a *tlsAttacher) scan(pid uint32, p *tlsProc) {
	base := filepath.Join(a.procRoot, strconv.FormatUint(uint64(pid), 10))
	maps, err := os.ReadFile(filepath.Join(base, "maps"))
	if err != nil {
		return
	}
	var paths []string
	for _, lib := range findSSLLibs(maps) {
		paths = append(paths, filepath.Join(base, "root", lib))
	}
	if len(paths) == 0 && !p.exeDone {
		// No shared libssl: the binary may link OpenSSL statically. Checked
		// once per process because parsing a large ELF is not free.
		p.exeDone = true
		paths = append(paths, filepath.Join(base, "exe"))
	}
	for _, path := range paths {
		if p.tried[path] {
			continue
		}
		p.tried[path] = true
		ls, err := a.attach(pid, path)
		if err != nil {
			a.log.Debug("tls uprobes not attached", "pid", pid, "elf", path, "err", err)
			continue
		}
		a.log.Info("tls uprobes attached", "pid", pid, "elf", path, "probes", len(ls))
		p.links = append(p.links, ls...)
	}
}

// attach puts the uprobes for every OpenSSL symbol the ELF exports on pid.
// A read symbol is attached only as an entry+return pair.
func (a *tlsAttacher) attach(pid uint32, path string) ([]link.Link, error) {
	ex, err := link.OpenExecutable(path)
	if err != nil {
		return nil, err
	}
	opts := &link.UprobeOptions{PID: int(pid)}
	var ls []link.Link
	var errs []error
	for _, u := range tlsUprobes {
		l, err := ex.Uprobe(u.sym, a.coll.Programs[u.entry], opts)
		if err != nil {
			if !errors.Is(err, link.ErrNoSymbol) {
				errs = append(errs, fmt.Errorf("%s: %w", u.sym, err))
			}
			continue
		}
		if u.ret != "" {
			r, err := ex.Uretprobe(u.sym, a.coll.Programs[u.ret], opts)
			if err != nil {
				_ = l.Close()
				errs = append(errs, fmt.Errorf("%s return: %w", u.sym, err))
				continue
			}
			ls = append(ls, r)
		}
		ls = append(ls, l)
	}
	if len(ls) == 0 {
		if len(errs) == 0 {
			return nil, errors.New("no OpenSSL symbols")
		}
		return nil, errors.Join(errs...)
	}
	return ls, nil
}

func (a *tlsAttacher) startTime(pid uint32) string {
	b, err := os.ReadFile(filepath.Join(a.procRoot, strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		return ""
	}
	return procStartTime(b)
}

// attached reports whether pid currently has uprobes attached.
func (a *tlsAttacher) attached(pid uint32) bool {
	p := a.procs[pid]
	return p != nil && len(p.links) > 0
}

// closeAll detaches everything.
func (a *tlsAttacher) closeAll() {
	for pid, p := range a.procs {
		p.close()
		delete(a.procs, pid)
	}
}
