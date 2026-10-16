package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerowatch/zerowatch/internal/httpx"
	"github.com/zerowatch/zerowatch/internal/traces"
)

func traceServer(t *testing.T) (*http.ServeMux, *traces.Store) {
	t.Helper()
	st := traces.NewStore(traces.Config{})
	for i := 0; i < 3; i++ {
		status := traces.StatusOK
		if i == 1 {
			status = traces.StatusError
		}
		st.Add(1, []traces.Span{{TraceID: fmt.Sprintf("%032x", i+1), SpanID: fmt.Sprintf("%016x", i+1), Service: "svc",
			Name: fmt.Sprintf("GET /%d", i), Kind: traces.KindServer, Start: float64(1000 * (i + 1)), DurationMs: float64(10 * (i + 1)), Status: status}})
	}
	st.Add(7, []traces.Span{{TraceID: fmt.Sprintf("%032x", 99), SpanID: fmt.Sprintf("%016x", 99), Service: "other",
		Name: "x", Kind: traces.KindServer, Start: 5000, DurationMs: 1, Status: traces.StatusOK}})
	// Routes() only touches the fields it mounts, so a partial Server suffices.
	return (&Server{Traces: st}).Routes(), st
}

func doGet(mux *http.ServeMux, tenant uint32, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(httpx.WithTenant(req.Context(), tenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestTraceSearchHandler(t *testing.T) {
	mux, _ := traceServer(t)
	var body struct {
		Traces []traces.TraceSummary `json:"traces"`
	}
	rec := doGet(mux, 1, "/api/v1/traces")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Traces) != 3 || body.Traces[0].RootName != "GET /2" {
		t.Fatalf("search all: %d %s", rec.Code, rec.Body)
	}
	cases := map[string]int{
		"/api/v1/traces?error=true":                 1,
		"/api/v1/traces?minDurationMs=20":           2,
		"/api/v1/traces?start=1500&end=2500":        1,
		"/api/v1/traces?service=svc&name=GET%20/0":  1,
		"/api/v1/traces?limit=1":                    1,
		"/api/v1/traces?limit=100000":               3, // clamped, not rejected
		"/api/v1/traces?service=nope":               0,
		"/api/v1/traces?error=false&minDurationMs=": 3,
	}
	for path, n := range cases {
		body.Traces = nil
		rec := doGet(mux, 1, path)
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Traces) != n {
			t.Fatalf("%s: want %d, got %d %s", path, n, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/api/v1/traces?limit=0", "/api/v1/traces?minDurationMs=abc", "/api/v1/traces?error=maybe", "/api/v1/traces?start=-1"} {
		if rec := doGet(mux, 1, path); rec.Code != 400 {
			t.Fatalf("%s: want 400, got %d", path, rec.Code)
		}
	}
	// An empty result is an empty array, never null (the UI maps over it).
	if rec := doGet(mux, 42, "/api/v1/traces"); rec.Body.String() != "{\"traces\":[]}\n" && rec.Body.String() != "{\"traces\":[]}" {
		t.Fatalf("empty tenant: %q", rec.Body)
	}
}

func TestTraceGetHandler(t *testing.T) {
	mux, _ := traceServer(t)
	id := fmt.Sprintf("%032x", 2)
	rec := doGet(mux, 1, "/api/v1/traces/"+id)
	var d traces.TraceDetail
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil || d.TraceID != id || len(d.Spans) != 1 || d.Spans[0].Status != "error" {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	span := raw["spans"].([]any)[0].(map[string]any)
	for _, k := range []string{"traceId", "spanId", "service", "name", "kind", "start", "durationMs", "status"} {
		if _, ok := span[k]; !ok {
			t.Fatalf("span JSON missing %q: %v", k, span)
		}
	}
	if rec := doGet(mux, 1, "/api/v1/traces/"+fmt.Sprintf("%032x", 77)); rec.Code != 404 {
		t.Fatalf("unknown: want 404, got %d", rec.Code)
	}
	if rec := doGet(mux, 1, "/api/v1/traces/"+fmt.Sprintf("%032x", 99)); rec.Code != 404 {
		t.Fatalf("other tenant's trace: want 404, got %d", rec.Code)
	}
	if rec := doGet(mux, 1, "/api/v1/traces/not-hex"); rec.Code != 400 {
		t.Fatalf("malformed: want 400, got %d", rec.Code)
	}
}

func TestTraceRoutesAbsentWithoutStore(t *testing.T) {
	mux := (&Server{}).Routes()
	if rec := doGet(mux, 1, "/api/v1/traces"); rec.Code != 404 {
		t.Fatalf("want 404 without a store, got %d", rec.Code)
	}
}
