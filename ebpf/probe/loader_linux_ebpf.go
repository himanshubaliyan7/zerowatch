//go:build linux && ebpf

package probe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"syscall"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// Available reports whether this binary was built with the eBPF loader.
const Available = true

// DefaultObjectPath is where `make ebpf` installs the compiled object.
const DefaultObjectPath = "ebpf/red_probe.bpf.o" // relative to the working directory

// eventSize is sizeof(struct red_event) in red_probe.bpf.c.
const eventSize = 8 + 8 + 4 + 2 + 2 + 8 + 64 + 16

// redFlagTLS is RED_F_TLS in red_event.flags.
const redFlagTLS = 1

// tracepoints maps BPF program names (C function names) to syscall tracepoints.
var tracepoints = map[string]string{
	"tp_enter_read":     "sys_enter_read",
	"tp_exit_read":      "sys_exit_read",
	"tp_enter_recvfrom": "sys_enter_recvfrom",
	"tp_exit_recvfrom":  "sys_exit_recvfrom",
	"tp_enter_write":    "sys_enter_write",
	"tp_enter_sendto":   "sys_enter_sendto",
}

// New returns the kernel probe when the object at objPath can be loaded,
// otherwise the /proc fallback. The decision is made lazily in Run so that a
// missing CAP_BPF degrades gracefully instead of failing startup.
func New(log *slog.Logger, objPath string) Probe {
	if objPath == "" {
		objPath = DefaultObjectPath
	}
	return &kernelProbe{objPath: objPath, log: log, agg: NewAggregator(2000, 200),
		fallback: NewProcProbe(log), targets: map[uint32]Target{}, rate: 1, flushEvery: 10 * time.Second,
		tlsRescan: 5 * time.Second, tlsNudge: make(chan struct{}, 1),
		status: Status{Name: "ebpf-http-red", Mode: "ebpf", SampleRate: 1}}
}

type kernelProbe struct {
	objPath  string
	log      *slog.Logger
	agg      *Aggregator
	fallback *ProcProbe
	// flushEvery is how often events are folded into RED datums.
	flushEvery time.Duration
	// tlsRescan is how often targets are re-checked for libssl (late dlopen,
	// exits, PID reuse); SetTargets also triggers a check via tlsNudge.
	tlsRescan time.Duration
	tlsNudge  chan struct{}

	mu        sync.Mutex
	targets   map[uint32]Target
	tlsPIDs   map[uint32]bool // targets with OpenSSL uprobes attached
	rate      float64
	status    Status
	coll      *cebpf.Collection
	useFallbk bool
}

func (k *kernelProbe) Name() string { return "ebpf-http-red" }

func (k *kernelProbe) SetTargets(ts []Target) {
	k.fallback.SetTargets(ts) // fallback always tracks ports too
	k.mu.Lock()
	defer k.mu.Unlock()
	next := make(map[uint32]Target, len(ts))
	for _, t := range ts {
		if t.PID > 0 {
			next[uint32(t.PID)] = t
		}
	}
	k.status.Targets = len(next)
	if k.coll != nil {
		m := k.coll.Maps["target_pids"]
		for pid := range k.targets {
			if _, keep := next[pid]; !keep {
				_ = m.Delete(pid)
			}
		}
		one := uint8(1)
		for pid := range next {
			if err := m.Put(pid, one); err != nil {
				k.log.Warn("ebpf target update failed", "pid", pid, "err", err)
			}
		}
	}
	k.targets = next
	select { // let the TLS attacher pick up new/removed processes now
	case k.tlsNudge <- struct{}{}:
	default:
	}
}

func (k *kernelProbe) SetSampleRate(r float64) {
	if r <= 0 || r > 1 {
		return
	}
	k.fallback.SetSampleRate(r)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rate, k.status.SampleRate = r, r
	if k.coll != nil {
		every := uint32(math.Round(1 / r))
		_ = k.coll.Maps["sample_every"].Put(uint32(0), every)
	}
}

func (k *kernelProbe) Status() Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.useFallbk {
		s := k.fallback.Status()
		s.Error = k.status.Error
		return s
	}
	return k.status
}

// Run loads and attaches the BPF programs, then pumps the ring buffer.
func (k *kernelProbe) Run(ctx context.Context, sink Sink) error {
	links, rd, err := k.load()
	if err != nil {
		k.log.Warn("eBPF probe unavailable, falling back to /proc sampler", "err", err)
		k.mu.Lock()
		k.useFallbk, k.status.Error = true, err.Error()
		k.mu.Unlock()
		return k.fallback.Run(ctx, sink)
	}
	// TLS uprobes are attached per process as targets come and go.
	var tlsDone sync.WaitGroup
	if tls := newTLSAttacher(k.log, k.coll); tls != nil {
		tlsDone.Add(1)
		go func() {
			defer tlsDone.Done()
			defer tls.closeAll()
			k.runTLS(ctx, tls)
		}()
	}
	defer func() {
		tlsDone.Wait() // uprobe links go before the programs they use
		for _, l := range links {
			_ = l.Close()
		}
		k.mu.Lock()
		k.coll.Close()
		k.coll = nil
		k.mu.Unlock()
	}()
	k.mu.Lock()
	k.status.Healthy = true
	k.mu.Unlock()

	// Closing the reader unblocks Read() on shutdown.
	go func() { <-ctx.Done(); _ = rd.Close() }()

	// Flusher: fold events into RED datums every flushEvery (10s).
	go func() {
		t := time.NewTicker(k.flushEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				ms := k.agg.Flush(now.UnixMilli())
				if len(ms) == 0 {
					continue
				}
				sink(ctx, ms)
				k.mu.Lock()
				k.status.Emitted += uint64(len(ms))
				k.status.LastEmit = now
				k.mu.Unlock()
			}
		}
	}()

	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			k.log.Debug("ringbuf read", "err", err)
			continue
		}
		ev, err := decodeEvent(rec.RawSample)
		if err != nil {
			continue
		}
		k.mu.Lock()
		t := k.targets[ev.PID]
		k.status.Events++
		if ev.TLS {
			k.status.TLSEvents++
		}
		k.mu.Unlock()
		k.agg.Add(serviceName(t, ev.Comm), ev)
	}
}

// runTLS keeps the OpenSSL uprobes in step with the target set until ctx ends.
func (k *kernelProbe) runTLS(ctx context.Context, a *tlsAttacher) {
	t := time.NewTicker(k.tlsRescan)
	defer t.Stop()
	for {
		k.mu.Lock()
		pids := make([]uint32, 0, len(k.targets))
		for pid := range k.targets {
			pids = append(pids, pid)
		}
		k.mu.Unlock()
		n := a.sync(pids)
		attached := make(map[uint32]bool, n)
		for _, pid := range pids {
			if a.attached(pid) {
				attached[pid] = true
			}
		}
		k.mu.Lock()
		k.tlsPIDs, k.status.TLSTargets = attached, n
		k.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-k.tlsNudge:
		}
	}
}

// tlsAttached reports whether pid has OpenSSL uprobes attached.
func (k *kernelProbe) tlsAttached(pid uint32) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tlsPIDs[pid]
}

func (k *kernelProbe) load() ([]link.Link, *ringbuf.Reader, error) {
	if _, err := os.Stat(k.objPath); err != nil {
		return nil, nil, fmt.Errorf("bpf object %s: %w (run `make ebpf`)", k.objPath, err)
	}
	// Kernels < 5.11 account BPF memory against RLIMIT_MEMLOCK.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, nil, fmt.Errorf("remove memlock: %w", err)
	}
	spec, err := cebpf.LoadCollectionSpec(k.objPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load spec: %w", err)
	}
	coll, err := cebpf.NewCollection(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("load collection (need CAP_BPF/CAP_PERFMON or root): %w", err)
	}
	var links []link.Link
	for prog, tp := range tracepoints {
		p := coll.Programs[prog]
		if p == nil {
			coll.Close()
			return nil, nil, fmt.Errorf("program %s missing from object", prog)
		}
		l, err := link.Tracepoint("syscalls", tp, p, nil)
		if err != nil {
			for _, l := range links {
				_ = l.Close()
			}
			coll.Close()
			return nil, nil, fmt.Errorf("attach %s: %w", tp, err)
		}
		links = append(links, l)
	}
	rd, err := ringbuf.NewReader(coll.Maps["events"])
	if err != nil {
		for _, l := range links {
			_ = l.Close()
		}
		coll.Close()
		return nil, nil, fmt.Errorf("ringbuf reader: %w", err)
	}
	// Target PIDs come from our own /proc, so the kernel side must translate
	// tgids into our PID namespace (see pidns_ino in red_probe.bpf.c).
	if ino := pidNamespaceIno(); ino != 0 {
		err := errors.New("object has no pidns_ino map (rebuild red_probe.bpf.o)")
		if m := coll.Maps["pidns_ino"]; m != nil {
			err = m.Put(uint32(0), ino)
		}
		if err != nil {
			for _, l := range links {
				_ = l.Close()
			}
			rd.Close()
			coll.Close()
			return nil, nil, fmt.Errorf("set pid namespace: %w", err)
		}
	}
	// Publish the collection, then push targets/sample rate configured before load.
	k.mu.Lock()
	k.coll = coll
	ts := make([]Target, 0, len(k.targets))
	for _, t := range k.targets {
		ts = append(ts, t)
	}
	rate := k.rate
	k.mu.Unlock()
	k.SetTargets(ts)
	k.SetSampleRate(rate)
	return links, rd, nil
}

// procPidInitIno is PROC_PID_INIT_INO, the inode of the initial PID namespace.
const procPidInitIno = 0xEFFFFFFC

// pidNamespaceIno returns the inode of this process's PID namespace, or 0
// when it is the initial namespace (where the global tgid already matches
// /proc) or cannot be determined.
func pidNamespaceIno() uint32 {
	fi, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == procPidInitIno || st.Ino > math.MaxUint32 {
		return 0
	}
	return uint32(st.Ino)
}

// decodeEvent parses struct red_event (little-endian, no padding surprises:
// see the layout comment in red_probe.bpf.c).
func decodeEvent(b []byte) (REDEvent, error) {
	if len(b) < eventSize {
		return REDEvent{}, fmt.Errorf("short event: %d bytes", len(b))
	}
	le := binary.LittleEndian
	cstr := func(x []byte) string {
		if i := bytes.IndexByte(x, 0); i >= 0 {
			x = x[:i]
		}
		return string(x)
	}
	return REDEvent{
		TimestampNs: le.Uint64(b[0:8]),
		LatencyNs:   le.Uint64(b[8:16]),
		PID:         le.Uint32(b[16:20]),
		Status:      le.Uint16(b[20:22]),
		TLS:         le.Uint16(b[22:24])&redFlagTLS != 0,
		Method:      cstr(b[24:32]),
		Path:        cstr(b[32:96]),
		Comm:        cstr(b[96:112]),
	}, nil
}
