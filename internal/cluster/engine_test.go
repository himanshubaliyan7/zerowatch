package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
	"github.com/himanshubaliyan7/zerowatch/internal/traces"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testCluster is three storage shards behind httptest servers plus a
// query-role coordinator that is not on the ring itself.
type testCluster struct {
	mem    *Membership
	eng    *Engine
	locals map[string]*storage.Memory
	traces map[string]*traces.Store
	srvs   map[string]*httptest.Server
}

func newMemory() *storage.Memory {
	return storage.NewMemory(storage.MemoryConfig{RawRetention: time.Hour, RollupRetention: 24 * time.Hour, MaxSeries: 10000, MaxLogEvents: 10000})
}

// hideRaw strips FetchRaw so a shard behaves like an engine without raw access.
type hideRaw struct{ storage.Engine }

func newTestCluster(t *testing.T, secret string, wrap func(*storage.Memory) storage.Engine) *testCluster {
	t.Helper()
	tc := &testCluster{locals: map[string]*storage.Memory{}, traces: map[string]*traces.Store{}, srvs: map[string]*httptest.Server{}}
	tc.mem = NewStatic(NodeMeta{Name: "q", Role: "query"}, nil, quiet)
	for _, name := range []string{"s1", "s2", "s3"} {
		local := newMemory()
		var eng storage.Engine = local
		if wrap != nil {
			eng = wrap(local)
		}
		tr := traces.NewStore(traces.Config{})
		srv := httptest.NewServer(ShardHandler(eng, tr, tc.mem, secret, quiet))
		t.Cleanup(srv.Close)
		tc.locals[name], tc.traces[name], tc.srvs[name] = local, tr, srv
		tc.mem.upsert(Member{Name: name, Role: "storage", Addr: "127.0.0.1:7946", HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), State: "alive"})
	}
	tc.eng = NewEngine(tc.mem, nil, secret, quiet)
	return tc
}

var base = time.Now().Add(-30 * time.Minute).Truncate(time.Minute).UnixMilli()

func datum(host string, ts int64, v float64) model.MetricDatum {
	return model.MetricDatum{TenantID: 1, Namespace: "App", MetricName: "Latency", Timestamp: ts, Value: model.Float(v),
		Dimensions: []model.Dimension{{Name: "Host", Value: host}}}
}

func seriesOn(t *testing.T, m *storage.Memory) map[uint64]bool {
	t.Helper()
	ss, err := m.ListSeries(context.Background(), 1, storage.SeriesFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint64]bool{}
	for _, s := range ss {
		out[s.ID] = true
	}
	return out
}

func TestWriteRoutesByRing(t *testing.T) {
	tc := newTestCluster(t, "", nil)
	var ms []model.MetricDatum
	for i := 0; i < 300; i++ {
		ms = append(ms, datum(fmt.Sprintf("h%d", i), base, float64(i)))
	}
	if err := tc.eng.WriteMetrics(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	on := map[string]map[uint64]bool{}
	for n, l := range tc.locals {
		on[n] = seriesOn(t, l)
		if len(on[n]) == 0 {
			t.Fatalf("shard %s got no series; ring not spreading", n)
		}
	}
	for _, d := range ms {
		owner, _ := tc.mem.Ring().Get(d.ID())
		for n := range tc.locals {
			if on[n][d.ID()] != (n == owner) {
				t.Fatalf("series %d: on %s=%v, owner %s", d.ID(), n, on[n][d.ID()], owner)
			}
		}
	}
	if st := tc.eng.RoutingStats(); st.Forwarded != 300 || st.Failovers != 0 || st.Dropped != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestWriteFailsOverToReplica(t *testing.T) {
	tc := newTestCluster(t, "", nil)
	tc.srvs["s2"].Close() // s2 is down but gossip has not noticed yet
	var ms []model.MetricDatum
	var onS2 int
	for i := 0; i < 300; i++ {
		d := datum(fmt.Sprintf("h%d", i), base, 1)
		if o, _ := tc.mem.Ring().Get(d.ID()); o == "s2" {
			onS2++
		}
		ms = append(ms, d)
	}
	if err := tc.eng.WriteMetrics(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	st := tc.eng.RoutingStats()
	if onS2 == 0 || st.Failovers != uint64(onS2) || st.Dropped != 0 || st.Forwarded != 300 {
		t.Fatalf("onS2=%d stats %+v", onS2, st)
	}
	for _, d := range ms {
		owners, _ := tc.mem.Ring().GetN(d.ID(), 2)
		want := owners[0]
		if want == "s2" {
			want = owners[1]
		}
		if !seriesOn(t, tc.locals[want])[d.ID()] {
			t.Fatalf("series %d not on %s", d.ID(), want)
		}
	}
	// While s2 is still on the ring, every read comes back partial and says so.
	sum := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Host": "*"}, Stat: "SampleCount"}
	_, err := tc.eng.FetchStat(context.Background(), 1, sum, base, base+60_000, 60)
	var pe *query.PartialError
	if !errors.As(err, &pe) || len(pe.Points) != 1 || pe.Points[0].V != 300 || !strings.Contains(pe.Msg, "s2") {
		t.Fatalf("partial fetch: %v", err)
	}
	qe := &query.Engine{Fetcher: tc.eng, MaxParallel: 2}
	res, err := qe.Execute(context.Background(), query.Request{TenantID: 1, StartMs: base, EndMs: base + 60_000, Period: 60,
		Queries: []query.MetricDataQuery{{ID: "m", MetricStat: &sum}}})
	if err != nil || len(res) != 1 || res[0].StatusCode != "PartialData" || len(res[0].Values) != 1 || res[0].Values[0] != 300 {
		t.Fatalf("engine: %+v %v", res, err)
	}
	ss, err := tc.eng.ListSeries(context.Background(), 1, storage.SeriesFilter{})
	if !storage.IsPartial(err) || !strings.Contains(err.Error(), "s2") || len(ss) == 0 {
		t.Fatalf("ListSeries with a down shard: %d series, err %v", len(ss), err)
	}
	if lr, err := tc.eng.SearchLogs(context.Background(), storage.LogQuery{TenantID: 1}); err != nil || !strings.Contains(lr.Partial, "s2") {
		t.Fatalf("SearchLogs with a down shard: partial %q err %v", lr.Partial, err)
	}
	if _, err := tc.eng.ListLogGroups(context.Background(), 1); !storage.IsPartial(err) {
		t.Fatalf("ListLogGroups with a down shard: %v", err)
	}
	if tc.eng.RoutingStats().PartialReads == 0 {
		t.Fatal("partial read not counted")
	}
	// Once membership drops it, everything is readable from the survivors.
	tc.mem.remove("s2")
	pts, err := tc.eng.FetchStat(context.Background(), 1, query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Host": "*"}, Stat: "SampleCount"}, base, base+60_000, 60)
	if err != nil || len(pts) != 1 || pts[0].V != 300 {
		t.Fatalf("count after failover: %v %v", pts, err)
	}
}

func TestWriteAllReplicasDown(t *testing.T) {
	tc := newTestCluster(t, "", nil)
	for _, s := range tc.srvs {
		s.Close()
	}
	err := tc.eng.WriteMetrics(context.Background(), []model.MetricDatum{datum("a", base, 1), datum("b", base, 2)})
	if err == nil {
		t.Fatal("expected error when no replica accepts")
	}
	if st := tc.eng.RoutingStats(); st.Dropped != 2 {
		t.Fatalf("stats %+v", st)
	}
}

// writeSpread writes n values per host across many hosts so a wildcard query
// spans every shard, and returns a single-node reference engine holding the
// same data.
func writeSpread(t *testing.T, tc *testCluster) *storage.Memory {
	t.Helper()
	ref := newMemory()
	var ms []model.MetricDatum
	for h := 0; h < 30; h++ {
		for i := 0; i < 40; i++ {
			// two periods; skewed values so averaging averages would be wrong
			ts := base + int64(i%2)*60_000 + int64(i)*100
			v := float64((h*h*7+i*13)%997) + float64(h)
			if h%5 == 0 {
				for k := 0; k < 3; k++ {
					ms = append(ms, datum(fmt.Sprintf("h%d", h), ts+int64(k), v*3))
				}
			}
			ms = append(ms, datum(fmt.Sprintf("h%d", h), ts, v))
		}
	}
	if err := tc.eng.WriteMetrics(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	if err := ref.WriteMetrics(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	for n, l := range tc.locals {
		if len(seriesOn(t, l)) == 0 {
			t.Fatalf("shard %s empty", n)
		}
	}
	return ref
}

func TestScatterGatherMergeMatchesSingleNode(t *testing.T) {
	tc := newTestCluster(t, "", nil)
	ref := writeSpread(t, tc)
	ctx := context.Background()
	for _, stat := range []string{"Sum", "Average", "Minimum", "Maximum", "SampleCount", "p99", "p50", "p90.5"} {
		for _, period := range []int{60, 30} { // 60 = rollup path, 30 = raw path
			ms := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Host": "*"}, Stat: stat}
			want, err := ref.FetchStat(ctx, 1, ms, base, base+120_000, period)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tc.eng.FetchStat(ctx, 1, ms, base, base+120_000, period)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) || len(want) == 0 {
				t.Fatalf("%s/%d: got %v want %v", stat, period, got, want)
			}
			for i := range want {
				if got[i].T != want[i].T || math.Abs(got[i].V-want[i].V) > 1e-9*math.Max(1, math.Abs(want[i].V)) {
					t.Fatalf("%s/%d bucket %d: got %v want %v", stat, period, i, got[i], want[i])
				}
			}
		}
	}
	if st := tc.eng.RoutingStats(); st.ApproxPctMrgs != 0 {
		t.Fatalf("percentiles should be exact with raw access: %+v", st)
	}
	// A single exact series lives on one shard; the merge must not alter it.
	ms := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Host": "h3"}, Stat: "Average"}
	want, _ := ref.FetchStat(ctx, 1, ms, base, base+120_000, 60)
	got, _ := tc.eng.FetchStat(ctx, 1, ms, base, base+120_000, 60)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("single series: %v vs %v", got, want)
	}
}

func TestPercentileWithoutRawIsFlaggedApproximate(t *testing.T) {
	tc := newTestCluster(t, "", func(m *storage.Memory) storage.Engine { return hideRaw{m} })
	writeSpread(t, tc)
	ms := query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Host": "*"}, Stat: "p99"}
	pts, err := tc.eng.FetchStat(context.Background(), 1, ms, base, base+120_000, 60)
	if err != nil || len(pts) != 2 {
		t.Fatalf("%v %v", pts, err)
	}
	if tc.eng.RoutingStats().ApproxPctMrgs == 0 {
		t.Fatal("expected approximate merge to be counted")
	}
}

func TestListSeriesDedupes(t *testing.T) {
	tc := newTestCluster(t, "", nil)
	ctx := context.Background()
	d := datum("dup", base, 1)
	// The same series on two shards, as after a failover.
	_ = tc.locals["s1"].WriteMetrics(ctx, []model.MetricDatum{d})
	_ = tc.locals["s3"].WriteMetrics(ctx, []model.MetricDatum{d})
	_ = tc.locals["s2"].WriteMetrics(ctx, []model.MetricDatum{datum("other", base, 1)})
	ss, err := tc.eng.ListSeries(ctx, 1, storage.SeriesFilter{Namespace: "App"})
	if err != nil || len(ss) != 2 {
		t.Fatalf("got %v %v", ss, err)
	}
	ss, _ = tc.eng.ListSeries(ctx, 1, storage.SeriesFilter{Namespace: "App", Limit: 1})
	if len(ss) != 1 {
		t.Fatalf("limit: %v", ss)
	}
}

func TestLogsRouteAndMerge(t *testing.T) {
	tc := newTestCluster(t, "", nil)
	ctx := context.Background()
	var ls []model.LogEvent
	for i := 0; i < 90; i++ {
		ls = append(ls, model.LogEvent{TenantID: 1, LogGroup: "/app", LogStream: fmt.Sprintf("pod-%d", i%9), Timestamp: base + int64(i)*1000, Message: fmt.Sprintf("req %d", i), Level: "info"})
	}
	if err := tc.eng.WriteLogs(ctx, ls); err != nil {
		t.Fatal(err)
	}
	// each stream stays whole on its owner
	for _, e := range ls {
		owner, _ := tc.mem.Ring().Get(LogStreamKey(&e))
		r, _ := tc.locals[owner].SearchLogs(ctx, storage.LogQuery{TenantID: 1, LogStream: e.LogStream, Limit: 1000})
		if len(r.Events) != 10 {
			t.Fatalf("stream %s on %s has %d events", e.LogStream, owner, len(r.Events))
		}
	}
	res, err := tc.eng.SearchLogs(ctx, storage.LogQuery{TenantID: 1, Limit: 5})
	if err != nil || len(res.Events) != 5 || !res.Truncated {
		t.Fatalf("%+v %v", res, err)
	}
	for i, e := range res.Events {
		if e.Timestamp != base+int64(89-i)*1000 {
			t.Fatalf("not newest-first: %d at %d", e.Timestamp, i)
		}
	}
	gs, err := tc.eng.ListLogGroups(ctx, 1)
	if err != nil || len(gs) != 1 || gs[0].Events != 90 || len(gs[0].Streams) != 9 || gs[0].LastEvent != base+89_000 {
		t.Fatalf("%+v %v", gs, err)
	}
	if st := tc.eng.Stats(); st.Backend != "cluster" || st.LogEvents != 90 {
		t.Fatalf("stats %+v", st)
	}
}

func post(t *testing.T, url, secret string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+pathSeries, bytes.NewReader([]byte(`{"tenant":1}`)))
	if secret != "" {
		req.Header.Set(SecretHeader, secret)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestInternalAuth(t *testing.T) {
	// No secret: only member IPs are accepted. 127.0.0.1 is not a member here.
	mem := NewStatic(NodeMeta{Name: "s1", Role: "storage", HTTPAddr: "10.9.9.1:8080"}, []Member{{Name: "i1", Role: "ingest", Addr: "10.9.9.2:7946"}}, quiet)
	srv := httptest.NewServer(ShardHandler(newMemory(), nil, mem, "", quiet))
	defer srv.Close()
	if code := post(t, srv.URL, ""); code != http.StatusForbidden {
		t.Fatalf("non-member without secret: %d", code)
	}
	mem.upsert(Member{Name: "local", Role: "ingest", Addr: "127.0.0.1:7946", State: "alive"})
	if code := post(t, srv.URL, ""); code != http.StatusOK {
		t.Fatalf("member: %d", code)
	}

	// With a secret, the header is required even from a member IP.
	sec := httptest.NewServer(ShardHandler(newMemory(), nil, mem, "s3cret-s3cret", quiet))
	defer sec.Close()
	for secret, want := range map[string]int{"": 403, "wrong": 403, "s3cret-s3cret": 200} {
		if code := post(t, sec.URL, secret); code != want {
			t.Fatalf("secret %q: %d want %d", secret, code, want)
		}
	}

	// The engine sends the secret, so a secret-protected cluster works end to end.
	tc := newTestCluster(t, "k", nil)
	if err := tc.eng.WriteMetrics(context.Background(), []model.MetricDatum{datum("a", base, 1)}); err != nil {
		t.Fatal(err)
	}
	bad := NewEngine(tc.mem, nil, "nope", quiet)
	if _, err := bad.ListSeries(context.Background(), 1, storage.SeriesFilter{}); err == nil {
		t.Fatal("wrong secret should be refused")
	}
}

func TestStandaloneAndLocalShard(t *testing.T) {
	local := newMemory()
	n, err := Start(Options{NodeName: "solo", Role: "all", HTTPAddr: ":8080", GossipPort: 7946}, local, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if n.Engine != nil || n.Internal != nil || n.Store != storage.Engine(local) {
		t.Fatal("role=all without seeds must not change storage")
	}
	st := n.Status()
	if st.Clustered || len(st.Members) != 1 || len(st.Ring.Nodes) != 1 || st.Ring.Nodes[0] != "solo" {
		t.Fatalf("status %+v", st)
	}
	if !(Options{Role: "storage"}).Clustered() || (Options{Role: "supervisor"}).Clustered() || !(Options{Role: "all", Seeds: []string{"x"}}).Clustered() {
		t.Fatal("Clustered()")
	}

	// role=all inside a cluster serves its own shard in-process (no HTTP).
	mem := NewStatic(NodeMeta{Name: "me", Role: "all"}, nil, quiet)
	eng := NewEngine(mem, local, "", quiet)
	if err := eng.WriteMetrics(context.Background(), []model.MetricDatum{datum("x", base, 2)}); err != nil {
		t.Fatal(err)
	}
	pts, err := eng.FetchStat(context.Background(), 1, query.MetricStat{Namespace: "App", MetricName: "Latency", Dimensions: map[string]string{"Host": "x"}, Stat: "Sum"}, base, base+60_000, 60)
	if err != nil || len(pts) != 1 || pts[0].V != 2 {
		t.Fatalf("%v %v", pts, err)
	}
}

func TestEmptyRing(t *testing.T) {
	eng := NewEngine(NewStatic(NodeMeta{Name: "q", Role: "query"}, nil, quiet), nil, "", quiet)
	if err := eng.WriteMetrics(context.Background(), []model.MetricDatum{datum("a", base, 1)}); err != ErrNoStorage {
		t.Fatalf("write: %v", err)
	}
	if _, err := eng.ListSeries(context.Background(), 1, storage.SeriesFilter{}); err != ErrNoStorage {
		t.Fatalf("read: %v", err)
	}
}
