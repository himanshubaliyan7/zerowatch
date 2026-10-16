package ingest

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/zerowatch/zerowatch/internal/httpx"
)

// OTLP/HTTP protobuf. The *Data messages (MetricsData, LogsData,
// TracesData) share field 1 with the Export*ServiceRequest messages, so they
// decode the same bytes without pulling in the gRPC service packages. Each
// decoded message is converted into the JSON structs, so protobuf and JSON
// go through one conversion to the model and produce identical output.

// readOTLPBody reads a bounded body, inflating Content-Encoding: gzip. Both
// the compressed and the inflated size are capped at MaxOTLPBody (the latter
// guards against gzip bombs).
func readOTLPBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := httpx.ReadBody(w, r, MaxOTLPBody)
	if err != nil {
		return nil, err
	}
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
		return raw, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, httpx.BadRequest("invalid gzip body: " + err.Error())
		}
		defer zr.Close()
		b, err := io.ReadAll(io.LimitReader(zr, MaxOTLPBody+1))
		if err != nil {
			return nil, httpx.BadRequest("invalid gzip body: " + err.Error())
		}
		if len(b) > MaxOTLPBody {
			return nil, httpx.TooLarge(fmt.Sprintf("decompressed body exceeds %d bytes", MaxOTLPBody))
		}
		return b, nil
	default:
		return nil, &httpx.APIError{Status: http.StatusUnsupportedMediaType, Code: "UNSUPPORTED_MEDIA_TYPE",
			Message: "unsupported Content-Encoding " + strconv.Quote(enc) + " (use gzip or none)"}
	}
}

// decodeOTLP fills jsonDst from a JSON body, or pbDst from a protobuf body
// (then fromPB copies it into jsonDst). It reports whether the body was
// protobuf, so the response can use the same encoding.
func decodeOTLP(w http.ResponseWriter, r *http.Request, jsonDst any, pbDst proto.Message, fromPB func()) (bool, error) {
	body, err := readOTLPBody(w, r)
	if err != nil {
		return false, err
	}
	if isProtobuf(r) {
		if err := proto.Unmarshal(body, pbDst); err != nil {
			return true, httpx.BadRequest("invalid protobuf: " + err.Error())
		}
		fromPB()
		return true, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false, httpx.BadRequest("request body is required")
	}
	if err := json.Unmarshal(body, jsonDst); err != nil {
		return false, httpx.BadRequest("invalid JSON: " + err.Error())
	}
	return false, nil
}

// writeOTLPProto writes an Export*ServiceResponse. Every response type has
// partial_success = 1 { rejected_<items> int64 = 1; error_message = 2 }, so
// one hand encoder covers metrics, logs and traces; a full success is the
// empty message.
func writeOTLPProto(w http.ResponseWriter, rejected int, msg string) {
	var b []byte
	if rejected > 0 {
		var ps []byte
		ps = protowire.AppendTag(ps, 1, protowire.VarintType)
		ps = protowire.AppendVarint(ps, uint64(rejected))
		ps = protowire.AppendTag(ps, 2, protowire.BytesType)
		ps = protowire.AppendString(ps, msg)
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, ps)
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ---- protobuf → JSON structs

func pbAnyValue(v *commonpb.AnyValue) otlpAnyValue {
	var out otlpAnyValue
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		s := x.StringValue
		out.StringValue = &s
	case *commonpb.AnyValue_IntValue:
		out.IntValue = json.RawMessage(strconv.FormatInt(x.IntValue, 10))
	case *commonpb.AnyValue_DoubleValue:
		d := x.DoubleValue
		out.DoubleValue = &d
	case *commonpb.AnyValue_BoolValue:
		b := x.BoolValue
		out.BoolValue = &b
	}
	// Arrays, kvlists and bytes are not modelled by the JSON path either.
	return out
}

func pbKVs(kvs []*commonpb.KeyValue) []otlpKV {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]otlpKV, len(kvs))
	for i, kv := range kvs {
		out[i] = otlpKV{Key: kv.GetKey(), Value: pbAnyValue(kv.GetValue())}
	}
	return out
}

func pbResource(r *resourcepb.Resource) otlpResource {
	return otlpResource{Attributes: pbKVs(r.GetAttributes())}
}

func rawUint(v uint64) json.RawMessage { return json.RawMessage(strconv.FormatUint(v, 10)) }

func pbID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

func pbNumberDPs(dps []*metricspb.NumberDataPoint) *otlpNumberDPs {
	out := &otlpNumberDPs{DataPoints: make([]otlpNumberDP, 0, len(dps))}
	for _, dp := range dps {
		d := otlpNumberDP{Attributes: pbKVs(dp.GetAttributes()), TimeUnixNano: rawUint(dp.GetTimeUnixNano())}
		switch v := dp.GetValue().(type) {
		case *metricspb.NumberDataPoint_AsDouble:
			f := v.AsDouble
			d.AsDouble = &f
		case *metricspb.NumberDataPoint_AsInt:
			d.AsInt = json.RawMessage(strconv.FormatInt(v.AsInt, 10))
		}
		out.DataPoints = append(out.DataPoints, d)
	}
	return out
}

func otlpMetricsFromProto(md *metricspb.MetricsData) *OTLPMetricsRequest {
	req := &OTLPMetricsRequest{}
	for _, rm := range md.GetResourceMetrics() {
		orm := otlpResourceMetrics{Resource: pbResource(rm.GetResource())}
		for _, sm := range rm.GetScopeMetrics() {
			var osm otlpScopeMetrics
			for _, m := range sm.GetMetrics() {
				om := otlpMetric{Name: m.GetName(), Unit: m.GetUnit()}
				switch d := m.GetData().(type) {
				case *metricspb.Metric_Gauge:
					om.Gauge = pbNumberDPs(d.Gauge.GetDataPoints())
				case *metricspb.Metric_Sum:
					om.Sum = pbNumberDPs(d.Sum.GetDataPoints())
				case *metricspb.Metric_Histogram:
					h := &otlpHistDPs{}
					for _, dp := range d.Histogram.GetDataPoints() {
						h.DataPoints = append(h.DataPoints, otlpHistDP{
							Attributes: pbKVs(dp.GetAttributes()), TimeUnixNano: rawUint(dp.GetTimeUnixNano()),
							Count: rawUint(dp.GetCount()), Sum: dp.Sum, Min: dp.Min, Max: dp.Max,
						})
					}
					om.Histogram = h
				}
				osm.Metrics = append(osm.Metrics, om)
			}
			orm.ScopeMetrics = append(orm.ScopeMetrics, osm)
		}
		req.ResourceMetrics = append(req.ResourceMetrics, orm)
	}
	return req
}

func otlpLogsFromProto(ld *logspb.LogsData) *OTLPLogsRequest {
	req := &OTLPLogsRequest{}
	for _, rl := range ld.GetResourceLogs() {
		orl := otlpResourceLogs{Resource: pbResource(rl.GetResource())}
		for _, sl := range rl.GetScopeLogs() {
			var osl otlpScopeLogs
			for _, lr := range sl.GetLogRecords() {
				osl.LogRecords = append(osl.LogRecords, otlpLogRecord{
					TimeUnixNano: rawUint(lr.GetTimeUnixNano()), ObservedTimeUnixNano: rawUint(lr.GetObservedTimeUnixNano()),
					SeverityText: lr.GetSeverityText(), SeverityNumber: int(lr.GetSeverityNumber()),
					Body: pbAnyValue(lr.GetBody()), Attributes: pbKVs(lr.GetAttributes()),
					TraceID: pbID(lr.GetTraceId()), SpanID: pbID(lr.GetSpanId()),
				})
			}
			orl.ScopeLogs = append(orl.ScopeLogs, osl)
		}
		req.ResourceLogs = append(req.ResourceLogs, orl)
	}
	return req
}

func otlpTracesFromProto(td *tracepb.TracesData) *OTLPTracesRequest {
	req := &OTLPTracesRequest{}
	for _, rs := range td.GetResourceSpans() {
		ors := otlpResourceSpans{Resource: pbResource(rs.GetResource())}
		for _, ss := range rs.GetScopeSpans() {
			var oss otlpScopeSpans
			for _, s := range ss.GetSpans() {
				oss.Spans = append(oss.Spans, otlpSpan{
					TraceID: pbID(s.GetTraceId()), SpanID: pbID(s.GetSpanId()), ParentSpanID: pbID(s.GetParentSpanId()),
					Name: s.GetName(), Kind: json.RawMessage(strconv.Itoa(int(s.GetKind()))),
					StartTimeUnixNano: rawUint(s.GetStartTimeUnixNano()), EndTimeUnixNano: rawUint(s.GetEndTimeUnixNano()),
					Attributes: pbKVs(s.GetAttributes()),
					Status: otlpSpanStatus{
						Code:    json.RawMessage(strconv.Itoa(int(s.GetStatus().GetCode()))),
						Message: s.GetStatus().GetMessage(),
					},
				})
			}
			ors.ScopeSpans = append(ors.ScopeSpans, oss)
		}
		req.ResourceSpans = append(req.ResourceSpans, ors)
	}
	return req
}
