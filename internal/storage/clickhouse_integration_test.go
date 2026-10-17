//go:build integration

package storage

// Live ClickHouse tests: every query runs against the same data in the
// memory engine and the results must agree. Run with
//
//	ZW_TEST_CLICKHOUSE_URL=http://zw:zw@localhost:8123 go test -tags integration ./internal/storage
//
// (deploy/compose/README.md). Each test uses its own throwaway database.

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
)

func newTestClickHouse(t *testing.T) *ClickHouse {
	t.Helper()
	u := os.Getenv("ZW_TEST_CLICKHOUSE_URL")
	if u == "" {
		t.Skip("ZW_TEST_CLICKHOUSE_URL not set")
	}
	db := fmt.Sprintf("zw_it_%s_%d", strings.ToLower(strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())), time.Now().UnixNano())
	ch := NewClickHouse(u, db, "", "")
	ctx := context.Background()
	if err := ch.Ping(ctx); err != nil {
		t.Fatalf("clickhouse unreachable at %s: %v", u, err)
	}
	t.Cleanup(func() {
		_ = ch.execDB(context.Background(), "default", "DROP DATABASE IF EXISTS "+quoteIdent(db), nil, nil)
		ch.Close()
	})
	return ch
}

func TestClickHouseEnsureSchemaIdempotentAndUpgrade(t *testing.T) {
	ch := newTestClickHouse(t)
	ctx := context.Background()
	// Tables as the first schema version created them (no unit/stat_*/level).
	if err := ch.execDB(ctx, "default", "CREATE DATABASE "+quoteIdent(ch.db), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE metrics_raw (tenant_id UInt32, series_id UInt64, timestamp DateTime64(3, 'UTC'), value Float64," +
			" namespace LowCardinality(String), metric_name LowCardinality(String), dimensions Map(LowCardinality(String), String))" +
			" ENGINE = MergeTree() ORDER BY (tenant_id, namespace, metric_name, series_id, timestamp)",
		"CREATE TABLE logs (tenant_id UInt32, log_group LowCardinality(String), log_stream String, timestamp DateTime64(3, 'UTC')," +
			" message String, attributes Map(String, String)) ENGINE = MergeTree() ORDER BY (tenant_id, log_group, timestamp, log_stream)",
		"INSERT INTO metrics_raw (tenant_id, series_id, timestamp, value, namespace, metric_name, dimensions) VALUES (1, 7, now64(3), 2.5, 'n', 'm', map())",
	} {
		if err := ch.exec(ctx, s, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := ch.EnsureSchema(ctx); err != nil {
			t.Fatalf("EnsureSchema run %d: %v", i+1, err)
		}
	}
	// The old row reads through the new columns' defaults.
	pts, err := ch.FetchStat(ctx, 1, query.MetricStat{Namespace: "n", MetricName: "m", Stat: "Sum"}, time.Now().Add(-time.Hour).UnixMilli(), time.Now().Add(time.Hour).UnixMilli(), 3600)
	if err != nil || len(pts) != 1 || pts[0].V != 2.5 {
		t.Fatalf("upgraded row: %v %v", pts, err)
	}
	ms := query.MetricStat{Namespace: "n", MetricName: "m", Stat: "SampleCount"}
	if pts, _ = ch.FetchStat(ctx, 1, ms, time.Now().Add(-time.Hour).UnixMilli(), time.Now().Add(time.Hour).UnixMilli(), 3600); len(pts) != 1 || pts[0].V != 1 {
		t.Fatalf("stat_count default: %v", pts)
	}
	// A fresh database also takes repeated EnsureSchema.
	fresh := newTestClickHouse(t)
	for i := 0; i < 2; i++ {
		if err := fresh.EnsureSchema(ctx); err != nil {
			t.Fatalf("fresh EnsureSchema run %d: %v", i+1, err)
		}
	}
}

// both writes the same data to a memory engine and a fresh ClickHouse database.
type both struct {
	mem *Memory
	ch  *ClickHouse
}

func newBoth(t *testing.T) both {
	ch := newTestClickHouse(t)
	if err := ch.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	mem := NewMemory(MemoryConfig{})
	t.Cleanup(func() { mem.Close() })
	return both{mem, ch}
}

func (b both) writeMetrics(t *testing.T, ds []model.MetricDatum) {
	t.Helper()
	for _, e := range []Engine{b.mem, b.ch} {
		if err := e.WriteMetrics(context.Background(), ds); err != nil {
			t.Fatal(err)
		}
	}
}

func (b both) writeLogs(t *testing.T, ls []model.LogEvent) {
	t.Helper()
	for _, e := range []Engine{b.mem, b.ch} {
		if err := e.WriteLogs(context.Background(), ls); err != nil {
			t.Fatal(err)
		}
	}
}

func dims(kv ...string) []model.Dimension {
	var out []model.Dimension
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, model.Dimension{Name: kv[i], Value: kv[i+1]})
	}
	return out
}

// metricFixture: 10 minutes at 1 s (plus a few off-grid milliseconds) for
// several dimension sets, StatisticSets, and a second tenant. Values are
// multiples of 0.5 so sums are exact in any summation order.
func metricFixture(base int64) []model.MetricDatum {
	var ds []model.MetricDatum
	add := func(tenant uint32, metric string, d []model.Dimension, ts int64, v float64) {
		ds = append(ds, model.MetricDatum{TenantID: tenant, Namespace: "App", MetricName: metric, Dimensions: d, Timestamp: ts, Value: model.Float(v), Unit: "Milliseconds"})
	}
	for i := int64(0); i < 600; i++ {
		ts := base + i*1000
		add(1, "Latency", dims("Service", "api", "Route", "/a"), ts, float64(i%60)*0.5)
		add(1, "Latency", dims("Service", "api", "Route", "/b"), ts+137, float64((i*7)%100))
		add(1, "Latency", dims("Service", "web", "Route", "/a"), ts+999, 100)
		add(1, "Latency", dims("Service", "api"), ts, float64(i%10))
		add(1, "Latency", dims("Service", "api", "Zone", "z"), ts, 3)
		if i%3 == 0 {
			add(1, "Latency", nil, ts+1, 1.5)
		}
		add(2, "Latency", dims("Service", "api", "Route", "/a"), ts, 1000+float64(i))
	}
	for m := int64(0); m < 10; m++ {
		ts := base + m*60_000 + 30_000
		ds = append(ds, model.MetricDatum{TenantID: 1, Namespace: "App", MetricName: "Batch", Dimensions: dims("Service", "api"), Timestamp: ts,
			StatisticValues: &model.StatisticSet{SampleCount: 4, Sum: 40 + float64(m), Minimum: 1, Maximum: 20 + float64(m)}})
		ds = append(ds, model.MetricDatum{TenantID: 1, Namespace: "App", MetricName: "Batch", Dimensions: dims("Service", "api"), Timestamp: ts + 5000, Value: model.Float(float64(m))})
	}
	// Hostile names and values must round-trip as data, not SQL.
	add(1, "We'ird`{x:String}", dims("k'`); DROP TABLE logs; --", "v' OR 1=1 --", "ünï", "{dv0:String}"), base, 42)
	return ds
}

func samePoints(a, b []model.Point, exact bool) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].T != b[i].T {
			return false, fmt.Sprintf("point %d: t %d vs %d", i, a[i].T, b[i].T)
		}
		x, y := a[i].V, b[i].V
		if x == y || math.IsNaN(x) && math.IsNaN(y) {
			continue
		}
		if exact || math.Abs(x-y) > 1e-9*math.Max(1, math.Abs(x)) {
			return false, fmt.Sprintf("point %d (t=%d): memory %v vs clickhouse %v", i, a[i].T, x, y)
		}
	}
	return true, ""
}

func TestClickHouseFetchStatParity(t *testing.T) {
	b := newBoth(t)
	ctx := context.Background()
	base := query.AlignTimestamp(time.Now().Add(-2*time.Hour).UnixMilli(), 300_000)
	b.writeMetrics(t, metricFixture(base))

	statsList := []string{"Average", "Sum", "Minimum", "Maximum", "SampleCount", "p0", "p50", "p90", "p99", "p99.9", "p100"}
	dimQueries := []map[string]string{
		{"Service": "api", "Route": "/a"},
		{"Service": "api", "Route": "*"},
		{"Service": "*", "Route": "*"},
		{"Service": "*", "Route": "/a"},
		{"Service": "api"},
		{"Service": "*"},
		nil,
		{"Service": "api", "Route": ""}, // key must be present: must not match {Service, Zone}
		{"Service": "api", "Zone": "*"},
		{"Service": "API", "Route": "/a"}, // values are case-sensitive
		{"Nope": "*"},
		{"k'`); DROP TABLE logs; --": "v' OR 1=1 --", "ünï": "{dv0:String}"},
	}
	windows := [][2]int64{{base, base + 600_000}, {base + 30_500, base + 421_234}, {base - 3_600_000, base + 3_600_000}, {base + 700_000, base + 800_000}}
	periods := []int{1, 10, 60, 300}
	metrics := []string{"Latency", "Batch", "We'ird`{x:String}"}

	var checked, nonEmpty, inexact int
	for _, tenant := range []uint32{1, 2, 3} {
		for _, metric := range metrics {
			for _, dq := range dimQueries {
				for _, w := range windows {
					for _, p := range periods {
						for _, s := range statsList {
							ms := query.MetricStat{Namespace: "App", MetricName: metric, Dimensions: dq, Stat: s}
							mp, err := b.mem.FetchStat(ctx, tenant, ms, w[0], w[1], p)
							if err != nil {
								t.Fatal(err)
							}
							cp, err := b.ch.FetchStat(ctx, tenant, ms, w[0], w[1], p)
							if err != nil {
								t.Fatalf("clickhouse %s %v %s p=%d: %v", metric, dq, s, p, err)
							}
							exactKind := s == "Sum" || s == "Minimum" || s == "Maximum" || s == "SampleCount"
							ok, why := samePoints(mp, cp, exactKind)
							if !ok {
								t.Errorf("tenant=%d %s dims=%v %s period=%d window=[%d,%d): %s", tenant, metric, dq, s, p, w[0]-base, w[1]-base, why)
							} else if bitOK, _ := samePoints(mp, cp, true); !bitOK {
								inexact++
							}
							checked++
							if len(mp) > 0 {
								nonEmpty++
							}
						}
					}
				}
			}
		}
	}
	if nonEmpty < 500 {
		t.Fatalf("fixture too sparse: only %d of %d queries returned data", nonEmpty, checked)
	}
	t.Logf("%d FetchStat queries compared (%d with data); %d agree only within 1e-9 relative, the rest bit-for-bit", checked, nonEmpty, inexact)

	// Spot checks that pin the semantics, not only parity.
	ms := query.MetricStat{Namespace: "App", MetricName: "Batch", Dimensions: map[string]string{"Service": "api"}, Stat: "Sum"}
	pts, _ := b.ch.FetchStat(ctx, 1, ms, base, base+60_000, 60)
	if len(pts) != 1 || pts[0].V != 40 {
		t.Fatalf("StatisticSet sum + value: %v", pts)
	}
	ms.Stat = "SampleCount"
	if pts, _ = b.ch.FetchStat(ctx, 1, ms, base, base+60_000, 60); len(pts) != 1 || pts[0].V != 5 {
		t.Fatalf("StatisticSet count: %v", pts)
	}
	ms = query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Service": "api", "Route": "/a"}, Stat: "p50"}
	if pts, _ = b.ch.FetchStat(ctx, 1, ms, base, base+10_000, 10); len(pts) != 1 || pts[0].V != 2.25 {
		t.Fatalf("p50 of 0..4.5 step .5 = 2.25 (R-7), got %v", pts)
	}
	if _, err := b.ch.FetchStat(ctx, 1, ms, base, base+10_000, 0); err == nil {
		t.Fatal("period 0 must be rejected")
	}
}

func TestClickHouseListSeriesParity(t *testing.T) {
	b := newBoth(t)
	ctx := context.Background()
	base := query.AlignTimestamp(time.Now().Add(-2*time.Hour).UnixMilli(), 300_000)
	b.writeMetrics(t, metricFixture(base))
	filters := []SeriesFilter{
		{},
		{Namespace: "App"},
		{Namespace: "Nope"},
		{MetricName: "Latency"},
		{MetricName: "Batch"},
		{Dimensions: map[string]string{"Service": "api"}},
		{Dimensions: map[string]string{"Service": "*"}},
		{Dimensions: map[string]string{"Service": ""}},
		{Dimensions: map[string]string{"Route": "/a"}},
		{Dimensions: map[string]string{"Zone": "z", "Service": "api"}},
		{Dimensions: map[string]string{"Nope": ""}},
		{MetricName: "Latency", Limit: 2},
		{Dimensions: map[string]string{"Service": "api"}, Limit: 3},
	}
	for _, tenant := range []uint32{1, 2, 3} {
		for _, f := range filters {
			m, err := b.mem.ListSeries(ctx, tenant, f)
			if err != nil {
				t.Fatal(err)
			}
			c, err := b.ch.ListSeries(ctx, tenant, f)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(m, c) {
				t.Errorf("tenant=%d filter=%+v\n memory     %+v\n clickhouse %+v", tenant, f, m, c)
			}
		}
	}
	all, _ := b.ch.ListSeries(ctx, 1, SeriesFilter{})
	if len(all) != 8 {
		t.Fatalf("tenant 1 series: want 8, got %d", len(all))
	}
}

// logFixture: strictly increasing timestamps (the memory engine orders by
// arrival, ClickHouse by timestamp; they agree when arrival is in time order).
func logFixture(base int64) []model.LogEvent {
	msgs := []string{
		"GET /health completed in 3ms",
		"upstream timeout after 30s",
		"connection reset by peer",
		"Timeouts exceeded: retrying",
		"user Ünïcode ÄRGER logged in",
		"payment declined code=51",
		"50% of quota used",
		"path=/api/v1/items done",
	}
	routes := []string{"GET /health", "POST /api", "GET /items"}
	levels := []string{"INFO", "WARN", "ERROR", "info", ""}
	var ls []model.LogEvent
	for i := 0; i < 240; i++ {
		e := model.LogEvent{
			TenantID:  1 + uint32(i%5/4), // every 5th event goes to tenant 2
			LogGroup:  []string{"app/api", "app/web", "sys"}[i%3],
			LogStream: fmt.Sprintf("s%d", i%4),
			Timestamp: base + int64(i)*1000 + int64(i%7), // off-grid ms survive the round trip
			Message:   fmt.Sprintf("%s #%d", msgs[i%len(msgs)], i),
			Level:     levels[i%len(levels)],
			Attributes: map[string]string{
				"route":  routes[i%len(routes)],
				"status": fmt.Sprint([]int{200, 500, 404}[i%3]),
				"Svc":    "Checkout",
			},
		}
		switch i % 11 {
		case 0:
			e.Attributes = nil
		case 5:
			e.Attributes["level"] = "attr-level" // shadows Level for level=... terms
		}
		ls = append(ls, e)
	}
	return ls
}

func normEvents(es []model.LogEvent) []model.LogEvent {
	out := make([]model.LogEvent, len(es))
	for i, e := range es {
		if len(e.Attributes) == 0 {
			e.Attributes = nil
		}
		out[i] = e
	}
	return out
}

func TestClickHouseLogsParity(t *testing.T) {
	b := newBoth(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).UnixMilli()
	b.writeLogs(t, logFixture(base))

	queries := []LogQuery{
		{TenantID: 1},
		{TenantID: 2},
		{TenantID: 3},
		{TenantID: 0}, // the memory engine treats tenant 0 as "all tenants"
		{TenantID: 1, LogGroup: "app/api"},
		{TenantID: 1, LogGroup: "app/web", LogStream: "s1"},
		{TenantID: 1, Level: "error"},
		{TenantID: 1, Level: "INFO"},
		{TenantID: 1, Query: "timeout"}, // substring: also "Timeouts"
		{TenantID: 1, Query: "time"},    // substring inside a token
		{TenantID: 1, Query: "-timeout"},
		{TenantID: 1, Query: `"connection reset"`},
		{TenantID: 1, Query: `"reset by" peer -#2`},
		{TenantID: 1, Query: "ünïcode"}, // case-insensitive, non-ASCII
		{TenantID: 1, Query: "är"},
		{TenantID: 1, Query: "50%"},
		{TenantID: 1, Query: "code=51"}, // key=value: no attribute "code"
		{TenantID: 1, Query: "path=/api/v1/items"},
		{TenantID: 1, Query: "=51"},
		{TenantID: 1, Query: `route="GET /health"`},
		{TenantID: 1, Query: `-route="GET /health" status=500`},
		{TenantID: 1, Query: "route=get"}, // unquoted value is exact, not substring
		{TenantID: 1, Query: "level=error"},
		{TenantID: 1, Query: "level=attr-level"},
		{TenantID: 1, Query: "-level=info"},
		{TenantID: 1, Query: "svc=checkout"}, // keys are lower-cased by Terms: no match
		{TenantID: 1, Query: "missing="},
		{TenantID: 1, Query: "-missing="},
		{TenantID: 1, Query: "status="},
		{TenantID: 1, Query: "-"},
		{TenantID: 1, Query: "' OR 1=1 --"},
		{TenantID: 1, Query: "{t0:String}"},
		{TenantID: 1, StartMs: base + 50_000, EndMs: base + 100_000},
		{TenantID: 1, StartMs: base + 50_003},
		{TenantID: 1, EndMs: base + 7_000},
		{TenantID: 1, Limit: 10},
		{TenantID: 1, Limit: 10, Query: "timeout"},
		{TenantID: 1, Limit: 4, Query: `"connection reset"`, LogGroup: "sys"},
		{TenantID: 1, Limit: 20000}, // >10000 falls back to 500 in both
	}
	for _, q := range queries {
		m, err := b.mem.SearchLogs(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		c, err := b.ch.SearchLogs(ctx, q)
		if err != nil {
			t.Fatalf("clickhouse %+v: %v", q, err)
		}
		if m.Truncated != c.Truncated || !reflect.DeepEqual(normEvents(m.Events), normEvents(c.Events)) {
			t.Errorf("query %+v: memory %d events (truncated=%v), clickhouse %d (truncated=%v)", q, len(m.Events), m.Truncated, len(c.Events), c.Truncated)
			for i := 0; i < len(m.Events) && i < len(c.Events); i++ {
				if !reflect.DeepEqual(normEvents(m.Events[i:i+1]), normEvents(c.Events[i:i+1])) {
					t.Errorf("  first difference at %d:\n   memory     %+v\n   clickhouse %+v", i, m.Events[i], c.Events[i])
					break
				}
			}
		}
	}
	for _, tenant := range []uint32{1, 2, 3} {
		m, _ := b.mem.ListLogGroups(ctx, tenant)
		c, err := b.ch.ListLogGroups(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(m, c) {
			t.Errorf("ListLogGroups(%d)\n memory     %+v\n clickhouse %+v", tenant, m, c)
		}
	}
}

func TestClickHouseStats(t *testing.T) {
	b := newBoth(t)
	base := query.AlignTimestamp(time.Now().Add(-2*time.Hour).UnixMilli(), 300_000)
	ds := metricFixture(base)
	b.writeMetrics(t, ds)
	b.writeLogs(t, logFixture(base))
	m, c := b.mem.Stats(), b.ch.Stats()
	if c.Backend != "clickhouse" || c.Series != m.Series || c.Samples != m.Samples || c.LogEvents != m.LogEvents {
		t.Fatalf("stats\n memory     %+v\n clickhouse %+v", m, c)
	}
	if c.ChunkBytes <= 0 || c.BytesPerPt <= 0 {
		t.Fatalf("clickhouse storage size missing: %+v", c)
	}
	t.Logf("memory %+v", m)
	t.Logf("clickhouse %+v", c)
	ids := map[uint64]bool{}
	for i := range ds {
		ids[ds[i].ID()] = true
	}
	if c.Series != len(ids) {
		t.Fatalf("series %d, want %d", c.Series, len(ids))
	}
}
