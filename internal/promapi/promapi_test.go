package promapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

func TestParse(t *testing.T) {
	ok := []string{
		`foo`, `foo{a="b"}`, `{__name__="foo",a!="b",c=~"x.*",d!~'y'}`, `foo[5m]`, `foo[1h30m] offset 5m`,
		`foo offset -1m`, `rate(foo[5m])`, `sum by (a, b) (rate(foo[5m]))`, `sum(rate(foo[5m])) without (a)`,
		`topk(3, foo)`, `bottomk by (a) (2, foo)`, `quantile_over_time(0.9, foo[10m])`,
		`foo + bar`, `foo / on(a) bar`, `foo > ignoring(b) bar`, `foo > bool 3`, `1 + 2 * 3`, `-foo`,
		`2 ^ 3 ^ 2`, `foo and bar or baz unless qux`, `abs(foo)`, `round(foo, 0.5)`, `clamp(foo, 0, 1)`,
		`scalar(foo) * 2`, `vector(1)`, `time()`, `histogram_quantile`, `1e3`, `0x1f`, `Inf`, `-NaN`,
		`sum(foo,)`, `avg_over_time(foo{stat="p99"}[1m])`, `foo{a="b",}`, `{"foo"}`,
	}
	for _, q := range ok {
		if _, err := ParseExpr(q); err != nil {
			t.Errorf("ParseExpr(%q): %v", q, err)
		}
	}
	bad := map[string]string{
		`{}`:                               "at least one non-empty matcher",
		`{a=""}`:                           "at least one non-empty matcher",
		`rate(foo)`:                        "expected type range vector",
		`foo[5m:1m]`:                       "subqueries",
		`rate(foo[5m])[5m]`:                "subqueries", // reported via range on non-selector
		`foo @ 100`:                        "@ modifier",
		`histogram_quantile(0.9, x)`:       "unsupported: function histogram_quantile",
		`label_replace(x,"a","b","c","d")`: "unsupported",
		`nosuch(x)`:                        "unknown function",
		`foo * on(a) group_left bar`:       "group_left",
		`count_values("v", foo)`:           "unsupported: aggregation count_values",
		`1 > 2`:                            "BOOL modifier",
		`foo and 1`:                        "set operator",
		`sum(foo[5m])`:                     "expected type instant vector",
		`foo{a=~"("}`:                      "invalid regular expression",
		`foo +`:                            "unexpected end of input",
		`(foo`:                             "expected \")\"",
		`topk(foo)`:                        "expected 2",
		`sum(foo) offset 5m`:               "offset modifier",
		`foo{a="b"`:                        "",
	}
	for q, want := range bad {
		_, err := ParseExpr(q)
		if err == nil {
			t.Errorf("ParseExpr(%q): want error", q)
			continue
		}
		if q == `rate(foo[5m])[5m]` {
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseExpr(%q) = %v, want it to contain %q", q, err, want)
		}
	}
	// precedence
	e, _ := ParseExpr(`1 + 2 * 3`)
	if b := e.(*Binary); b.Op != "+" || b.RHS.(*Binary).Op != "*" {
		t.Errorf("precedence wrong: %#v", e)
	}
	e, _ = ParseExpr(`2 ^ 3 ^ 2`)
	if b := e.(*Binary); b.LHS.(NumberLit).V != 2 {
		t.Errorf("^ must be right associative")
	}
	e, _ = ParseExpr(`-2 ^ 2`)
	if u, ok := e.(*Unary); !ok || u.Expr.(*Binary).Op != "^" {
		t.Errorf("-2^2 must be -(2^2), got %#v", e)
	}
	e, _ = ParseExpr(`foo[1h30m] offset 5m`)
	if m := e.(*MatrixSel); m.Range != 90*time.Minute || m.VS.Offset != 5*time.Minute {
		t.Errorf("range/offset: %+v", m)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[[2]string]string{
		{"ZeroWatch/Host", "CPUUtilization"}: "ZeroWatch_Host_CPUUtilization",
		{"AWS/EC2", "CPU-Util.x"}:            "AWS_EC2_CPU_Util_x",
		{"9ns", "m:1"}:                       "_9ns_m:1",
	}
	for in, want := range cases {
		if got := SanitizeMetricName(in[0], in[1]); got != want {
			t.Errorf("SanitizeMetricName(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	if dimLabel("stat") != "dim_stat" || dimLabel("a:b") != "a_b" || dimLabel("Host") != "Host" {
		t.Error("dimLabel")
	}
}

// seeded store: base is a minute-aligned time two hours ago.
type fixture struct {
	h    *Handler
	base time.Time
	now  time.Time
}

func f64(v float64) *float64 { return &v }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	m := storage.NewMemory(storage.MemoryConfig{})
	t.Cleanup(func() { _ = m.Close() })
	now := time.Now().Truncate(time.Hour)
	base := now.Add(-2 * time.Hour)
	var ds []model.MetricDatum
	add := func(ns, metric string, dims map[string]string, ts time.Time, v float64) {
		var dl []model.Dimension
		for k, x := range dims {
			dl = append(dl, model.Dimension{Name: k, Value: x})
		}
		ds = append(ds, model.MetricDatum{TenantID: 1, Namespace: ns, MetricName: metric, Dimensions: dl, Timestamp: ts.UnixMilli(), Value: f64(v)})
	}
	for i := 0; i <= 120; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		// counter: +60 per minute (1/s) for api, +120 (2/s) for web
		add("App/RED", "Requests", map[string]string{"Service": "api"}, ts, float64(60*i))
		add("App/RED", "Requests", map[string]string{"Service": "web"}, ts, float64(120*i))
		// gauge: two samples per minute for stats
		add("App/RED", "Latency", map[string]string{"Service": "api"}, ts, 10)
		add("App/RED", "Latency", map[string]string{"Service": "api"}, ts.Add(30*time.Second), 30)
		add("App/RED", "Latency", map[string]string{"Service": "web"}, ts, 100)
	}
	// another tenant must stay invisible
	ds = append(ds, model.MetricDatum{TenantID: 2, Namespace: "App/RED", MetricName: "Requests", Timestamp: now.UnixMilli(), Value: f64(1)})
	if err := m.WriteMetrics(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Store: m, Now: func() time.Time { return now }}
	return &fixture{h: h, base: base, now: now}
}

func (f *fixture) instant(t *testing.T, q string, at time.Time) queryData {
	t.Helper()
	d, _, err := f.h.Instant(context.Background(), 1, q, at)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return d.(queryData)
}

func vecMap(t *testing.T, d queryData, key string) map[string]float64 {
	t.Helper()
	if d.ResultType != "vector" {
		t.Fatalf("resultType %s", d.ResultType)
	}
	out := map[string]float64{}
	for _, s := range d.Result.([]vecSample) {
		out[s.Metric[key]] = s.Value.V
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9*math.Max(1, math.Abs(b)) }

func TestEvalInstant(t *testing.T) {
	f := newFixture(t)
	at := f.base.Add(60 * time.Minute)
	cases := []struct {
		q    string
		want map[string]float64
	}{
		{`App_RED_Requests`, map[string]float64{"api": 3600, "web": 7200}},
		{`App_RED_Requests{Service="api"}`, map[string]float64{"api": 3600}},
		{`App_RED_Requests{Service=~"w.*"}`, map[string]float64{"web": 7200}},
		{`App_RED_Requests{Service!="web"}`, map[string]float64{"api": 3600}},
		{`App_RED_Requests offset 10m`, map[string]float64{"api": 3000, "web": 6000}},
		{`rate(App_RED_Requests[5m])`, map[string]float64{"api": 1, "web": 2}},
		{`irate(App_RED_Requests[5m])`, map[string]float64{"api": 1, "web": 2}},
		{`increase(App_RED_Requests[10m])`, map[string]float64{"api": 600, "web": 1200}},
		{`delta(App_RED_Requests[10m])`, map[string]float64{"api": 600, "web": 1200}},
		{`App_RED_Latency`, map[string]float64{"api": 20, "web": 100}},
		{`App_RED_Latency{stat="Maximum"}`, map[string]float64{"api": 30, "web": 100}},
		{`App_RED_Latency{stat="SampleCount", Service="api"}`, map[string]float64{"api": 2}},
		{`App_RED_Latency{stat="p99", Service="api"}`, map[string]float64{"api": 29.8}},
		{`avg_over_time(App_RED_Latency[5m])`, map[string]float64{"api": 20, "web": 100}},
		{`sum_over_time(App_RED_Latency[5m])`, map[string]float64{"api": 100, "web": 500}},
		{`count_over_time(App_RED_Latency[5m])`, map[string]float64{"api": 5, "web": 5}},
		{`max_over_time(App_RED_Requests[5m])`, map[string]float64{"api": 3600, "web": 7200}},
		{`min_over_time(App_RED_Requests[5m])`, map[string]float64{"api": 3360, "web": 6720}},
		{`quantile_over_time(0.5, App_RED_Requests[5m])`, map[string]float64{"api": 3480, "web": 6960}},
		{`App_RED_Requests / 60`, map[string]float64{"api": 60, "web": 120}},
		{`App_RED_Requests > 4000`, map[string]float64{"web": 7200}},
		{`4000 < App_RED_Requests`, map[string]float64{"web": 7200}},
		{`App_RED_Requests > bool 4000`, map[string]float64{"api": 0, "web": 1}},
		{`App_RED_Requests / App_RED_Latency`, map[string]float64{}}, // metric label differs
		{`App_RED_Requests / ignoring(metric) App_RED_Latency`, map[string]float64{"api": 180, "web": 72}},
		{`App_RED_Requests - on(Service) App_RED_Latency`, map[string]float64{"api": 3580, "web": 7100}},
		{`App_RED_Requests and App_RED_Latency > 50`, map[string]float64{}},
		{`App_RED_Requests and on(Service) App_RED_Latency > 50`, map[string]float64{"web": 7200}},
		{`App_RED_Requests unless on(Service) App_RED_Latency > 50`, map[string]float64{"api": 3600}},
		{`App_RED_Requests{Service="api"} or on(Service) App_RED_Latency`, map[string]float64{"api": 3600, "web": 100}},
		{`topk(1, App_RED_Requests)`, map[string]float64{"web": 7200}},
		{`bottomk(1, App_RED_Requests)`, map[string]float64{"api": 3600}},
		{`abs(-App_RED_Requests)`, map[string]float64{"api": 3600, "web": 7200}},
		{`clamp_max(App_RED_Requests, 5000)`, map[string]float64{"api": 3600, "web": 5000}},
		{`clamp_min(App_RED_Requests, 5000)`, map[string]float64{"api": 5000, "web": 7200}},
		{`clamp(App_RED_Requests, 4000, 5000)`, map[string]float64{"api": 4000, "web": 5000}},
		{`round(App_RED_Latency / 7)`, map[string]float64{"api": 3, "web": 14}},
		{`ceil(App_RED_Latency / 7)`, map[string]float64{"api": 3, "web": 15}},
		{`floor(App_RED_Latency / 7)`, map[string]float64{"api": 2, "web": 14}},
		{`App_RED_Requests * scalar(App_RED_Latency{Service="web"})`, map[string]float64{"api": 360000, "web": 720000}},
		{`absent(nosuch{Service="x"})`, map[string]float64{"x": 1}},
		{`absent(App_RED_Requests)`, map[string]float64{}},
	}
	for _, c := range cases {
		got := vecMap(t, f.instant(t, c.q, at), "Service")
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.q, got, c.want)
			continue
		}
		for k, v := range c.want {
			if !near(got[k], v) {
				t.Errorf("%s: got %v, want %v", c.q, got, c.want)
				break
			}
		}
	}
	aggs := map[string]map[string]float64{
		`sum(App_RED_Requests)`:                          {"": 10800},
		`sum by (Service) (rate(App_RED_Requests[5m]))`:  {"api": 1, "web": 2},
		`avg(App_RED_Requests)`:                          {"": 5400},
		`min(App_RED_Requests)`:                          {"": 3600},
		`max(App_RED_Requests)`:                          {"": 7200},
		`count(App_RED_Requests)`:                        {"": 2},
		`count(App_RED_Latency{stat!="Average"})`:        {"": 16},
		`sum without (Service, stat) (App_RED_Requests)`: {"": 10800},
		`stddev(App_RED_Requests)`:                       {"": 1800},
		`quantile(0.5, App_RED_Requests)`:                {"": 5400},
		`group(App_RED_Requests)`:                        {"": 1},
	}
	for q, want := range aggs {
		got := vecMap(t, f.instant(t, q, at), "Service")
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", q, got, want)
			continue
		}
		for k, v := range want {
			if !near(got[k], v) {
				t.Errorf("%s: got %v, want %v", q, got, want)
			}
		}
	}
	// labels: name, namespace, metric, dimension and stat; functions drop __name__
	d := f.instant(t, `App_RED_Requests{Service="api"}`, at)
	m := d.Result.([]vecSample)[0].Metric
	want := map[string]string{"__name__": "App_RED_Requests", "namespace": "App/RED", "metric": "Requests", "Service": "api", "stat": "Average"}
	if labelKey(m) != labelKey(want) {
		t.Errorf("labels %v, want %v", m, want)
	}
	if _, has := f.instant(t, `rate(App_RED_Requests{Service="api"}[5m])`, at).Result.([]vecSample)[0].Metric["__name__"]; has {
		t.Error("rate must drop __name__")
	}
	if n := f.instant(t, `App_RED_Requests > 0`, at).Result.([]vecSample)[0].Metric["__name__"]; n != "App_RED_Requests" {
		t.Error("filtering comparison must keep __name__")
	}
	// scalar results
	d = f.instant(t, `1 + 2 * 3`, at)
	if d.ResultType != "scalar" || d.Result.(sample).V != 7 {
		t.Errorf("scalar: %+v", d)
	}
	d = f.instant(t, `2 ^ 3 ^ 2`, at)
	if d.Result.(sample).V != 512 {
		t.Errorf("^ assoc: %+v", d)
	}
	d = f.instant(t, `time()`, at)
	if d.Result.(sample).V != float64(at.Unix()) {
		t.Errorf("time(): %+v", d)
	}
	// range vector at top level of an instant query is a matrix of raw samples
	d = f.instant(t, `App_RED_Requests{Service="api"}[3m]`, at)
	if d.ResultType != "matrix" || len(d.Result.([]matSeries)[0].Values) != 3 {
		t.Errorf("matrix: %+v", d)
	}
	// lookback: 5 m after the last sample there is still a value, 6 m after not
	end := f.base.Add(120 * time.Minute)
	if len(f.instant(t, `App_RED_Requests`, end.Add(4*time.Minute)).Result.([]vecSample)) != 2 {
		t.Error("lookback: want 2 series 4m after the last sample")
	}
	if len(f.instant(t, `App_RED_Requests`, end.Add(6*time.Minute)).Result.([]vecSample)) != 0 {
		t.Error("lookback: want none 6m after the last sample")
	}
	// tenants are isolated
	d2, _, _ := f.h.Instant(context.Background(), 2, `App_RED_Requests`, f.now)
	if len(d2.(queryData).Result.([]vecSample)) != 1 {
		t.Error("tenant 2 must see only its series")
	}
	// errors
	errs := map[string]string{
		`App_RED_Requests + on(Service) App_RED_Requests{stat=~"Sum|Average"}`: "",
		`App_RED_Latency{stat="bogus"}`:                                        "invalid stat",
		`sum(App_RED_Requests) by (metric) / App_RED_Requests`:                 "",
	}
	for q, want := range errs {
		_, _, err := f.h.Instant(context.Background(), 1, q, at)
		if q == `App_RED_Requests + on(Service) App_RED_Requests{stat=~"Sum|Average"}` {
			// rhs has Sum and Average per Service: duplicate match group
			if err == nil || !strings.Contains(err.Error(), "duplicate series") {
				t.Errorf("%s: want many-to-many error, got %v", q, err)
			}
			continue
		}
		if q == `sum(App_RED_Requests) by (metric) / App_RED_Requests` {
			if err != nil {
				t.Errorf("%s: %v", q, err) // no match: empty result, no error
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", q, err, want)
		}
	}
}

func TestEvalRange(t *testing.T) {
	f := newFixture(t)
	start := f.base.Add(30*time.Minute + 20*time.Second) // aligned down to 30m
	end := f.base.Add(40 * time.Minute)
	d, _, err := f.h.Range(context.Background(), 1, `sum(rate(App_RED_Requests[5m]))`, start, end, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	q := d.(queryData)
	ms := q.Result.([]matSeries)
	if q.ResultType != "matrix" || len(ms) != 1 || len(ms[0].Values) != 11 {
		t.Fatalf("got %+v", q)
	}
	if ms[0].Values[0].T != f.base.Add(30*time.Minute).UnixMilli() {
		t.Errorf("start not aligned: %d", ms[0].Values[0].T)
	}
	for _, v := range ms[0].Values {
		if !near(v.V, 3) {
			t.Errorf("rate sum = %v, want 3", v.V)
		}
	}
	// scalar range query becomes one series with no labels
	d, _, err = f.h.Range(context.Background(), 1, `1`, start, end, time.Minute)
	if err != nil || len(d.(queryData).Result.([]matSeries)) != 1 || len(d.(queryData).Result.([]matSeries)[0].Metric) != 0 {
		t.Errorf("scalar range: %+v %v", d, err)
	}
	// topk per step
	d, _, _ = f.h.Range(context.Background(), 1, `topk(1, App_RED_Requests)`, start, end, time.Minute)
	if ms := d.(queryData).Result.([]matSeries); len(ms) != 1 || ms[0].Metric["Service"] != "web" {
		t.Errorf("topk range: %+v", ms)
	}
	// 11,000 points
	if _, _, err := f.h.Range(context.Background(), 1, `1`, start, start.Add(11001*time.Second), time.Second); err == nil || !strings.Contains(err.Error(), "11,000") {
		t.Errorf("want max resolution error, got %v", err)
	}
	if _, _, err := f.h.Range(context.Background(), 1, `foo[5m]`, start, end, time.Minute); err == nil {
		t.Error("range query of a range vector must fail")
	}
	if _, _, err := f.h.Range(context.Background(), 1, `1`, end, start, time.Minute); err == nil {
		t.Error("end before start must fail")
	}
}

func get(t *testing.T, srv *httptest.Server, path string, form url.Values) (int, map[string]any, string) {
	t.Helper()
	var resp *http.Response
	var err error
	if form != nil {
		resp, err = http.PostForm(srv.URL+path, form)
	} else {
		resp, err = http.Get(srv.URL + path)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

func TestHTTP(t *testing.T) {
	f := newFixture(t)
	mux := http.NewServeMux()
	f.h.Register(mux, "/prometheus")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	at := f.base.Add(60 * time.Minute).Unix()
	ts := func(s int64) string { return url.QueryEscape(time.Unix(s, 0).UTC().Format(time.RFC3339)) }

	// instant vector, GET with RFC 3339 time
	code, _, raw := get(t, srv, "/prometheus/api/v1/query?query=App_RED_Requests%7BService%3D%22api%22%7D&time="+ts(at), nil)
	want := `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"Service":"api","__name__":"App_RED_Requests","metric":"Requests","namespace":"App/RED","stat":"Average"},"value":[` +
		itoa(at) + `,"3600"]}]}}`
	if code != 200 || raw != want {
		t.Errorf("vector:\n got %s\nwant %s", raw, want)
	}
	// scalar via POST form, fractional time
	code, _, raw = get(t, srv, "/prometheus/api/v1/query", url.Values{"query": {"1+1"}, "time": {"1700000000.5"}})
	if code != 200 || raw != `{"status":"success","data":{"resultType":"scalar","result":[1700000000.5,"2"]}}` {
		t.Errorf("scalar: %d %s", code, raw)
	}
	// special values
	_, _, raw = get(t, srv, "/prometheus/api/v1/query", url.Values{"query": {"1/0"}, "time": {"100"}})
	if raw != `{"status":"success","data":{"resultType":"scalar","result":[100,"+Inf"]}}` {
		t.Errorf("+Inf: %s", raw)
	}
	// range matrix
	code, m, raw := get(t, srv, "/prometheus/api/v1/query_range", url.Values{
		"query": {`sum by (Service) (rate(App_RED_Requests[5m]))`}, "start": {itoa(at)}, "end": {itoa(at + 120)}, "step": {"60"}})
	if code != 200 {
		t.Fatalf("range: %d %s", code, raw)
	}
	data := m["data"].(map[string]any)
	res := data["result"].([]any)
	if data["resultType"] != "matrix" || len(res) != 2 {
		t.Fatalf("range: %s", raw)
	}
	first := res[0].(map[string]any)
	if lbl := first["metric"].(map[string]any); len(lbl) != 1 || lbl["Service"] != "api" {
		t.Errorf("range labels: %v", lbl)
	}
	vals := first["values"].([]any)
	if len(vals) != 3 || vals[0].([]any)[1] != "1" || vals[0].([]any)[0].(float64) != float64(at) {
		t.Errorf("range values: %v", vals)
	}
	// step as duration
	code, _, _ = get(t, srv, "/prometheus/api/v1/query_range?query=1&start="+itoa(at)+"&end="+itoa(at+600)+"&step=5m", nil)
	if code != 200 {
		t.Errorf("duration step: %d", code)
	}
	// errors: bad_data with 400
	code, m, _ = get(t, srv, "/prometheus/api/v1/query?query=histogram_quantile(0.9,x)", nil)
	if code != 400 || m["status"] != "error" || m["errorType"] != "bad_data" || !strings.Contains(m["error"].(string), "unsupported") {
		t.Errorf("unsupported: %d %v", code, m)
	}
	code, m, _ = get(t, srv, "/prometheus/api/v1/query_range?query=1&start=10&end=20000&step=1", nil)
	if code != 400 || !strings.Contains(m["error"].(string), "11,000") {
		t.Errorf("max points: %d %v", code, m)
	}
	code, m, _ = get(t, srv, "/prometheus/api/v1/query", url.Values{"query": {`App_RED_Requests + on(Service) App_RED_Requests{stat=~"Sum|Average"}`}, "time": {itoa(at)}})
	if code != 422 || m["errorType"] != "execution" {
		t.Errorf("execution error: %d %v", code, m)
	}
	code, m, _ = get(t, srv, "/prometheus/api/v1/query?query=1&time=notatime", nil)
	if code != 400 || m["errorType"] != "bad_data" {
		t.Errorf("bad time: %d %v", code, m)
	}
	// series
	code, m, raw = get(t, srv, "/prometheus/api/v1/series?match[]=App_RED_Latency", nil)
	if code != 200 || len(m["data"].([]any)) != 2 {
		t.Errorf("series: %s", raw)
	}
	code, m, _ = get(t, srv, "/prometheus/api/v1/series", nil)
	if code != 400 || m["error"] != "no match[] parameter provided" {
		t.Errorf("series without match: %d %v", code, m)
	}
	// labels
	_, m, raw = get(t, srv, "/prometheus/api/v1/labels", nil)
	if got := toStrings(m["data"]); strings.Join(got, ",") != "Service,__name__,metric,namespace,stat" {
		t.Errorf("labels: %s", raw)
	}
	_, m, raw = get(t, srv, "/prometheus/api/v1/label/__name__/values", nil)
	if got := toStrings(m["data"]); strings.Join(got, ",") != "App_RED_Latency,App_RED_Requests" {
		t.Errorf("names: %s", raw)
	}
	_, m, _ = get(t, srv, "/prometheus/api/v1/label/Service/values?match[]=App_RED_Latency%7BService%3D%22web%22%7D", nil)
	if got := toStrings(m["data"]); strings.Join(got, ",") != "web" {
		t.Errorf("values with match: %v", got)
	}
	_, m, _ = get(t, srv, "/prometheus/api/v1/label/stat/values", nil)
	if got := toStrings(m["data"]); len(got) != len(baseStats) {
		t.Errorf("stat values: %v", got)
	}
	// metadata, buildinfo, exemplars
	_, m, raw = get(t, srv, "/prometheus/api/v1/metadata", nil)
	if md := m["data"].(map[string]any); len(md) != 2 || md["App_RED_Requests"].([]any)[0].(map[string]any)["type"] != "gauge" {
		t.Errorf("metadata: %s", raw)
	}
	_, m, _ = get(t, srv, "/prometheus/api/v1/status/buildinfo", nil)
	if m["data"].(map[string]any)["version"] == "" {
		t.Error("buildinfo")
	}
	_, _, raw = get(t, srv, "/prometheus/api/v1/query_exemplars?query=x", nil)
	if raw != `{"status":"success","data":[]}` {
		t.Errorf("exemplars: %s", raw)
	}
	// empty vector is [] not null
	_, _, raw = get(t, srv, "/prometheus/api/v1/query?query=nosuch&time=100", nil)
	if raw != `{"status":"success","data":{"resultType":"vector","result":[]}}` {
		t.Errorf("empty: %s", raw)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
