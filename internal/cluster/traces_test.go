package cluster

import (
	"fmt"
	"testing"

	"github.com/zerowatch/zerowatch/internal/traces"
)

func span(trace, id int, service string, start float64) traces.Span {
	return traces.Span{TraceID: fmt.Sprintf("%032x", trace), SpanID: fmt.Sprintf("%016x", id), Service: service, Name: "GET /",
		Kind: traces.KindServer, Start: start, DurationMs: 5, Status: traces.StatusOK}
}

// Every span of a trace lands on the trace's ring owner, and any node can
// read every trace back.
func TestTraceRouter(t *testing.T) {
	tc := newTestCluster(t, "", nil)
	r := &TraceRouter{e: tc.eng}
	var spans []traces.Span
	for tr := 1; tr <= 60; tr++ {
		for i := 0; i < 3; i++ {
			spans = append(spans, span(tr, tr*10+i, fmt.Sprintf("svc%d", i), float64(1000*tr+i)))
		}
	}
	if kept := r.Add(1, spans); kept != len(spans) {
		t.Fatalf("kept %d of %d", kept, len(spans))
	}
	holding := 0
	for name, st := range tc.traces {
		if n := st.Stats().Traces; n > 0 {
			holding++
			for _, s := range st.Search(1, traces.Query{Limit: traces.MaxSearchLimit}) {
				owner, _ := tc.mem.Ring().Get(traceKey(1, s.TraceID))
				if owner != name || s.Spans != 3 {
					t.Fatalf("trace %s on %s with %d spans, owner %s", s.TraceID, name, s.Spans, owner)
				}
			}
		}
	}
	if holding < 2 {
		t.Fatalf("traces not spread: %d node(s) hold traces", holding)
	}
	got := r.Search(1, traces.Query{Limit: 10})
	if len(got) != 10 || got[0].TraceID != fmt.Sprintf("%032x", 60) {
		t.Fatalf("search: %d results, first %v", len(got), got)
	}
	d, ok := r.Get(1, fmt.Sprintf("%032x", 7))
	if !ok || len(d.Spans) != 3 {
		t.Fatalf("get: %v %+v", ok, d)
	}
	if _, ok := r.Get(2, fmt.Sprintf("%032x", 7)); ok {
		t.Fatal("tenant 2 must not see tenant 1's trace")
	}
	if st := r.Stats(); st.Traces != 60 || st.Spans != 180 {
		t.Fatalf("stats %+v", st)
	}

	// A trace whose owner is down goes to the replica and stays readable.
	tc.srvs["s1"].Close()
	var onS1 int
	var late []traces.Span
	for tr := 100; tr < 140; tr++ {
		if o, _ := tc.mem.Ring().Get(traceKey(1, fmt.Sprintf("%032x", tr))); o == "s1" {
			onS1++
			late = append(late, span(tr, tr*10, "svc0", float64(1000*tr)))
		}
	}
	if onS1 == 0 {
		t.Skip("no trace hashed to s1")
	}
	if kept := r.Add(1, late); kept != len(late) {
		t.Fatalf("failover kept %d of %d", kept, len(late))
	}
	if _, ok := r.Get(1, late[0].TraceID); !ok {
		t.Fatal("trace written during failover not readable")
	}
	if res := r.Search(1, traces.Query{Limit: traces.MaxSearchLimit}); len(res) < len(late) {
		t.Fatalf("search with a shard down returned %d", len(res))
	}
}
