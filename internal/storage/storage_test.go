package storage

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/query"
)

// Quoted terms keep their spaces, so attribute values like routes
// ("GET /health") can be filtered as key=value. Mirrored by
// web/src/lib/live.ts filterTerms.
func TestTermsQuoting(t *testing.T) {
	cases := map[string][]string{
		`timeout service=Checkout`:        {"timeout", "service=checkout"},
		`route="GET /health" -status=500`: {"route=get /health", "-status=500"},
		`"connection reset"  peer`:        {"connection reset", "peer"},
		`-route="POST /api"`:              {"-route=post /api"},
		`unclosed "quote runs to the end`: {"unclosed", "quote runs to the end"},
		"  ":                              nil,
	}
	for in, want := range cases {
		got := Terms(in)
		if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("Terms(%q) = %q, want %q", in, got, want)
		}
	}
	e := &model.LogEvent{TenantID: 1, Message: "GET /health completed", Attributes: map[string]string{"route": "GET /health", "status": "200"}}
	q := &LogQuery{TenantID: 1}
	if !MatchLog(q, Terms(`route="GET /health"`), e) {
		t.Error(`route="GET /health" should match`)
	}
	if MatchLog(q, Terms(`-route="GET /health"`), e) {
		t.Error(`-route="GET /health" should exclude`)
	}
	if MatchLog(q, Terms(`route=GET /health`), e) {
		t.Error("unquoted route=GET splits into two terms and must not match")
	}
}

func datum(svc string, t int64, v float64) model.MetricDatum {
	return model.MetricDatum{TenantID: 1, Namespace: "App", MetricName: "Latency", Dimensions: []model.Dimension{{Name: "Service", Value: svc}}, Timestamp: t, Value: model.Float(v)}
}

func TestParseStat(t *testing.T) {
	for _, s := range []string{"Average", "Sum", "Minimum", "Maximum", "SampleCount", "p99", "p99.9"} {
		if _, err := ParseStat(s); err != nil {
			t.Fatal(s, err)
		}
	}
	if _, err := ParseStat("p101"); err == nil {
		t.Fatal("p101")
	}
	if v := (Stat{Kind: "pct", Pct: 50}).Reduce([]float64{1, 2, 3, 4, 5}); v != 3 {
		t.Fatal(v)
	}
}

// FetchRaw returns the same samples FetchStat reduces, so a cluster
// coordinator merging shards gets exact percentiles.
func TestMemoryFetchRaw(t *testing.T) {
	m := NewMemory(MemoryConfig{})
	defer m.Close()
	ctx := context.Background()
	base := int64(1_700_000_000_000) / 60_000 * 60_000
	var ds []model.MetricDatum
	for i := 0; i < 120; i++ { // 2 minutes @ 1s, two services
		ds = append(ds, datum("api", base+int64(i)*1000, float64(i)), datum("web", base+int64(i)*1000, 1000))
	}
	if err := m.WriteMetrics(ctx, ds); err != nil {
		t.Fatal(err)
	}
	ms := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Service": "api"}, Stat: "p99"}
	raw, truncated, err := m.FetchRaw(ctx, 1, ms, base, base+120_000, 60, 0)
	if err != nil || truncated || len(raw) != 2 || len(raw[base]) != 60 || len(raw[base+60_000]) != 60 {
		t.Fatalf("raw: buckets=%d first=%d truncated=%v err=%v", len(raw), len(raw[base]), truncated, err)
	}
	pts, _ := m.FetchStat(ctx, 1, ms, base, base+120_000, 60)
	st, _ := ParseStat("p99")
	if got := st.Reduce(append([]float64(nil), raw[base]...)); got != pts[0].V {
		t.Fatalf("p99 from raw %v != FetchStat %v", got, pts[0].V)
	}
	// Exact dimension matching: {Service} alone matches nothing extra; "*" matches both.
	ms.Dimensions = map[string]string{"Service": "*"}
	raw, _, _ = m.FetchRaw(ctx, 1, ms, base, base+60_000, 60, 0)
	if len(raw[base]) != 120 {
		t.Fatalf("wildcard: %d values, want 120", len(raw[base]))
	}
	raw, truncated, _ = m.FetchRaw(ctx, 1, ms, base, base+120_000, 60, 50)
	n := 0
	for _, v := range raw {
		n += len(v)
	}
	if !truncated || n != 50 {
		t.Fatalf("limit: %d values, truncated=%v", n, truncated)
	}
	if _, _, err := m.FetchRaw(ctx, 1, ms, base, base+60_000, 0, 0); err == nil {
		t.Fatal("period 0 must be refused")
	}
}

func TestMemoryFetchRollupAndRaw(t *testing.T) {
	m := NewMemory(MemoryConfig{})
	defer m.Close()
	var ds []model.MetricDatum
	base := int64(1_700_000_000_000) / 300000 * 300000
	for i := 0; i < 600; i++ { // 10 minutes @ 1s
		ds = append(ds, datum("api", base+int64(i)*1000, float64(i%60)))
		ds = append(ds, datum("web", base+int64(i)*1000, 100))
	}
	if err := m.WriteMetrics(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ms := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Service": "api"}, Stat: "Average"}
	pts, err := m.FetchStat(ctx, 1, ms, base, base+600_000, 60)
	if err != nil || len(pts) != 10 {
		t.Fatalf("rollup: %v %v", len(pts), err)
	}
	if pts[0].V != 29.5 || pts[0].T != base {
		t.Fatalf("avg %v", pts[0])
	}
	ms.Stat = "Maximum"
	pts, _ = m.FetchStat(ctx, 1, ms, base, base+600_000, 300)
	if len(pts) != 2 || pts[0].V != 59 {
		t.Fatalf("max %v", pts)
	}
	ms.Stat = "p50"
	pts, _ = m.FetchStat(ctx, 1, ms, base, base+60_000, 10)
	if len(pts) != 6 || pts[0].V != 4.5 {
		t.Fatalf("raw pct %v", pts)
	}
	// wildcard aggregates across services
	ms.Stat, ms.Dimensions = "SampleCount", map[string]string{"Service": "*"}
	pts, _ = m.FetchStat(ctx, 1, ms, base, base+60_000, 60)
	if len(pts) != 1 || pts[0].V != 120 {
		t.Fatalf("wildcard %v", pts)
	}
	// exact match: no dims => no series
	ms.Dimensions = nil
	if pts, _ = m.FetchStat(ctx, 1, ms, base, base+60_000, 60); len(pts) != 0 {
		t.Fatal("exact dims match violated")
	}
	// tenant isolation
	ms.Dimensions = map[string]string{"Service": "api"}
	if pts, _ = m.FetchStat(ctx, 2, ms, base, base+60_000, 60); len(pts) != 0 {
		t.Fatal("tenant leak")
	}
	st := m.Stats()
	if st.Series != 2 || st.BytesPerPt <= 0 || st.BytesPerPt > 4 {
		t.Fatalf("stats %+v", st)
	}
	series, _ := m.ListSeries(ctx, 1, SeriesFilter{Dimensions: map[string]string{"Service": "web"}})
	if len(series) != 1 {
		t.Fatalf("list %v", series)
	}
}

func TestStatisticSetAndRetention(t *testing.T) {
	m := NewMemory(MemoryConfig{RawRetention: time.Hour, RollupRetention: 2 * time.Hour, MaxSeries: 1})
	defer m.Close()
	now := time.UnixMilli(10 * 3600_000)
	m.now = func() time.Time { return now }
	d := model.MetricDatum{TenantID: 1, Namespace: "n", MetricName: "m", Timestamp: now.UnixMilli() - 90*60_000,
		StatisticValues: &model.StatisticSet{SampleCount: 4, Sum: 40, Minimum: 1, Maximum: 20}}
	_ = m.WriteMetrics(context.Background(), []model.MetricDatum{d})
	pts, _ := m.FetchStat(context.Background(), 1, query.MetricStat{Namespace: "n", MetricName: "m", Stat: "Sum"}, 0, now.UnixMilli(), 60)
	if len(pts) != 1 || pts[0].V != 40 {
		t.Fatalf("%v", pts)
	}
	if err := m.WriteMetrics(context.Background(), []model.MetricDatum{datum("x", 1, 1)}); err != ErrCardinality {
		t.Fatalf("want cardinality err, got %v", err)
	}
	m.Compact() // raw expired (90m > 1h) but rollup kept
	pts, _ = m.FetchStat(context.Background(), 1, query.MetricStat{Namespace: "n", MetricName: "m", Stat: "p50"}, 0, now.UnixMilli(), 60)
	if len(pts) != 0 {
		t.Fatal("raw should be expired")
	}
	now = now.Add(time.Hour)
	m.Compact()
	if m.Stats().Series != 0 {
		t.Fatal("series should be dropped")
	}
}

func TestLogs(t *testing.T) {
	m := NewMemory(MemoryConfig{MaxLogEvents: 5})
	defer m.Close()
	var ls []model.LogEvent
	for i := 0; i < 8; i++ {
		lvl := "INFO"
		if i%2 == 0 {
			lvl = "ERROR"
		}
		ls = append(ls, model.LogEvent{TenantID: 1, LogGroup: "g", LogStream: "s", Timestamp: int64(i), Level: lvl,
			Message: "request done path=/api/" + string(rune('a'+i)), Attributes: map[string]string{"svc": "api"}})
	}
	_ = m.WriteLogs(context.Background(), ls)
	r, _ := m.SearchLogs(context.Background(), LogQuery{TenantID: 1})
	if len(r.Events) != 5 || r.Events[0].Timestamp != 7 {
		t.Fatalf("ring/newest-first broken %+v", r.Events)
	}
	r, _ = m.SearchLogs(context.Background(), LogQuery{TenantID: 1, Query: "level=error svc=API -/api/e"})
	if len(r.Events) != 1 || r.Events[0].Timestamp != 6 {
		t.Fatalf("filter %+v", r.Events)
	}
	g, _ := m.ListLogGroups(context.Background(), 1)
	if len(g) != 1 || g[0].Events != 5 {
		t.Fatalf("%+v", g)
	}
}

func TestClickHouseHTTP(t *testing.T) {
	var gotSQL, gotParams string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotSQL, gotParams = string(b), r.URL.RawQuery
		_, _ = w.Write([]byte("{\"t\":60000,\"v\":1.5}\n{\"t\":120000,\"v\":2.5}\n"))
	}))
	defer srv.Close()
	ch := NewClickHouse(srv.URL, "zw", "", "")
	pts, err := ch.FetchStat(context.Background(), 3, query.MetricStat{Namespace: "App", MetricName: "Lat'; DROP", Dimensions: map[string]string{"Service": "api"}, Stat: "p99"}, 0, 180000, 60)
	if err != nil || len(pts) != 2 || pts[1].V != 2.5 {
		t.Fatalf("%v %v", pts, err)
	}
	if strings.Contains(gotSQL, "DROP") || !strings.Contains(gotSQL, "quantileExactInclusive(0.99)") || !strings.Contains(gotParams, "param_metric=Lat") {
		t.Fatalf("injection-safe params expected: %s | %s", gotSQL, gotParams)
	}
	sql, _, _ := BuildFetchSQL(1, query.MetricStat{Namespace: "a", MetricName: "b", Dimensions: map[string]string{"x": "*"}}, 0, 1, 60)
	if !strings.Contains(sql, "mapContains") || math.IsNaN(0) {
		t.Fatal(sql)
	}
}
