package ingest

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/zerowatch/zerowatch/internal/commitlog"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/traces"
)

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func testResource() *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", "api"), strAttr("host.name", "h1")}}
}

// post runs one request through the registered routes.
func post(t *testing.T, h *Handlers, path, ct string, body []byte, gz bool) *httptest.ResponseRecorder {
	t.Helper()
	if gz {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = buf.Bytes()
	}
	mux := http.NewServeMux()
	h.Register(mux, nil)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func mustProto(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func metricsOf(c *capture) []model.MetricDatum {
	var out []model.MetricDatum
	for _, r := range c.recs {
		if r.Metric != nil {
			out = append(out, *r.Metric)
		}
	}
	return out
}

func logsOf(c *capture) []model.LogEvent {
	var out []model.LogEvent
	for _, r := range c.recs {
		if r.Log != nil {
			out = append(out, *r.Log)
		}
	}
	return out
}

func TestOTLPMetricsProtobufMatchesJSON(t *testing.T) {
	ns := uint64(time.Now().Add(-time.Minute).UnixNano())
	sum, mn, mx := 30.0, 2.0, 20.0
	pb := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: testResource(),
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "cpu", Unit: "Percent", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{
				{Attributes: []*commonpb.KeyValue{strAttr("core", "0")}, TimeUnixNano: ns, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.5}},
			}}}},
			{Name: "reqs", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{
				{Attributes: []*commonpb.KeyValue{intAttr("code", 200)}, TimeUnixNano: ns, Value: &metricspb.NumberDataPoint_AsInt{AsInt: 7}},
			}}}},
			{Name: "lat", Unit: "Milliseconds", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{
				{TimeUnixNano: ns, Count: 3, Sum: &sum, Min: &mn, Max: &mx},
			}}}},
		}}},
	}}}
	js := `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}},{"key":"host.name","value":{"stringValue":"h1"}}]},
	"scopeMetrics":[{"metrics":[
	 {"name":"cpu","unit":"Percent","gauge":{"dataPoints":[{"attributes":[{"key":"core","value":{"stringValue":"0"}}],"timeUnixNano":"NS","asDouble":0.5}]}},
	 {"name":"reqs","sum":{"dataPoints":[{"attributes":[{"key":"code","value":{"intValue":"200"}}],"timeUnixNano":"NS","asInt":"7"}]}},
	 {"name":"lat","unit":"Milliseconds","histogram":{"dataPoints":[{"timeUnixNano":"NS","count":"3","sum":30,"min":2,"max":20}]}}
	]}]}]}`
	js = strings.ReplaceAll(js, "NS", ujsonNum(ns))

	pj, cj := newTestPipeline()
	if rec := post(t, &Handlers{Pipeline: pj}, "/v1/metrics", "application/json", []byte(js), false); rec.Code != 200 {
		t.Fatalf("json: %d %s", rec.Code, rec.Body)
	}
	for _, gz := range []bool{false, true} {
		pp, cp := newTestPipeline()
		rec := post(t, &Handlers{Pipeline: pp}, "/v1/metrics", "application/x-protobuf", mustProto(t, pb), gz)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-protobuf" || rec.Body.Len() != 0 {
			t.Fatalf("protobuf (gzip=%v): %d %q %q", gz, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
		got, want := metricsOf(cp), metricsOf(cj)
		if len(want) != 3 || !reflect.DeepEqual(got, want) {
			t.Fatalf("gzip=%v parity mismatch:\n pb=%+v\njson=%+v", gz, got, want)
		}
	}
}

func ujsonNum(v uint64) string { b, _ := json.Marshal(v); return string(b) }

func TestOTLPLogsProtobufMatchesJSON(t *testing.T) {
	ns := uint64(time.Now().Add(-time.Minute).UnixNano())
	tid, _ := hex.DecodeString("0af7651916cd43dd8448eb211c80319c")
	sid, _ := hex.DecodeString("b7ad6b7169203331")
	pb := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: testResource(),
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{TimeUnixNano: ns, SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "boom"}},
				Attributes: []*commonpb.KeyValue{strAttr("route", "/x")}, TraceId: tid, SpanId: sid},
			{ObservedTimeUnixNano: ns, SeverityText: "WARN", Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}}},
		}}},
	}}}
	js := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}},{"key":"host.name","value":{"stringValue":"h1"}}]},
	"scopeLogs":[{"logRecords":[
	 {"timeUnixNano":"NS","severityNumber":17,"body":{"stringValue":"boom"},"attributes":[{"key":"route","value":{"stringValue":"/x"}}],"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331"},
	 {"observedTimeUnixNano":"NS","severityText":"WARN","body":{"intValue":42}}
	]}]}]}`
	js = strings.ReplaceAll(js, "NS", ujsonNum(ns))

	pj, cj := newTestPipeline()
	if rec := post(t, &Handlers{Pipeline: pj}, "/v1/logs", "application/json", []byte(js), false); rec.Code != 200 {
		t.Fatalf("json: %d %s", rec.Code, rec.Body)
	}
	pp, cp := newTestPipeline()
	if rec := post(t, &Handlers{Pipeline: pp}, "/v1/logs", "application/x-protobuf", mustProto(t, pb), true); rec.Code != 200 {
		t.Fatalf("protobuf: %d %s", rec.Code, rec.Body)
	}
	got, want := logsOf(cp), logsOf(cj)
	if len(want) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("parity mismatch:\n pb=%+v\njson=%+v", got, want)
	}
	if want[0].Attributes["trace_id"] != "0af7651916cd43dd8448eb211c80319c" || want[0].Level != "error" || want[1].Message != "42" {
		t.Fatalf("unexpected conversion: %+v", want)
	}
}

func traceFixture(ns uint64) (*tracepb.TracesData, string) {
	tid, _ := hex.DecodeString("0af7651916cd43dd8448eb211c80319c")
	root, _ := hex.DecodeString("b7ad6b7169203331")
	child, _ := hex.DecodeString("00f067aa0ba902b7")
	pb := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: testResource(),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
			{TraceId: tid, SpanId: root, Name: "GET /users", Kind: tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: ns, EndTimeUnixNano: ns + 12_500_000, Attributes: []*commonpb.KeyValue{intAttr("http.status_code", 500)},
				Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "db down"}},
			{TraceId: tid, SpanId: child, ParentSpanId: root, Name: "SELECT users", Kind: tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: ns + 1_000_000, EndTimeUnixNano: ns + 11_000_000},
		}}},
	}}}
	js := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}},{"key":"host.name","value":{"stringValue":"h1"}}]},
	"scopeSpans":[{"spans":[
	 {"traceId":"0AF7651916CD43DD8448EB211C80319C","spanId":"b7ad6b7169203331","name":"GET /users","kind":"SPAN_KIND_SERVER","startTimeUnixNano":"NS0","endTimeUnixNano":"NS1",
	  "attributes":[{"key":"http.status_code","value":{"intValue":"500"}}],"status":{"code":2,"message":"db down"}},
	 {"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"00f067aa0ba902b7","parentSpanId":"b7ad6b7169203331","name":"SELECT users","kind":3,"startTimeUnixNano":"NS2","endTimeUnixNano":"NS3"}
	]}]}]}`
	r := strings.NewReplacer("NS0", ujsonNum(ns), "NS1", ujsonNum(ns+12_500_000), "NS2", ujsonNum(ns+1_000_000), "NS3", ujsonNum(ns+11_000_000))
	return pb, r.Replace(js)
}

func TestOTLPTracesProtobufMatchesJSONAndDerivesRED(t *testing.T) {
	ns := uint64(time.Now().Add(-time.Minute).UnixNano())
	pb, js := traceFixture(ns)
	const id = "0af7651916cd43dd8448eb211c80319c"

	pj, cj := newTestPipeline()
	sj := traces.NewStore(traces.Config{})
	if rec := post(t, &Handlers{Pipeline: pj, Traces: sj}, "/v1/traces", "application/json", []byte(js), false); rec.Code != 200 {
		t.Fatalf("json: %d %s", rec.Code, rec.Body)
	}
	pp, cp := newTestPipeline()
	sp := traces.NewStore(traces.Config{})
	if rec := post(t, &Handlers{Pipeline: pp, Traces: sp}, "/v1/traces", "application/protobuf", mustProto(t, pb), true); rec.Code != 200 {
		t.Fatalf("protobuf: %d %s", rec.Code, rec.Body)
	}
	want, ok1 := sj.Get(1, id)
	got, ok2 := sp.Get(1, id)
	if !ok1 || !ok2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("parity mismatch:\n pb=%+v\njson=%+v", got, want)
	}
	if len(want.Spans) != 2 {
		t.Fatalf("want 2 spans, got %+v", want.Spans)
	}
	root := want.Spans[0]
	if root.Kind != "server" || root.Status != "error" || root.StatusMessage != "db down" || root.Service != "api" ||
		root.DurationMs != 12.5 || root.Attributes["http.status_code"] != "500" || root.Attributes["host.name"] != "h1" {
		t.Fatalf("bad root span: %+v", root)
	}
	if c := want.Spans[1]; c.Kind != "client" || c.ParentSpanID != root.SpanID || c.Status != "unset" {
		t.Fatalf("bad child span: %+v", c)
	}
	// Only the SERVER span yields RED: Requests=1, Errors=1, one Latency sample.
	red := metricsOf(cj)
	if !reflect.DeepEqual(red, metricsOf(cp)) || len(red) != 3 {
		t.Fatalf("RED mismatch: %+v vs %+v", red, metricsOf(cp))
	}
	for _, m := range red {
		if m.Namespace != traces.REDNamespace || m.Dimensions[0].Value != "GET /users" || m.Dimensions[1].Value != "api" {
			t.Fatalf("bad RED datum %+v", m)
		}
	}
}

func TestOTLPTracesValidation(t *testing.T) {
	p, c := newTestPipeline()
	h := &Handlers{Pipeline: p, Traces: traces.NewStore(traces.Config{})}
	bad := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"zz","spanId":"b7ad6b7169203331","name":"x","startTimeUnixNano":"1"}]}]}]}`
	if rec := post(t, h, "/v1/traces", "application/json", []byte(bad), false); rec.Code != 400 {
		t.Fatalf("all-invalid batch: want 400, got %d %s", rec.Code, rec.Body)
	}
	ns := time.Now().UnixNano()
	mixed := `{"resourceSpans":[{"scopeSpans":[{"spans":[
	 {"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331","name":"ok","startTimeUnixNano":"` + ujsonNum(uint64(ns)) + `"},
	 {"traceId":"00000000000000000000000000000000","spanId":"b7ad6b7169203331","name":"zero","startTimeUnixNano":"1"}]}]}]}`
	rec := post(t, h, "/v1/traces", "application/json", []byte(mixed), false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rejectedSpans":1`) {
		t.Fatalf("partial success: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, "/v1/traces", "application/x-protobuf", []byte{0xff, 0xff}, false); rec.Code != 400 {
		t.Fatalf("garbage protobuf: want 400, got %d", rec.Code)
	}
	if rec := post(t, h, "/v1/traces", "application/json", []byte("not gzip"), false); rec.Code != 400 {
		t.Fatalf("bad json: want 400, got %d", rec.Code)
	}
	_ = c
	// Gzip that is not gzip, and an unknown encoding.
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("plain"))
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	h.OTLPTraces(rr, req)
	if rr.Code != 400 {
		t.Fatalf("bad gzip: want 400, got %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("{}"))
	req.Header.Set("Content-Encoding", "br")
	rr = httptest.NewRecorder()
	h.OTLPTraces(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("br: want 415, got %d", rr.Code)
	}
}

func TestOTLPGzipBombRejected(t *testing.T) {
	p, _ := newTestPipeline()
	big := bytes.Repeat([]byte(" "), MaxOTLPBody+10) // compresses to a few KB
	rec := post(t, &Handlers{Pipeline: p}, "/v1/metrics", "application/json", big, true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rec.Code)
	}
}

func TestOTLPProtoPartialSuccessResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOTLPProto(rec, 3, "bad")
	b := rec.Body.Bytes()
	num, typ, n := protowire.ConsumeTag(b)
	if num != 1 || typ != protowire.BytesType || n < 0 {
		t.Fatalf("bad outer tag")
	}
	inner, _ := protowire.ConsumeBytes(b[n:])
	num, _, n = protowire.ConsumeTag(inner)
	v, _ := protowire.ConsumeVarint(inner[n:])
	if num != 1 || v != 3 {
		t.Fatalf("rejected count: field %d value %d", num, v)
	}
}

func TestTracesRouteOnlyWithStore(t *testing.T) {
	p, _ := newTestPipeline()
	if rec := post(t, &Handlers{Pipeline: p}, "/v1/traces", "application/json", []byte(`{}`), false); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want no /v1/traces without a store, got %d", rec.Code)
	}
}

var _ commitlog.Producer = (*capture)(nil)
