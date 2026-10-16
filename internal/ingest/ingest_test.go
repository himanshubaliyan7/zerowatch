package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/commitlog"
	"github.com/zerowatch/zerowatch/internal/model"
)

type capture struct{ recs []commitlog.Record }

func (c *capture) Publish(_ context.Context, r []commitlog.Record) error {
	c.recs = append(c.recs, r...)
	return nil
}
func (c *capture) Close() error { return nil }

func newTestPipeline() (*Pipeline, *capture) {
	c := &capture{}
	return NewPipeline(c, slog.New(slog.NewTextHandler(io.Discard, nil))), c
}

func TestPromRemoteWriteRoundTrip(t *testing.T) {
	now := time.Now().UnixMilli()
	in := []model.MetricDatum{
		{MetricName: "http_requests_total", Dimensions: []model.Dimension{{Name: "job", Value: "api"}, {Name: "code", Value: "200"}}, Timestamp: now, Value: model.Float(42)},
		{MetricName: "up", Timestamp: now + 1000, Value: model.Float(1)},
	}
	out, err := DecodeRemoteWrite(EncodeRemoteWrite(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].MetricName != "http_requests_total" || *out[0].Value != 42 || out[0].Namespace != PromNamespace {
		t.Fatalf("bad decode: %+v", out)
	}
	if out[0].Dimensions[0].Name != "code" || out[1].Timestamp != now+1000 {
		t.Fatalf("dims not sorted or ts wrong: %+v", out)
	}
	if _, err := DecodeRemoteWrite([]byte("not snappy at all")); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("want validation error, got %v", err)
	}
}

func TestStatsDParseAndAggregate(t *testing.T) {
	cases := []struct {
		line    string
		wantErr bool
	}{
		{"api.hits:1|c", false},
		{"api.hits:2|c|@0.5|#env:prod,route:/x", false},
		{"lat:12.5|ms", false},
		{"q:+3|g", false},
		{"users:alice|s", false},
		{"bad", true},
		{"x:1|zz", true},
		{"x:abc|c", true},
		{"x:1|c|@2", true},
	}
	for _, c := range cases {
		_, err := ParseStatsDLine(c.line)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", c.line, err, c.wantErr)
		}
	}
	a := NewStatsDAggregator(10)
	for _, l := range []string{"hits:1|c", "hits:1|c|@0.5", "lat:10|ms", "lat:30|ms", "g:5|g", "g:+2|g", "u:a|s", "u:b|s", "u:a|s"} {
		s, err := ParseStatsDLine(l)
		if err != nil {
			t.Fatal(err)
		}
		a.Add(s)
	}
	got := map[string]model.MetricDatum{}
	for _, d := range a.Flush(1) {
		got[d.MetricName] = d
	}
	if *got["hits"].Value != 3 {
		t.Errorf("counter with sample rate: got %v want 3", *got["hits"].Value)
	}
	if sv := got["lat"].StatisticValues; sv.SampleCount != 2 || sv.Sum != 40 || sv.Minimum != 10 || sv.Maximum != 30 {
		t.Errorf("timer stats wrong: %+v", sv)
	}
	if *got["g"].Value != 7 || *got["u"].Value != 2 {
		t.Errorf("gauge=%v set=%v", *got["g"].Value, *got["u"].Value)
	}
	if len(a.Flush(2)) != 0 {
		t.Error("flush should reset interval")
	}
}

func TestOTLPConvert(t *testing.T) {
	body := `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}},{"key":"host.name","value":{"stringValue":"h1"}}]},
	"scopeMetrics":[{"metrics":[
	 {"name":"requests","unit":"1","sum":{"dataPoints":[{"attributes":[{"key":"route","value":{"stringValue":"/pay"}}],"timeUnixNano":"1700000000000000000","asInt":"7"}]}},
	 {"name":"latency","unit":"ms","histogram":{"dataPoints":[{"timeUnixNano":"1700000000000000000","count":"4","sum":100,"min":5,"max":60}]}}
	]}]}]}`
	var req OTLPMetricsRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	ms, err := ConvertOTLPMetrics(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || *ms[0].Value != 7 || ms[0].Timestamp != 1700000000000 {
		t.Fatalf("bad conversion: %+v", ms)
	}
	if len(ms[0].Dimensions) != 2 { // service.name + route; host.name dropped (cardinality)
		t.Fatalf("dims: %+v", ms[0].Dimensions)
	}
	if ms[1].StatisticValues.SampleCount != 4 || ms[1].StatisticValues.Maximum != 60 {
		t.Fatalf("histogram: %+v", ms[1].StatisticValues)
	}
	lbody := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"1700000000000000000","severityNumber":17,"body":{"stringValue":"boom"},"traceId":"abc"}]}]}]}`
	var lreq OTLPLogsRequest
	if err := json.Unmarshal([]byte(lbody), &lreq); err != nil {
		t.Fatal(err)
	}
	ls := ConvertOTLPLogs(&lreq)
	if len(ls) != 1 || ls[0].Level != "error" || ls[0].LogGroup != "api" || ls[0].Attributes["trace_id"] != "abc" {
		t.Fatalf("logs: %+v", ls)
	}
}

func TestPutMetricDataForm(t *testing.T) {
	f := url.Values{}
	f.Set("Action", "PutMetricData")
	f.Set("Namespace", "App")
	f.Set("MetricData.member.1.MetricName", "Latency")
	f.Set("MetricData.member.1.Value", "12.5")
	f.Set("MetricData.member.1.Unit", "Milliseconds")
	f.Set("MetricData.member.1.Dimensions.member.1.Name", "Service")
	f.Set("MetricData.member.1.Dimensions.member.1.Value", "api")
	f.Set("MetricData.member.2.MetricName", "Batch")
	f.Set("MetricData.member.2.Values.member.1", "10")
	f.Set("MetricData.member.2.Values.member.2", "20")
	f.Set("MetricData.member.2.Counts.member.2", "3")
	ms, err := ParsePutMetricDataForm(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || *ms[0].Value != 12.5 || ms[0].Dimensions[0].Value != "api" {
		t.Fatalf("%+v", ms)
	}
	if sv := ms[1].StatisticValues; sv.SampleCount != 4 || sv.Sum != 70 {
		t.Fatalf("values/counts: %+v", sv)
	}

	p, c := newTestPipeline()
	h := &CloudWatchHandler{Pipeline: p}
	req := httptest.NewRequest(http.MethodPost, "/cloudwatch", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "PutMetricDataResponse") || len(c.recs) != 2 {
		t.Fatalf("code=%d body=%s recs=%d", rec.Code, rec.Body.String(), len(c.recs))
	}

	// JSON protocol PutLogEvents with an embedded EMF line -> log + metric.
	emfLine := `{"_aws":{"Timestamp":` + jsonNum(time.Now().UnixMilli()) + `,"CloudWatchMetrics":[{"Namespace":"Svc","Dimensions":[["Op"]],"Metrics":[{"Name":"Time","Unit":"Milliseconds"}]}]},"Op":"get","Time":17}`
	payload, _ := json.Marshal(map[string]any{"logGroupName": "g", "logStreamName": "s",
		"logEvents": []map[string]any{{"timestamp": time.Now().UnixMilli(), "message": emfLine}}})
	req = httptest.NewRequest(http.MethodPost, "/cloudwatch", strings.NewReader(string(payload)))
	req.Header.Set("X-Amz-Target", "Logs_20140328.PutLogEvents")
	rec = httptest.NewRecorder()
	c.recs = nil
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(c.recs) != 2 || c.recs[1].Metric == nil || c.recs[1].Metric.MetricName != "Time" {
		t.Fatalf("PutLogEvents: code=%d body=%s recs=%+v", rec.Code, rec.Body.String(), c.recs)
	}
}

func jsonNum(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestPipelineValidation(t *testing.T) {
	p, c := newTestPipeline()
	res, err := p.WriteMetrics(context.Background(), 7, []model.MetricDatum{
		{Namespace: "A", MetricName: "ok", Value: model.Float(1)},
		{Namespace: "", MetricName: "bad", Value: model.Float(1)},
	})
	if err != nil || res.Accepted != 1 || res.Rejected != 1 || c.recs[0].Metric.TenantID != 7 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := p.WriteMetrics(context.Background(), 1, []model.MetricDatum{{MetricName: "x"}}); !errors.Is(err, ErrAllRejected) {
		t.Fatalf("want ErrAllRejected, got %v", err)
	}
	if InferLevel(`{"level":"error","msg":"x"}`) != "error" || InferLevel("WARNING disk") != "warn" || InferLevel("terrorist") != "info" {
		t.Fatal("InferLevel heuristics broken")
	}
}
