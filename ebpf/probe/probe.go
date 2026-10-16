// Package probe provides ZeroWatch's Tier-1 zero-code RED instrumentation.
//
// Two implementations share the Probe interface:
//
//   - eBPF (file loader_linux_ebpf.go, build tags `linux && ebpf`): loads
//     red_probe.bpf.o, attaches syscall tracepoints and parses HTTP/1.x
//     request/response heads in the kernel, streaming one event per request
//     over a BPF ring buffer. Gives true Rate / Errors / Duration per route.
//   - /proc fallback (procprobe.go, used by the default build and whenever the
//     eBPF object cannot be loaded): samples /proc/net/tcp{,6} to derive
//     per-port connection rate, concurrency and socket backlog. No root, no
//     kernel requirements, works in any container.
//
// See ebpf/README.md for how to compile the BPF object and build with -tags ebpf.
package probe

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
)

// Namespaces emitted by probes.
const (
	NamespaceRED = "ZeroWatch/RED"
	NamespaceNet = "ZeroWatch/Net"
)

// Target is a process/port the supervisor asked the probe to watch.
type Target struct {
	PID     int    `json:"pid"`
	Port    uint16 `json:"port"`
	Service string `json:"service"`
}

// Sink receives batches of metrics produced by a probe.
type Sink func(ctx context.Context, ms []model.MetricDatum)

// Status is reported to the supervisor / UI.
type Status struct {
	Name     string    `json:"name"`
	Mode     string    `json:"mode"` // "ebpf" | "procfs"
	Healthy  bool      `json:"healthy"`
	Targets  int       `json:"targets"`
	Events   uint64    `json:"events"`
	Emitted  uint64    `json:"emitted"`
	LastEmit time.Time `json:"lastEmit"`
	Error    string    `json:"error,omitempty"`
	// SampleRate is the fraction of events kept (watchdog may lower it).
	SampleRate float64 `json:"sampleRate"`
	// TLSTargets is how many targets have OpenSSL uprobes attached.
	TLSTargets int `json:"tlsTargets,omitempty"`
	// TLSEvents counts the Events that came from the TLS uprobes.
	TLSEvents uint64 `json:"tlsEvents,omitempty"`
}

// Probe is a RED data source.
type Probe interface {
	Name() string
	// Run blocks until ctx is cancelled, delivering metrics to sink.
	Run(ctx context.Context, sink Sink) error
	// SetTargets replaces the watched set (called every supervisor cycle).
	SetTargets(ts []Target)
	// SetSampleRate lets the resource watchdog throttle the probe (0 < r <= 1).
	SetSampleRate(r float64)
	Status() Status
}

// REDEvent is one observed request/response pair. Its layout mirrors
// struct red_event in red_probe.bpf.c (see decodeEvent in the loader).
type REDEvent struct {
	TimestampNs uint64
	LatencyNs   uint64
	PID         uint32
	Status      uint16
	Method      string
	Path        string
	Comm        string
	// TLS is set when the plaintext came from the OpenSSL uprobes.
	TLS bool
}

var (
	reUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reHex  = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	reNum  = regexp.MustCompile(`^\d+$`)
)

// NormalizeRoute collapses high-cardinality path segments so per-route
// metrics stay bounded: /users/42/orders/9f..e1 -> /users/:id/orders/:id.
// Query strings are dropped and depth is capped at 6 segments.
func NormalizeRoute(method, path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		path = "/"
	}
	path = reUUID.ReplaceAllString(path, ":id")
	segs := strings.Split(path, "/")
	if len(segs) > 7 {
		segs = append(segs[:7], "*")
	}
	for i, s := range segs {
		if reNum.MatchString(s) || reHex.MatchString(s) {
			segs[i] = ":id"
		}
	}
	if method == "" {
		method = "UNKNOWN"
	}
	return method + " " + strings.Join(segs, "/")
}

type aggKey struct{ service, route string }

type aggVal struct {
	requests  float64
	errors    float64
	latencies []float64
}

// Aggregator folds per-request events into per-interval RED datums
// (Requests, Errors, Latency) — the same shape the synthetic generator and
// dashboards use, so auto-instrumented services light up existing views.
type Aggregator struct {
	mu         sync.Mutex
	cur        map[aggKey]*aggVal
	maxRoutes  int
	maxSamples int
	overflow   uint64
}

// NewAggregator bounds distinct (service, route) keys and raw latency samples per key.
func NewAggregator(maxRoutes, maxSamples int) *Aggregator {
	if maxRoutes <= 0 {
		maxRoutes = 2000
	}
	if maxSamples <= 0 {
		maxSamples = 200
	}
	return &Aggregator{cur: map[aggKey]*aggVal{}, maxRoutes: maxRoutes, maxSamples: maxSamples}
}

// Add records one event attributed to service.
func (a *Aggregator) Add(service string, e REDEvent) {
	k := aggKey{service, NormalizeRoute(e.Method, e.Path)}
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.cur[k]
	if !ok {
		if len(a.cur) >= a.maxRoutes {
			k.route = "OTHER"
			if v, ok = a.cur[k]; !ok {
				v = &aggVal{}
				a.cur[k] = v
			}
			a.overflow++
		} else {
			v = &aggVal{}
			a.cur[k] = v
		}
	}
	v.requests++
	if e.Status >= 500 {
		v.errors++
	}
	if len(v.latencies) < a.maxSamples {
		v.latencies = append(v.latencies, float64(e.LatencyNs)/1e6)
	}
}

// Flush returns datums for the interval ending at ts and resets state.
func (a *Aggregator) Flush(ts int64) []model.MetricDatum {
	a.mu.Lock()
	cur := a.cur
	a.cur = make(map[aggKey]*aggVal, len(cur))
	a.mu.Unlock()
	keys := make([]aggKey, 0, len(cur))
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].service != keys[j].service {
			return keys[i].service < keys[j].service
		}
		return keys[i].route < keys[j].route
	})
	var out []model.MetricDatum
	for _, k := range keys {
		v := cur[k]
		dims := []model.Dimension{{Name: "Route", Value: k.route}, {Name: "Service", Value: k.service}}
		out = append(out,
			model.MetricDatum{Namespace: NamespaceRED, MetricName: "Requests", Dimensions: dims, Timestamp: ts, Value: model.Float(v.requests), Unit: "Count"},
			model.MetricDatum{Namespace: NamespaceRED, MetricName: "Errors", Dimensions: dims, Timestamp: ts, Value: model.Float(v.errors), Unit: "Count"},
		)
		for i, l := range v.latencies {
			out = append(out, model.MetricDatum{Namespace: NamespaceRED, MetricName: "Latency", Dimensions: dims,
				Timestamp: ts - int64(len(v.latencies)-1-i), Value: model.Float(l), Unit: "Milliseconds"})
		}
	}
	return out
}

// serviceName picks a stable service label for a pid.
func serviceName(t Target, comm string) string {
	if t.Service != "" {
		return t.Service
	}
	if comm != "" {
		return comm
	}
	return "pid-" + strconv.Itoa(t.PID)
}
