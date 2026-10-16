package traces

import (
	"fmt"
	"testing"
)

func tid(n int) string { return fmt.Sprintf("%032x", n+1) }
func sid(n int) string { return fmt.Sprintf("%016x", n+1) }

func mkSpan(trace, span, parent int, service, name, kind string, start, dur float64, status string) Span {
	s := Span{TraceID: tid(trace), SpanID: sid(span), Service: service, Name: name, Kind: kind, Start: start, DurationMs: dur, Status: status}
	if parent >= 0 {
		s.ParentSpanID = sid(parent)
	}
	return s
}

func TestSpanNormalizeValidate(t *testing.T) {
	s := Span{TraceID: "0AF7651916CD43DD8448EB211C80319C", SpanID: "B7AD6B7169203331", ParentSpanID: "0000000000000000", Name: "x", Start: 1, Kind: "weird", Status: "?"}
	s.Normalize()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.TraceID != "0af7651916cd43dd8448eb211c80319c" || s.ParentSpanID != "" || s.Kind != KindInternal || s.Status != StatusUnset || s.Service != "unknown_service" {
		t.Fatalf("bad normalize: %+v", s)
	}
	for _, bad := range []Span{
		{TraceID: "00000000000000000000000000000000", SpanID: sid(0), Name: "x", Start: 1},
		{TraceID: tid(0), SpanID: "short", Name: "x", Start: 1},
		{TraceID: tid(0), SpanID: sid(0), Name: "x"},
		{TraceID: tid(0), SpanID: sid(0), Start: 1},
	} {
		if bad.Validate() == nil {
			t.Fatalf("expected invalid: %+v", bad)
		}
	}
}

func TestStoreEvictsOldestTraces(t *testing.T) {
	st := NewStore(Config{MaxSpans: 10, MaxSpansPerTrace: 5})
	for tr := 0; tr < 5; tr++ { // 5 traces × 3 spans = 15 > 10
		var spans []Span
		for i := 0; i < 3; i++ {
			spans = append(spans, mkSpan(tr, tr*10+i, -1, "a", "op", KindServer, float64(1000+tr), 1, StatusOK))
		}
		st.Add(1, spans)
	}
	stats := st.Stats()
	if stats.Spans > 10 || stats.Traces != 3 || stats.Evicted != 2 {
		t.Fatalf("unexpected stats %+v", stats)
	}
	for tr := 0; tr < 5; tr++ {
		_, ok := st.Get(1, tid(tr))
		if ok != (tr >= 2) {
			t.Fatalf("trace %d present=%v", tr, ok)
		}
	}
	// Per-trace cap and duplicate span ids.
	var big []Span
	for i := 0; i < 8; i++ {
		big = append(big, mkSpan(99, 500+i, -1, "a", "op", KindInternal, 5000, 1, StatusOK))
	}
	big = append(big, big[0])
	if kept := st.Add(1, big); kept != 5 {
		t.Fatalf("per-trace cap: kept %d", kept)
	}
	if st.Stats().Spans > 10 {
		t.Fatalf("over budget: %+v", st.Stats())
	}
}

func TestStoreEvictionIsAmortised(t *testing.T) {
	st := NewStore(Config{MaxSpans: 100})
	for tr := 0; tr < 20000; tr++ {
		st.Add(1, []Span{mkSpan(tr, tr, -1, "a", "op", KindServer, float64(tr), 1, StatusOK)})
	}
	ts := st.tenants[1]
	if st.Stats().Spans != 100 || len(ts.fifo)-ts.head != 100 || len(ts.fifo) > 3000 {
		t.Fatalf("fifo not compacted: len=%d head=%d", len(ts.fifo), ts.head)
	}
}

func TestStoreSearchAndGet(t *testing.T) {
	st := NewStore(Config{})
	// trace 0: gateway → payments, error, 100ms, start 1000
	st.Add(1, []Span{
		mkSpan(0, 1, 2, "payments", "POST /charge", KindServer, 1010, 50, StatusError), // child arrives first
		mkSpan(0, 2, -1, "gateway", "GET /checkout", KindServer, 1000, 100, StatusOK),
	})
	// trace 1: catalog only, 10ms, start 2000
	st.Add(1, []Span{mkSpan(1, 3, -1, "catalog", "GET /items", KindServer, 2000, 10, StatusOK)})
	// another tenant
	st.Add(2, []Span{mkSpan(2, 4, -1, "secret", "GET /x", KindServer, 3000, 5, StatusOK)})

	all := st.Search(1, Query{})
	if len(all) != 2 || all[0].TraceID != tid(1) || all[1].TraceID != tid(0) {
		t.Fatalf("want newest first, got %+v", all)
	}
	s0 := all[1]
	if s0.RootService != "gateway" || s0.RootName != "GET /checkout" || s0.DurationMs != 100 || s0.Spans != 2 || s0.Errors != 1 ||
		len(s0.Services) != 2 || s0.Services[0] != "gateway" {
		t.Fatalf("bad summary %+v", s0)
	}
	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"service any span", Query{Service: "payments"}, []string{tid(0)}},
		{"name substring", Query{Name: "items"}, []string{tid(1)}},
		{"name scoped to service", Query{Service: "gateway", Name: "charge"}, nil},
		{"min duration", Query{MinDurationMs: 50}, []string{tid(0)}},
		{"errors only", Query{ErrorsOnly: true}, []string{tid(0)}},
		{"time window", Query{Start: 1500, End: 2500}, []string{tid(1)}},
		{"limit", Query{Limit: 1}, []string{tid(1)}},
	}
	for _, c := range cases {
		got := st.Search(1, c.q)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %+v", c.name, got)
		}
		for i := range got {
			if got[i].TraceID != c.want[i] {
				t.Fatalf("%s: got %+v", c.name, got)
			}
		}
	}
	// Tenant isolation.
	if got := st.Search(2, Query{}); len(got) != 1 || got[0].RootService != "secret" {
		t.Fatalf("tenant 2: %+v", got)
	}
	if _, ok := st.Get(1, tid(2)); ok {
		t.Fatal("tenant 1 can read tenant 2's trace")
	}
	if got := st.Search(3, Query{}); got == nil || len(got) != 0 {
		t.Fatalf("unknown tenant should give empty non-nil slice, got %#v", got)
	}
	d, ok := st.Get(1, tid(0))
	if !ok || len(d.Spans) != 2 || d.Spans[0].Service != "gateway" {
		t.Fatalf("get should sort by start: %+v", d)
	}
	// Summary cache invalidates on late spans.
	st.Add(1, []Span{mkSpan(0, 5, 2, "auth", "verify", KindClient, 1001, 300, StatusOK)})
	if s := st.Search(1, Query{Service: "auth"}); len(s) != 1 || s[0].DurationMs != 301 || s[0].Spans != 3 {
		t.Fatalf("stale summary %+v", s)
	}
}

func TestREDMetricsFromServerSpans(t *testing.T) {
	spans := []Span{
		mkSpan(0, 1, -1, "api", "GET /a", KindServer, 1_700_000_000_100, 10, StatusOK),
		mkSpan(0, 2, -1, "api", "GET /a", KindServer, 1_700_000_000_900, 30, StatusError),
		mkSpan(0, 3, 1, "api", "SELECT", KindClient, 1_700_000_000_150, 5, StatusOK),
		mkSpan(1, 4, -1, "api", "GET /a", KindServer, 1_700_000_001_000, 20, StatusOK),
	}
	syn := mkSpan(2, 5, -1, "api", "GET /a", KindServer, 1_700_000_000_000, 1, StatusOK)
	syn.Attributes = map[string]string{SyntheticAttr: "true"}
	spans = append(spans, syn)

	ms := REDMetrics(spans)
	// two seconds × (Requests, Errors) + 3 latency samples
	if len(ms) != 7 {
		t.Fatalf("want 7 datums, got %d: %+v", len(ms), ms)
	}
	counts := map[string]float64{}
	for _, m := range ms {
		if m.Namespace != REDNamespace || len(m.Dimensions) != 2 || m.Dimensions[0].Name != "Route" || m.Dimensions[1].Value != "api" {
			t.Fatalf("bad datum %+v", m)
		}
		if m.MetricName != "Latency" {
			counts[fmt.Sprintf("%s@%d", m.MetricName, m.Timestamp)] = *m.Value
		}
	}
	want := map[string]float64{
		"Requests@1700000000000": 2, "Errors@1700000000000": 1,
		"Requests@1700000001000": 1, "Errors@1700000001000": 0,
	}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("%s = %v, want %v (all %v)", k, counts[k], v, counts)
		}
	}
}
