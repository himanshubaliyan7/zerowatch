package generator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/commitlog"
	"github.com/zerowatch/zerowatch/internal/ingest"
	"github.com/zerowatch/zerowatch/internal/traces"
)

func TestTickProducesValidRED(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl := commitlog.NewMemory(100000)
	p := ingest.NewPipeline(cl, log)
	g := New(Config{Services: 3, Seed: 42}, p, log)
	now := time.Now()
	ms, ls := g.Tick(now, time.Second)
	names := map[string]int{}
	for _, m := range ms {
		names[m.MetricName]++
		if err := m.Validate(now); err != nil {
			t.Fatalf("invalid datum %+v: %v", m, err)
		}
	}
	if names["Requests"] != 9 || names["Errors"] != 9 || names["Latency"] == 0 {
		t.Fatalf("unexpected metric mix: %v", names)
	}
	if len(ls) == 0 {
		t.Fatal("expected logs")
	}
	res, err := p.WriteLogs(context.Background(), 1, ls)
	if err != nil || res.Rejected != 0 {
		t.Fatalf("logs rejected: %+v %v", res, err)
	}
	if g.poisson(0) != 0 || g.poisson(1000) < 800 {
		t.Fatal("poisson sampler out of range")
	}
}

func TestLoggedRequestsHaveTraces(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := ingest.NewPipeline(commitlog.NewMemory(100000), log)
	store := traces.NewStore(traces.Config{})
	g := New(Config{Services: 6, Seed: 7, Traces: store}, p, log)
	now := time.Now()
	ms, ls, spans := g.TickWithTraces(now, 5*time.Second)
	for i := range spans {
		if err := spans[i].Validate(); err != nil {
			t.Fatalf("invalid span %+v: %v", spans[i], err)
		}
	}
	if kept := store.Add(1, spans); kept != len(spans) {
		t.Fatalf("store kept %d of %d", kept, len(spans))
	}
	logged, withCalls := 0, 0
	for _, l := range ls {
		id := l.Attributes["trace_id"]
		if id == "" {
			continue // EMF lines carry no trace
		}
		logged++
		d, ok := store.Get(1, id)
		if !ok {
			t.Fatalf("log trace_id %s has no trace", id)
		}
		root := d.Spans[0]
		if root.ParentSpanID != "" || root.SpanID != l.Attributes["span_id"] || root.Service != l.Attributes["service"] ||
			root.Name != l.Attributes["route"] || root.Kind != traces.KindServer || root.End() != float64(l.Timestamp) {
			t.Fatalf("root span %+v does not match log %+v", root, l)
		}
		if (root.Status == traces.StatusError) != (l.Level == "error") {
			t.Fatalf("status mismatch: span %s, log level %s", root.Status, l.Level)
		}
		byID := map[string]traces.Span{}
		for _, s := range d.Spans {
			byID[s.SpanID] = s
		}
		errs := 0
		for _, s := range d.Spans[1:] {
			parent, ok := byID[s.ParentSpanID]
			if !ok || s.Start < parent.Start || s.End() > parent.End()+1e-6 {
				t.Fatalf("span %+v not nested in parent %+v", s, parent)
			}
			if s.Kind == traces.KindServer && (parent.Kind != traces.KindClient || parent.Attributes["peer.service"] != s.Service) {
				t.Fatalf("server span %+v without matching client %+v", s, parent)
			}
			if s.IsError() {
				errs++
			}
		}
		if len(d.Spans) > 1 {
			withCalls++
			if root.IsError() && errs == 0 {
				t.Fatalf("error did not propagate from a downstream call: %+v", d.Spans)
			}
		}
	}
	if logged == 0 || withCalls == 0 {
		t.Fatalf("expected traced requests with downstream calls: logged=%d withCalls=%d", logged, withCalls)
	}
	// Synthetic spans must never be turned into RED (the generator already emits it).
	if red := traces.REDMetrics(spans); len(red) != 0 {
		t.Fatalf("synthetic spans produced %d RED datums", len(red))
	}
	if len(ms) == 0 {
		t.Fatal("metrics must still be emitted alongside traces")
	}
}
