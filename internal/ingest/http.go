package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/zerowatch/zerowatch/internal/httpx"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/traces"
)

// Body limits per protocol.
const (
	MaxOTLPBody = 8 << 20
	MaxPromBody = 8 << 20
	MaxJSONBody = 4 << 20
)

// Handlers exposes the HTTP ingest endpoints.
type Handlers struct {
	Pipeline *Pipeline
	// Traces receives OTLP spans; POST /v1/traces is mounted only when set.
	Traces traces.Backend
}

func isProtobuf(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "application/x-protobuf" || ct == "application/protobuf"
}

func writeResult(w http.ResponseWriter, r *http.Request, res Result, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// writeOTLPResult answers in the request's encoding. Rejections use the OTLP
// partial-success shape (rejectedKey is rejectedDataPoints, rejectedLogRecords
// or rejectedSpans). Errors stay JSON envelopes in both encodings.
func writeOTLPResult(w http.ResponseWriter, r *http.Request, pb bool, res Result, err error, rejectedKey string) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if pb {
		writeOTLPProto(w, res.Rejected, fmt.Sprint(res.Errors))
		return
	}
	if res.Rejected > 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"partialSuccess": map[string]any{
			rejectedKey: res.Rejected, "errorMessage": fmt.Sprint(res.Errors)}})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// OTLPMetrics handles POST /v1/metrics (OTLP/HTTP JSON or protobuf, optionally gzip).
func (h *Handlers) OTLPMetrics(w http.ResponseWriter, r *http.Request) {
	var req OTLPMetricsRequest
	var pbReq metricspb.MetricsData
	pb, err := decodeOTLP(w, r, &req, &pbReq, func() { req = *otlpMetricsFromProto(&pbReq) })
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ms, err := ConvertOTLPMetrics(&req)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.Pipeline.WriteMetrics(r.Context(), httpx.Tenant(r.Context()), ms)
	writeOTLPResult(w, r, pb, res, err, "rejectedDataPoints")
}

// OTLPLogs handles POST /v1/logs (OTLP/HTTP JSON or protobuf, optionally gzip).
func (h *Handlers) OTLPLogs(w http.ResponseWriter, r *http.Request) {
	var req OTLPLogsRequest
	var pbReq logspb.LogsData
	pb, err := decodeOTLP(w, r, &req, &pbReq, func() { req = *otlpLogsFromProto(&pbReq) })
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.Pipeline.WriteLogs(r.Context(), httpx.Tenant(r.Context()), ConvertOTLPLogs(&req))
	if pb && err == nil {
		writeOTLPProto(w, res.Rejected, fmt.Sprint(res.Errors))
		return
	}
	// The JSON response shape for logs is unchanged (a plain Result).
	writeResult(w, r, res, err)
}

// OTLPTraces handles POST /v1/traces: spans go to the trace store, and
// SERVER spans also feed RED metrics into the pipeline.
func (h *Handlers) OTLPTraces(w http.ResponseWriter, r *http.Request) {
	var req OTLPTracesRequest
	var pbReq tracepb.TracesData
	pb, err := decodeOTLP(w, r, &req, &pbReq, func() { req = *otlpTracesFromProto(&pbReq) })
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	spans, res := ConvertOTLPTraces(&req)
	if len(spans) == 0 && res.Rejected > 0 {
		httpx.WriteError(w, r, errNoSpans(res))
		return
	}
	tenant := httpx.Tenant(r.Context())
	kept := h.Traces.Add(tenant, spans)
	res.Accepted = len(spans)
	if dup := len(spans) - kept; dup > 0 && h.Pipeline.Log != nil {
		h.Pipeline.Log.Debug("trace store dropped spans (duplicate or per-trace cap)", "count", dup)
	}
	if red := traces.REDMetrics(spans); len(red) > 0 {
		// RED is derived data: a rejected datum (e.g. a span too old for the
		// TSDB) must not fail the trace export, but backpressure should.
		if _, err := h.Pipeline.WriteMetrics(r.Context(), tenant, red); err != nil && !errors.Is(err, model.ErrValidation) {
			httpx.WriteError(w, r, err)
			return
		}
	}
	writeOTLPResult(w, r, pb, res, nil, "rejectedSpans")
}

// PromRemoteWrite handles POST /api/v1/write.
func (h *Handlers) PromRemoteWrite(w http.ResponseWriter, r *http.Request) {
	body, err := httpx.ReadBody(w, r, MaxPromBody)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ms, err := DecodeRemoteWrite(body)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.Pipeline.WriteMetrics(r.Context(), httpx.Tenant(r.Context()), ms)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	_ = res
	w.WriteHeader(http.StatusNoContent) // Prometheus expects 2xx with empty body
}

// NativeMetrics handles POST /api/v1/ingest/metrics with a JSON array of datums.
func (h *Handlers) NativeMetrics(w http.ResponseWriter, r *http.Request) {
	var ms []model.MetricDatum
	if err := httpx.DecodeJSON(w, r, &ms, MaxJSONBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.Pipeline.WriteMetrics(r.Context(), httpx.Tenant(r.Context()), ms)
	writeResult(w, r, res, err)
}

// NativeLogs handles POST /api/v1/ingest/logs with a JSON array of events
// (or NDJSON when Content-Type is application/x-ndjson).
func (h *Handlers) NativeLogs(w http.ResponseWriter, r *http.Request) {
	var ls []model.LogEvent
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-ndjson" {
		body, err := httpx.ReadBody(w, r, model.MaxLogBatchBytes+model.MaxLogBatchEvents*256)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		for dec.More() {
			var e model.LogEvent
			if err := dec.Decode(&e); err != nil {
				httpx.WriteError(w, r, httpx.BadRequest("invalid NDJSON: "+err.Error()))
				return
			}
			ls = append(ls, e)
		}
	} else if err := httpx.DecodeJSON(w, r, &ls, MaxJSONBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.Pipeline.WriteLogs(r.Context(), httpx.Tenant(r.Context()), ls)
	writeResult(w, r, res, err)
}

// Register mounts all ingest routes on mux (Go 1.22 patterns).
func (h *Handlers) Register(mux *http.ServeMux, cw *CloudWatchHandler) {
	mux.HandleFunc("POST /v1/metrics", h.OTLPMetrics)
	mux.HandleFunc("POST /v1/logs", h.OTLPLogs)
	if h.Traces != nil {
		mux.HandleFunc("POST /v1/traces", h.OTLPTraces)
	}
	mux.HandleFunc("POST /api/v1/write", h.PromRemoteWrite)
	mux.HandleFunc("POST /api/v1/ingest/metrics", h.NativeMetrics)
	mux.HandleFunc("POST /api/v1/ingest/logs", h.NativeLogs)
	if cw != nil {
		mux.Handle("POST /cloudwatch", cw)
		mux.Handle("POST /cloudwatch/", cw)
	}
}
