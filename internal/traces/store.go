package traces

import (
	"sort"
	"strings"
	"sync"
)

// Defaults for Config.
const (
	DefaultMaxSpans         = 200_000
	DefaultMaxSpansPerTrace = 1_000
	DefaultSearchLimit      = 50
	MaxSearchLimit          = 500
)

// Config bounds the store.
type Config struct {
	// MaxSpans caps spans held per tenant; the oldest traces (by first
	// arrival) are evicted whole once it is exceeded.
	MaxSpans int
	// MaxSpansPerTrace drops further spans of a runaway trace instead of
	// letting it evict everything else.
	MaxSpansPerTrace int
}

// TraceSummary is one row of a trace search.
type TraceSummary struct {
	TraceID     string   `json:"traceId"`
	RootService string   `json:"rootService"`
	RootName    string   `json:"rootName"`
	Start       float64  `json:"start"`
	DurationMs  float64  `json:"durationMs"`
	Spans       int      `json:"spans"`
	Errors      int      `json:"errors"`
	Services    []string `json:"services"`
}

// TraceDetail is a full trace, spans ordered by start time.
type TraceDetail struct {
	TraceID string `json:"traceId"`
	Spans   []Span `json:"spans"`
}

// Query filters Search. Zero values mean "no filter".
type Query struct {
	Service       string
	Name          string // case-insensitive substring of any span name (of Service, when set)
	MinDurationMs float64
	ErrorsOnly    bool
	Start, End    float64 // trace start in [Start, End], unix ms
	Limit         int
}

// Stats are store counters.
type Stats struct {
	Tenants  int    `json:"tenants"`
	Traces   int    `json:"traces"`
	Spans    int    `json:"spans"`
	Accepted uint64 `json:"accepted"`
	Dropped  uint64 `json:"dropped"`
	Evicted  uint64 `json:"evictedTraces"`
}

type trace struct {
	spans   []Span
	summary TraceSummary
	dirty   bool
}

type tenantStore struct {
	traces map[string]*trace
	fifo   []string // trace ids in first-arrival order; fifo[head:] are live
	head   int
	spans  int
}

// Store is a bounded in-memory trace store, safe for concurrent use.
type Store struct {
	cfg Config

	mu       sync.RWMutex
	tenants  map[uint32]*tenantStore
	accepted uint64
	dropped  uint64
	evicted  uint64
}

// NewStore returns an empty store; zero Config fields take defaults.
func NewStore(cfg Config) *Store {
	if cfg.MaxSpans <= 0 {
		cfg.MaxSpans = DefaultMaxSpans
	}
	if cfg.MaxSpansPerTrace <= 0 {
		cfg.MaxSpansPerTrace = DefaultMaxSpansPerTrace
	}
	if cfg.MaxSpansPerTrace > cfg.MaxSpans {
		cfg.MaxSpansPerTrace = cfg.MaxSpans
	}
	return &Store{cfg: cfg, tenants: map[uint32]*tenantStore{}}
}

// Add stores already-validated spans for tenant and returns how many were
// kept. Spans are grouped by trace id; a span id already present in its
// trace is ignored (exporter retries).
func (s *Store) Add(tenant uint32, spans []Span) int {
	if len(spans) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tenants[tenant]
	if ts == nil {
		ts = &tenantStore{traces: map[string]*trace{}}
		s.tenants[tenant] = ts
	}
	kept := 0
	for i := range spans {
		sp := spans[i]
		t := ts.traces[sp.TraceID]
		if t == nil {
			t = &trace{}
			ts.traces[sp.TraceID] = t
			ts.fifo = append(ts.fifo, sp.TraceID)
		}
		if len(t.spans) >= s.cfg.MaxSpansPerTrace || hasSpan(t.spans, sp.SpanID) {
			s.dropped++
			continue
		}
		t.spans = append(t.spans, sp)
		t.dirty = true
		ts.spans++
		kept++
	}
	s.accepted += uint64(kept)
	s.evictLocked(ts, spans[len(spans)-1].TraceID)
	return kept
}

// evictLocked drops the oldest traces until the tenant is within budget. The
// trace just written to is spared unless it alone is over budget, which
// MaxSpansPerTrace <= MaxSpans rules out.
func (s *Store) evictLocked(ts *tenantStore, keep string) {
	for ts.spans > s.cfg.MaxSpans && ts.head < len(ts.fifo) {
		id := ts.fifo[ts.head]
		ts.fifo[ts.head] = ""
		ts.head++
		if id == keep && ts.spans <= s.cfg.MaxSpans+len(ts.traces[id].spans) {
			// Re-queue the newest trace at the back rather than evicting it.
			ts.fifo = append(ts.fifo, id)
			continue
		}
		if t := ts.traces[id]; t != nil {
			ts.spans -= len(t.spans)
			delete(ts.traces, id)
			s.evicted++
		}
	}
	// Compact once the dead prefix dominates, keeping append amortised O(1).
	if ts.head > 1024 && ts.head*2 > len(ts.fifo) {
		ts.fifo = append([]string(nil), ts.fifo[ts.head:]...)
		ts.head = 0
	}
}

func hasSpan(spans []Span, id string) bool {
	for i := range spans {
		if spans[i].SpanID == id {
			return true
		}
	}
	return false
}

// Get returns a copy of one trace.
func (s *Store) Get(tenant uint32, traceID string) (TraceDetail, bool) {
	traceID = strings.ToLower(traceID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	ts := s.tenants[tenant]
	if ts == nil {
		return TraceDetail{}, false
	}
	t := ts.traces[traceID]
	if t == nil {
		return TraceDetail{}, false
	}
	out := append([]Span(nil), t.spans...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Start < out[b].Start })
	return TraceDetail{TraceID: traceID, Spans: out}, true
}

// Search returns matching trace summaries, newest first.
func (s *Store) Search(tenant uint32, q Query) []TraceSummary {
	if q.Limit <= 0 {
		q.Limit = DefaultSearchLimit
	}
	if q.Limit > MaxSearchLimit {
		q.Limit = MaxSearchLimit
	}
	name := strings.ToLower(q.Name)
	// Summaries are cached on the trace, so a search mutates under the write
	// lock; it is a linear scan bounded by MaxSpans.
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tenants[tenant]
	if ts == nil {
		return []TraceSummary{}
	}
	out := []TraceSummary{}
	for id, t := range ts.traces {
		sum := t.summarize(id)
		if q.ErrorsOnly && sum.Errors == 0 ||
			sum.DurationMs < q.MinDurationMs ||
			q.Start > 0 && sum.Start < q.Start ||
			q.End > 0 && sum.Start > q.End ||
			!t.matches(q.Service, name) {
			continue
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Start != out[b].Start {
			return out[a].Start > out[b].Start
		}
		return out[a].TraceID < out[b].TraceID
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	for i := range out {
		out[i].Services = append([]string(nil), out[i].Services...)
	}
	return out
}

func (t *trace) matches(service, name string) bool {
	if service == "" && name == "" {
		return true
	}
	for i := range t.spans {
		sp := &t.spans[i]
		if service != "" && sp.Service != service {
			continue
		}
		if name == "" || strings.Contains(strings.ToLower(sp.Name), name) {
			return true
		}
	}
	return false
}

// summarize recomputes the cached summary when spans changed. The root is
// the earliest span without a parent in this trace (spans may arrive before
// their parent, or the parent may never arrive).
func (t *trace) summarize(id string) TraceSummary {
	if !t.dirty {
		return t.summary
	}
	ids := make(map[string]struct{}, len(t.spans))
	for i := range t.spans {
		ids[t.spans[i].SpanID] = struct{}{}
	}
	sum := TraceSummary{TraceID: id, Spans: len(t.spans)}
	var root *Span
	var end float64
	svcs := map[string]struct{}{}
	for i := range t.spans {
		sp := &t.spans[i]
		if i == 0 || sp.Start < sum.Start {
			sum.Start = sp.Start
		}
		if e := sp.End(); e > end {
			end = e
		}
		if sp.IsError() {
			sum.Errors++
		}
		svcs[sp.Service] = struct{}{}
		_, hasParent := ids[sp.ParentSpanID]
		isRoot := sp.ParentSpanID == "" || !hasParent
		if isRoot && (root == nil || sp.Start < root.Start || root.ParentSpanID != "" && sp.ParentSpanID == "") {
			root = sp
		}
	}
	if root != nil {
		sum.RootService, sum.RootName = root.Service, root.Name
	}
	sum.DurationMs = end - sum.Start
	sum.Services = make([]string, 0, len(svcs))
	for s := range svcs {
		sum.Services = append(sum.Services, s)
	}
	sort.Strings(sum.Services)
	t.summary, t.dirty = sum, false
	return sum
}

// Stats returns counters and current sizes.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{Tenants: len(s.tenants), Accepted: s.accepted, Dropped: s.dropped, Evicted: s.evicted}
	for _, ts := range s.tenants {
		st.Traces += len(ts.traces)
		st.Spans += ts.spans
	}
	return st
}

// Backend is what the API, the OTLP receiver, the generator and reports use:
// the in-process Store, or the cluster's router that keeps each trace on its
// ring owner (internal/cluster TraceRouter).
type Backend interface {
	Add(tenant uint32, spans []Span) int
	Get(tenant uint32, traceID string) (TraceDetail, bool)
	Search(tenant uint32, q Query) []TraceSummary
	Stats() Stats
}

var _ Backend = (*Store)(nil)
