package probe

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/procfs"
)

// ProcProbe is the portable fallback. Without kernel hooks it cannot see
// individual HTTP requests, so it derives RED *proxies* per listening port
// from /proc/net/tcp{,6}:
//
//	ConnectionsOpened  new ESTABLISHED 4-tuples since the last sample (≈ rate for
//	                   short-lived / non-keepalive clients)
//	ActiveConnections  concurrent ESTABLISHED sockets (≈ in-flight load)
//	SocketBacklog      sum of rx+tx queue bytes (≈ saturation; growing backlog
//	                   precedes latency spikes)
//
// It is honest about being a proxy: metrics land in ZeroWatch/Net, not RED.
type ProcProbe struct {
	Root     string
	Interval time.Duration
	Log      *slog.Logger

	mu      sync.Mutex
	targets map[uint16]Target
	seen    map[string]struct{}
	status  Status
	rate    float64
}

// NewProcProbe returns a /proc-sampling probe.
func NewProcProbe(log *slog.Logger) *ProcProbe {
	return &ProcProbe{Root: procfs.DefaultRoot, Interval: 10 * time.Second, Log: log,
		targets: map[uint16]Target{}, seen: map[string]struct{}{}, rate: 1,
		status: Status{Name: "procfs-socket-sampler", Mode: "procfs", SampleRate: 1}}
}

// Name implements Probe.
func (p *ProcProbe) Name() string { return p.status.Name }

// SetTargets implements Probe.
func (p *ProcProbe) SetTargets(ts []Target) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = make(map[uint16]Target, len(ts))
	for _, t := range ts {
		if t.Port != 0 {
			p.targets[t.Port] = t
		}
	}
	p.status.Targets = len(p.targets)
}

// SetSampleRate implements Probe. For the sampler, a lower rate stretches
// the sampling interval (rate 0.5 => sample every other tick).
func (p *ProcProbe) SetSampleRate(r float64) {
	if r <= 0 || r > 1 {
		return
	}
	p.mu.Lock()
	p.rate, p.status.SampleRate = r, r
	p.mu.Unlock()
}

// Status implements Probe.
func (p *ProcProbe) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// Run implements Probe.
func (p *ProcProbe) Run(ctx context.Context, sink Sink) error {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			p.mu.Lock()
			skip := p.rate < 1 && rand.Float64() > p.rate
			p.mu.Unlock()
			if skip {
				continue
			}
			ms, err := p.Sample(now)
			p.mu.Lock()
			if err != nil {
				p.status.Healthy, p.status.Error = false, err.Error()
			} else {
				p.status.Healthy, p.status.Error = true, ""
			}
			p.mu.Unlock()
			if len(ms) > 0 {
				sink(ctx, ms)
				p.mu.Lock()
				p.status.Emitted += uint64(len(ms))
				p.status.LastEmit = now
				p.mu.Unlock()
			}
		}
	}
}

// Sample reads the socket tables once and returns datums (exported for tests).
func (p *ProcProbe) Sample(now time.Time) ([]model.MetricDatum, error) {
	socks, err := procfs.ReadSockets(p.Root)
	if err != nil && len(socks) == 0 {
		return nil, fmt.Errorf("read sockets: %w", err)
	}
	return p.fold(now, socks), nil
}

func (p *ProcProbe) fold(now time.Time, socks []procfs.Socket) []model.MetricDatum {
	p.mu.Lock()
	defer p.mu.Unlock()
	type acc struct{ active, opened, backlog float64 }
	per := map[uint16]*acc{}
	for port := range p.targets {
		per[port] = &acc{}
	}
	nextSeen := make(map[string]struct{}, len(p.seen))
	for _, s := range socks {
		a, ok := per[s.LocalPort]
		if !ok || s.State != procfs.TCPEstablished {
			continue
		}
		p.status.Events++
		a.active++
		a.backlog += float64(s.RxQueue + s.TxQueue)
		key := s.RemoteIP.String() + ":" + strconv.Itoa(int(s.RemotePort)) + ">" + strconv.Itoa(int(s.LocalPort))
		nextSeen[key] = struct{}{}
		if _, was := p.seen[key]; !was {
			a.opened++
		}
	}
	p.seen = nextSeen
	ts := now.UnixMilli()
	var out []model.MetricDatum
	for port, a := range per {
		t := p.targets[port]
		dims := []model.Dimension{{Name: "Port", Value: strconv.Itoa(int(port))}, {Name: "Service", Value: serviceName(t, "")}}
		out = append(out,
			model.MetricDatum{Namespace: NamespaceNet, MetricName: "ActiveConnections", Dimensions: dims, Timestamp: ts, Value: model.Float(a.active), Unit: "Count"},
			model.MetricDatum{Namespace: NamespaceNet, MetricName: "ConnectionsOpened", Dimensions: dims, Timestamp: ts, Value: model.Float(a.opened), Unit: "Count"},
			model.MetricDatum{Namespace: NamespaceNet, MetricName: "SocketBacklog", Dimensions: dims, Timestamp: ts, Value: model.Float(a.backlog), Unit: "Bytes"},
		)
	}
	return out
}
