package ingest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zerowatch/zerowatch/internal/httpx"
	"github.com/zerowatch/zerowatch/internal/traces"
)

// OTLP/HTTP JSON traces (ExportTraceServiceRequest). Ids are hex strings in
// the JSON mapping; enums may arrive as integers or as their proto names.

type otlpSpanStatus struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId"`
	Name              string          `json:"name"`
	Kind              json.RawMessage `json:"kind"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
	Attributes        []otlpKV        `json:"attributes"`
	Status            otlpSpanStatus  `json:"status"`
}

type otlpScopeSpans struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

// OTLPTracesRequest is ExportTraceServiceRequest.
type OTLPTracesRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// enumValue reads a proto enum from JSON: an integer, or a name such as
// "SPAN_KIND_SERVER" / "STATUS_CODE_ERROR" (both are valid proto3 JSON).
func enumValue(r json.RawMessage, names []string) int {
	s := strings.Trim(string(r), `"`)
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	for i, name := range names {
		if s == name {
			return i
		}
	}
	return 0
}

var (
	spanKindNames   = []string{"SPAN_KIND_UNSPECIFIED", "SPAN_KIND_INTERNAL", "SPAN_KIND_SERVER", "SPAN_KIND_CLIENT", "SPAN_KIND_PRODUCER", "SPAN_KIND_CONSUMER"}
	spanKinds       = []string{traces.KindInternal, traces.KindInternal, traces.KindServer, traces.KindClient, traces.KindProducer, traces.KindConsumer}
	statusCodeNames = []string{"STATUS_CODE_UNSET", "STATUS_CODE_OK", "STATUS_CODE_ERROR"}
	statusCodes     = []string{traces.StatusUnset, traces.StatusOK, traces.StatusError}
)

func nanoToMsFloat(r json.RawMessage) float64 {
	v, ok := rawInt(r)
	if !ok || v <= 0 {
		return 0
	}
	return float64(v) / 1e6
}

// ConvertOTLPTraces maps OTLP spans to traces.Span. Invalid spans (bad ids,
// no start time) are rejected individually and reported in res.
func ConvertOTLPTraces(req *OTLPTracesRequest) ([]traces.Span, Result) {
	var out []traces.Span
	var res Result
	i := -1
	for _, rs := range req.ResourceSpans {
		service := ""
		resAttrs := map[string]string{}
		for _, kv := range rs.Resource.Attributes {
			if kv.Key == "service.name" {
				service = kv.Value.String()
				continue
			}
			if v := kv.Value.String(); v != "" {
				resAttrs[kv.Key] = v
			}
		}
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				i++
				attrs := make(map[string]string, len(resAttrs)+len(s.Attributes))
				for k, v := range resAttrs {
					attrs[k] = v
				}
				for _, kv := range s.Attributes { // span attributes win over resource ones
					attrs[kv.Key] = kv.Value.String()
				}
				if len(attrs) == 0 {
					attrs = nil
				}
				start := nanoToMsFloat(s.StartTimeUnixNano)
				// Duration from the integer nanoseconds: subtracting two float ms
				// timestamps (~1.8e12) loses sub-microsecond precision (12.5 ms
				// came out as 12.49976).
				dur := 0.0
				if sn, ok := rawInt(s.StartTimeUnixNano); ok {
					if en, ok := rawInt(s.EndTimeUnixNano); ok && en > sn {
						dur = float64(en-sn) / 1e6
					}
				}
				kind := enumValue(s.Kind, spanKindNames)
				code := enumValue(s.Status.Code, statusCodeNames)
				sp := traces.Span{
					TraceID: s.TraceID, SpanID: s.SpanID, ParentSpanID: s.ParentSpanID,
					Service: service, Name: s.Name, Start: start, DurationMs: dur,
					Kind: pick(spanKinds, kind), Status: pick(statusCodes, code),
					StatusMessage: s.Status.Message, Attributes: attrs,
				}
				sp.Normalize()
				if err := sp.Validate(); err != nil {
					res.reject(i, err)
					continue
				}
				out = append(out, sp)
			}
		}
	}
	return out, res
}

func pick(vals []string, i int) string {
	if i < 0 || i >= len(vals) {
		return ""
	}
	return vals[i]
}

// errNoSpans is returned when a non-empty traces request had no valid span.
// It is an APIError (400) itself rather than relying on the api package's
// ErrValidation mapper, so the handler behaves the same when mounted alone.
func errNoSpans(res Result) error {
	return httpx.BadRequest(fmt.Sprintf("every span in the request was rejected: %v", res.Errors))
}
