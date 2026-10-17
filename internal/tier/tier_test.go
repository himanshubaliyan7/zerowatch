package tier

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

const day = 24 * time.Hour

// genData writes `span` of data ending at now: three Latency series sampled
// every 20 s (values are multiples of 0.25, so sums are exact in any order),
// one Requests series of statistic sets per minute, and a log event every
// 2 minutes in two groups.
func genData(tenant uint32, now time.Time, span time.Duration) ([]model.MetricDatum, []model.LogEvent) {
	var ms []model.MetricDatum
	var ls []model.LogEvent
	end := now.UnixMilli()
	start := end - span.Milliseconds()
	i := 0
	for ts := start; ts < end; ts += 20_000 {
		for j, svc := range []string{"a", "b", "c"} {
			v := float64((i*7+j*13)%97) * 0.25
			ms = append(ms, model.MetricDatum{TenantID: tenant, Namespace: "App", MetricName: "Latency", Dimensions: []model.Dimension{{Name: "Service", Value: svc}}, Timestamp: ts, Value: model.Float(v)})
		}
		if i%3 == 0 {
			n := float64(i%5 + 1)
			ms = append(ms, model.MetricDatum{TenantID: tenant, Namespace: "App", MetricName: "Requests", Timestamp: ts,
				StatisticValues: &model.StatisticSet{SampleCount: n, Sum: n * 2.5, Minimum: 1, Maximum: n}})
		}
		if i%6 == 0 {
			status, group := "200", "app"
			if i%60 == 0 {
				status = "500"
			}
			if i%12 == 0 {
				group = "db"
			}
			ls = append(ls, model.LogEvent{TenantID: tenant, LogGroup: group, LogStream: "s1", Timestamp: ts, Level: "info",
				Message: fmt.Sprintf("request %d status=%s", i, status), Attributes: map[string]string{"status": status}})
		}
		i++
	}
	return ms, ls
}

func write(t testing.TB, e storage.Engine, ms []model.MetricDatum, ls []model.LogEvent) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < len(ms); i += 5000 {
		if err := e.WriteMetrics(ctx, ms[i:min(i+5000, len(ms))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.WriteLogs(ctx, ls); err != nil {
		t.Fatal(err)
	}
}

func samePoints(a, b []model.Point) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].T != b[i].T || !(a[i].V == b[i].V || math.IsNaN(a[i].V) && math.IsNaN(b[i].V)) {
			return false
		}
	}
	return true
}

// compareEngines checks that every query answers identically on both
// engines over ranges reaching deep into the cold tier.
func compareEngines(t testing.TB, tenant uint32, want, got *storage.Memory, now time.Time) {
	t.Helper()
	ctx := context.Background()
	n := now.UnixMilli()
	h := time.Hour.Milliseconds()
	ranges := [][2]int64{{n - 73*h, n + h}, {n - 72*h, n - 48*h}, {n - 30*h, n - h/3}, {n - 7*h, n - 5*h}, {n - h/2, n}}
	stats := []string{"Average", "Sum", "Minimum", "Maximum", "SampleCount", "p50", "p90", "p99", "p99.9"}
	queries := []query.MetricStat{
		{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Service": "a"}},
		{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Service": "*"}},
		{Namespace: "App", MetricName: "Requests"},
		{Namespace: "App", MetricName: "Missing"},
	}
	checked := 0
	for _, r := range ranges {
		for _, q := range queries {
			for _, st := range stats {
				for _, p := range []int{60, 300, 3600, 20} {
					q.Stat = st
					a, err1 := want.FetchStat(ctx, tenant, q, r[0], r[1], p)
					b, err2 := got.FetchStat(ctx, tenant, q, r[0], r[1], p)
					if err1 != nil || err2 != nil {
						t.Fatalf("errors: %v %v", err1, err2)
					}
					if !samePoints(a, b) {
						t.Fatalf("%s %s %v period %d range %d..%d: hot %d points, tiered %d points (first diff %s)", q.MetricName, st, q.Dimensions, p, (r[0]-n)/h, (r[1]-n)/h, len(a), len(b), firstDiff(a, b))
					}
					checked += len(a)
				}
			}
			ra, _, _ := want.FetchRaw(ctx, tenant, q, r[0], r[1], 300, 0)
			rb, _, err := got.FetchRaw(ctx, tenant, q, r[0], r[1], 300, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(ra) != len(rb) {
				t.Fatalf("raw buckets %d vs %d", len(ra), len(rb))
			}
			for k, va := range ra {
				vb := rb[k]
				sort.Float64s(va)
				sort.Float64s(vb)
				if fmt.Sprint(va) != fmt.Sprint(vb) {
					t.Fatalf("raw bucket %d differs", k)
				}
			}
		}
	}
	if checked < 10000 {
		t.Fatalf("only %d points compared", checked)
	}
	for _, lq := range []storage.LogQuery{{Query: "status=500"}, {LogGroup: "db", Query: "request"}, {StartMs: n - 50*h, EndMs: n - 40*h}} {
		lq.TenantID, lq.Limit = tenant, 10_000
		a, _ := want.SearchLogs(ctx, lq)
		b, err := got.SearchLogs(ctx, lq)
		if err != nil || b.Partial != "" {
			t.Fatalf("logs: %v %q", err, b.Partial)
		}
		if logKey(a.Events) != logKey(b.Events) || len(a.Events) == 0 {
			t.Fatalf("logs %+v: hot %d events, tiered %d", lq, len(a.Events), len(b.Events))
		}
		for i := 1; i < len(b.Events); i++ {
			if b.Events[i].Timestamp > b.Events[i-1].Timestamp {
				t.Fatalf("tiered logs not newest first at %d", i)
			}
		}
	}
	lim, err := got.SearchLogs(ctx, storage.LogQuery{TenantID: tenant, Query: "request", Limit: 50, StartMs: n - 60*h, EndMs: n - 50*h})
	if err != nil || len(lim.Events) != 50 || !lim.Truncated {
		t.Fatalf("limit: %d events truncated=%v err=%v", len(lim.Events), lim.Truncated, err)
	}
}

func firstDiff(a, b []model.Point) string {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("%v vs %v", a[i], b[i])
		}
	}
	return "length"
}

func logKey(es []model.LogEvent) string {
	ks := make([]string, len(es))
	for i, e := range es {
		ks[i] = fmt.Sprintf("%d|%s|%s", e.Timestamp, e.LogGroup, e.Message)
	}
	sort.Strings(ks)
	return strings.Join(ks, "\n")
}

func tieredPair(t testing.TB, store *S3, prefix string) (*storage.Memory, *storage.Memory, *Tier) {
	hot := storage.NewMemory(storage.MemoryConfig{RawRetention: 30 * day, RollupRetention: 30 * day})
	tr := New(Config{Store: store, Prefix: prefix, RollupHot: 6 * time.Hour, LogsHot: time.Hour,
		Retention: Retention{Default: Policy{RawHot: time.Hour, Raw: 30 * day, Rollup1m: 30 * day, Logs: 30 * day}}})
	if err := tr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	cold := storage.NewMemory(storage.MemoryConfig{Tier: tr})
	t.Cleanup(func() { hot.Close(); cold.Close() })
	return hot, cold, tr
}

// runTieredExactness writes 3 days with a 1 h hot window and checks that
// queries match an all-hot engine while queued, after upload, and with a
// cold cache and a re-read index.
func runTieredExactness(t *testing.T, store *S3, prefix string) *Tier {
	now := time.Now()
	hot, cold, tr := tieredPair(t, store, prefix)
	ms, ls := genData(1, now, 3*day)
	write(t, hot, ms, ls)
	write(t, cold, ms, ls)
	cold.Compact()
	if st := cold.Stats(); st.ChunkBytes*10 > hot.Stats().ChunkBytes || st.LogEvents*10 > len(ls) {
		t.Fatalf("not evicted: tiered %+v, hot %+v", st, hot.Stats())
	}
	compareEngines(t, 1, hot, cold, now)
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := tr.Stats()
	if st.SegmentsWritten == 0 || st.QueuedBytes != 0 || st.StagedGroups != 0 {
		t.Fatalf("flush: %+v", st)
	}
	compareEngines(t, 1, hot, cold, now)
	// A cold cache and an index rebuilt from LIST (as after a restart).
	tr.cache = newLRU(tr.cfg.CacheBytes)
	tr.mu.Lock()
	tr.hours = map[hourKey]*hourState{}
	tr.mu.Unlock()
	if err := tr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	compareEngines(t, 1, hot, cold, now)
	t.Logf("tier: %+v", tr.Stats())
	return tr
}

func TestTieredMatchesHot(t *testing.T) {
	f, s3 := newFakeS3(t)
	f.pageSize = 50
	tr := runTieredExactness(t, s3, "node1")
	if f.ranged == 0 {
		t.Error("no ranged GETs")
	}
	for _, k := range f.keys() {
		if _, ok := parseKey("node1", k); !ok {
			t.Errorf("unexpected key %s", k)
		}
	}
	// Retention: 36 h for everything; older hourly prefixes are deleted.
	tr.cfg.Retention.Default = Policy{RawHot: time.Hour, Raw: 36 * time.Hour, Rollup1m: 36 * time.Hour, Logs: 36 * time.Hour}
	before := len(f.keys())
	n, err := tr.RunRetention(context.Background())
	if err != nil || n == 0 {
		t.Fatalf("retention: %d %v", n, err)
	}
	cut := time.Now().Add(-37 * time.Hour).UnixMilli()
	for _, k := range f.keys() {
		hk, _ := parseKey("node1", k)
		if hk.hour < cut {
			t.Errorf("%s survived retention", k)
		}
	}
	if len(f.keys()) != before-n {
		t.Errorf("deleted %d of %d", n, before)
	}
}

func TestRetentionLocalPerTenant(t *testing.T) {
	now := time.Now()
	tr := New(Config{Retention: Retention{Default: Policy{Raw: 30 * day, Rollup1m: 30 * day, Logs: 30 * day},
		PerTenant: map[uint32]Policy{7: {Raw: 2 * time.Hour, Rollup1m: 3 * time.Hour, Logs: 2 * time.Hour}}}})
	m := storage.NewMemory(storage.MemoryConfig{Tier: tr})
	defer m.Close()
	for _, tenant := range []uint32{1, 7} {
		ms, ls := genData(tenant, now, 8*time.Hour)
		write(t, m, ms, ls)
	}
	m.Compact()
	ctx := context.Background()
	h := time.Hour.Milliseconds()
	n := now.UnixMilli()
	q := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Service": "a"}, Stat: "p50"}
	count := func(tenant uint32, stat string, from, to int64) int {
		q.Stat = stat
		ps, err := m.FetchStat(ctx, tenant, q, from, to, 60)
		if err != nil {
			t.Fatal(err)
		}
		return len(ps)
	}
	if count(1, "p50", n-8*h, n-6*h) == 0 || count(1, "Average", n-8*h, n-6*h) == 0 {
		t.Fatal("tenant 1 lost data within its retention")
	}
	if c := count(7, "p50", n-8*h, n-4*h); c != 0 {
		t.Fatalf("tenant 7 raw kept %d points past 2 h", c)
	}
	if c := count(7, "Average", n-8*h, n-4*h); c != 0 {
		t.Fatalf("tenant 7 rollups kept %d points past 3 h", c)
	}
	if count(7, "p50", n-h, n) == 0 {
		t.Fatal("tenant 7 lost recent data")
	}
	r7, _ := m.SearchLogs(ctx, storage.LogQuery{TenantID: 7, StartMs: n - 8*h, EndMs: n - 3*h, Limit: 1000})
	r1, _ := m.SearchLogs(ctx, storage.LogQuery{TenantID: 1, StartMs: n - 8*h, EndMs: n - 3*h, Limit: 1000})
	if len(r7.Events) != 0 || len(r1.Events) == 0 {
		t.Fatalf("logs: tenant 7 %d, tenant 1 %d", len(r7.Events), len(r1.Events))
	}
}

func TestColdLogScanCap(t *testing.T) {
	_, s3 := newFakeS3(t)
	now := time.Now()
	tr := New(Config{Store: s3, LogsHot: time.Hour, MaxLogScanBytes: 2048, Retention: Retention{Default: Policy{RawHot: time.Hour}}})
	m := storage.NewMemory(storage.MemoryConfig{Tier: tr})
	defer m.Close()
	_, ls := genData(1, now, 12*time.Hour)
	write(t, m, nil, ls)
	m.Compact()
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := m.SearchLogs(context.Background(), storage.LogQuery{TenantID: 1, Query: "nomatch", StartMs: now.Add(-12 * time.Hour).UnixMilli(), Limit: 100})
	if err != nil || res.Partial == "" || !strings.Contains(res.Partial, "narrow") {
		t.Fatalf("partial %q err %v", res.Partial, err)
	}
}

func TestFlushFailureKeepsData(t *testing.T) {
	f, s3 := newFakeS3(t)
	now := time.Now()
	hot, cold, tr := tieredPair(t, s3, "p")
	ms, ls := genData(1, now, 6*time.Hour)
	write(t, hot, ms, ls)
	write(t, cold, ms, ls)
	cold.Compact()
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	if err := tr.Flush(context.Background()); err == nil {
		t.Fatal("flush against a failing bucket succeeded")
	}
	if tr.Stats().StagedGroups == 0 || tr.Stats().LastError == "" {
		t.Fatalf("staged data lost: %+v", tr.Stats())
	}
	compareEngines6h(t, hot, cold, now)
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.Stats().StagedGroups != 0 {
		t.Fatal("still staged")
	}
	compareEngines6h(t, hot, cold, now)
}

func compareEngines6h(t *testing.T, want, got storage.Engine, now time.Time) {
	ctx := context.Background()
	for _, st := range []string{"Average", "Sum", "p99"} {
		q := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Service": "*"}, Stat: st}
		a, _ := want.FetchStat(ctx, 1, q, now.Add(-7*time.Hour).UnixMilli(), now.UnixMilli(), 300)
		b, err := got.FetchStat(ctx, 1, q, now.Add(-7*time.Hour).UnixMilli(), now.UnixMilli(), 300)
		if err != nil || !samePoints(a, b) || len(a) < 60 {
			t.Fatalf("%s: %d vs %d (%v)", st, len(a), len(b), err)
		}
	}
}

func TestS3Client(t *testing.T) {
	f, s3 := newFakeS3(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		if err := s3.Put(ctx, fmt.Sprintf("a/k%d", i), []byte(fmt.Sprintf("value-%d", i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	_ = s3.Put(ctx, "b/other", []byte("x"), "")
	objs, err := s3.List(ctx, "a/")
	if err != nil || len(objs) != 7 || objs[6].Key != "a/k6" || objs[0].Size != 7 {
		t.Fatalf("list (paged by %d): %v %v", f.pageSize, objs, err)
	}
	if b, err := s3.GetRange(ctx, "a/k3", 2, 3); err != nil || string(b) != "lue" {
		t.Fatalf("range: %q %v", b, err)
	}
	if b, err := s3.Get(ctx, "a/k3"); err != nil || string(b) != "value-3" {
		t.Fatalf("get: %q %v", b, err)
	}
	if err := s3.Delete(ctx, "a/k3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Get(ctx, "a/k3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	f.fail = true
	if err := s3.Put(ctx, "x", nil, ""); err == nil || !strings.Contains(err.Error(), "InternalError") {
		t.Fatalf("error: %v", err)
	}
}

func TestSegmentFormat(t *testing.T) {
	w := newSegmentWriter()
	ss := []sample{{1000, 1.5}, {2000, math.Inf(1)}, {3000, -0.25}}
	w.add(BlockInfo{ID: 1, MinT: 1000, MaxT: 3000, N: 3}, encodeRaw(ss))
	r := map[int64]storage.Agg{60000: {Count: 3, Sum: 4.5, Min: 1, Max: 2}, 120000: {Count: 1, Sum: 0.1, Min: 0.1, Max: 0.1}}
	rb, n := encodeRollup(r)
	w.add(BlockInfo{ID: 2, N: n}, rb)
	seg, blocks, err := w.finish()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := readSegmentIndex(seg)
	if err != nil || fmt.Sprint(idx) != fmt.Sprint(blocks) {
		t.Fatalf("index %v %v", idx, err)
	}
	var got []sample
	_ = decodeRaw(seg[idx[0].Off:idx[0].Off+idx[0].Len], idx[0].N, func(ts int64, v float64) bool { got = append(got, sample{ts, v}); return true })
	if fmt.Sprint(got) != fmt.Sprint(ss) {
		t.Fatalf("raw %v", got)
	}
	back := map[int64]storage.Agg{}
	if err := decodeRollup(seg[idx[1].Off:idx[1].Off+idx[1].Len], idx[1].N, func(m int64, a storage.Agg) { back[m] = a }); err != nil || fmt.Sprint(back) != fmt.Sprint(r) {
		t.Fatalf("rollup %v %v", back, err)
	}
	k := hourKey{42, KindLogs, time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC).UnixMilli()}
	key := k.dir("zw/node-1") + "01-000002.zwseg"
	if key != "zw/node-1/42/logs/2026/10/03/07/01-000002.zwseg" {
		t.Fatal(key)
	}
	if back, ok := parseKey("zw/node-1", key); !ok || back != k {
		t.Fatalf("parseKey %v", back)
	}
}

func TestParseRetention(t *testing.T) {
	r, err := ParseRetention("raw=7d,rawhot=12h,rollup=13w", []string{"42:raw=30d,logs=1d;7:logs=2h", "9:rollup1m=1d"})
	if err != nil {
		t.Fatal(err)
	}
	if p := r.For(42); p.Raw != 30*day || p.Logs != day || p.RawHot != 12*time.Hour || p.Rollup1m != 91*day {
		t.Fatalf("tenant 42: %+v", p)
	}
	if p := r.For(1); p.Logs != DefaultLogs || p.Raw != 7*day {
		t.Fatalf("default: %+v", p)
	}
	if r.For(7).Logs != 2*time.Hour || r.For(9).Rollup1m != day {
		t.Fatal("tenants 7/9")
	}
	for _, bad := range []string{"raw", "raw=-1d", "speed=1d"} {
		if _, err := ParsePolicy(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseRetention("", []string{"x:raw=1d"}); err == nil {
		t.Error("bad tenant accepted")
	}
	tr := New(Config{Retention: r})
	if c := tr.Cutoffs(42, 1e12); c.Raw != 1e12-(30*day).Milliseconds() {
		t.Fatalf("no bucket: raw cutoff is the retention: %+v", c)
	}
	tr = New(Config{Store: &S3{}, Retention: r})
	if c := tr.Cutoffs(42, 1e12); c.Raw != 1e12-(12*time.Hour).Milliseconds() || c.Logs != 1e12-day.Milliseconds() {
		t.Fatalf("bucket: hot cutoffs: %+v", c)
	}
}
