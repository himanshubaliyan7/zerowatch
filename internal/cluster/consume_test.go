package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/commitlog"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
	"github.com/himanshubaliyan7/zerowatch/internal/traces"
)

// consumeCluster is storage node s1 (this process, consuming the commit log)
// plus peers s2 and s3 behind httptest shard servers.
type consumeCluster struct {
	mem    *Membership
	router *ConsumeRouter
	locals map[string]*storage.Memory
	srvs   map[string]*httptest.Server
	writes map[string]*atomic.Int64 // internal write RPCs served per peer
}

func newConsumeCluster(t *testing.T) *consumeCluster {
	t.Helper()
	cc := &consumeCluster{locals: map[string]*storage.Memory{}, srvs: map[string]*httptest.Server{}, writes: map[string]*atomic.Int64{}}
	cc.mem = NewStatic(NodeMeta{Name: "s1", Role: "storage"}, nil, quiet)
	cc.locals["s1"] = newMemory()
	for _, name := range []string{"s2", "s3"} {
		local := newMemory()
		h := ShardHandler(local, traces.NewStore(traces.Config{}), cc.mem, "sec", quiet)
		cnt := &atomic.Int64{}
		srv := httptest.NewServer(countWrites(h, cnt))
		t.Cleanup(srv.Close)
		cc.locals[name], cc.srvs[name], cc.writes[name] = local, srv, cnt
		cc.mem.upsert(Member{Name: name, Role: "storage", HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), State: "alive"})
	}
	r, err := NewConsumeRouter(NewEngine(cc.mem, cc.locals["s1"], "sec", quiet), quiet)
	if err != nil {
		t.Fatal(err)
	}
	r.RetryDelay = -1
	cc.router = r
	return cc
}

// countWrites counts the internal write RPCs a shard server answers.
func countWrites(h http.Handler, n *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathWrite {
			n.Add(1)
		}
		h.ServeHTTP(w, r)
	})
}

func consumeData(nSeries, nStreams int) ([]model.MetricDatum, []model.LogEvent) {
	ms := make([]model.MetricDatum, nSeries)
	for i := range ms {
		ms[i] = datum(fmt.Sprintf("h%03d", i), base, float64(i))
	}
	ls := make([]model.LogEvent, nStreams)
	for i := range ls {
		ls[i] = model.LogEvent{TenantID: 1, LogGroup: "app", LogStream: fmt.Sprintf("st%03d", i), Timestamp: base, Message: "hello"}
	}
	return ms, ls
}

func streamsOn(t *testing.T, m *storage.Memory) map[string]bool {
	t.Helper()
	gs, err := m.ListLogGroups(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, g := range gs {
		for _, s := range g.Streams {
			out[s] = true
		}
	}
	return out
}

// placement returns, per series ID and per stream, the nodes holding it.
func (cc *consumeCluster) placement(t *testing.T) (map[uint64][]string, map[string][]string) {
	series, streams := map[uint64][]string{}, map[string][]string{}
	for name, m := range cc.locals {
		for id := range seriesOn(t, m) {
			series[id] = append(series[id], name)
		}
		for s := range streamsOn(t, m) {
			streams[s] = append(streams[s], name)
		}
	}
	return series, streams
}

func TestConsumeRouterPlacesOnRingOwner(t *testing.T) {
	cc := newConsumeCluster(t)
	ms, ls := consumeData(300, 60)
	if err := cc.router.Write(context.Background(), ms, ls); err != nil {
		t.Fatal(err)
	}
	series, streams := cc.placement(t)
	ring := cc.mem.Ring()
	for _, d := range ms {
		want, _ := ring.Get(d.ID())
		if got := series[d.ID()]; len(got) != 1 || got[0] != want {
			t.Fatalf("series %d on %v, want only %s", d.ID(), got, want)
		}
	}
	for i := range ls {
		want, _ := ring.Get(LogStreamKey(&ls[i]))
		if got := streams[ls[i].LogStream]; len(got) != 1 || got[0] != want {
			t.Fatalf("stream %s on %v, want only %s", ls[i].LogStream, got, want)
		}
	}
	st := cc.router.Stats()
	if st.Owned+st.Forwarded != 360 || st.Owned == 0 || st.Forwarded == 0 || st.KeptLocal != 0 || st.ForwardFailed != 0 {
		t.Fatalf("stats %+v", st)
	}
	// one write RPC per peer for the whole batch (metrics and logs together)
	for name, c := range cc.writes {
		if c.Load() != 1 {
			t.Fatalf("%s served %d write RPCs, want 1", name, c.Load())
		}
	}
}

func TestConsumeRouterFailsOverToReplica(t *testing.T) {
	cc := newConsumeCluster(t)
	cc.srvs["s2"].Close() // s2 is down but gossip has not noticed yet
	ms, ls := consumeData(300, 60)
	if err := cc.router.Write(context.Background(), ms, ls); err != nil {
		t.Fatal(err)
	}
	series, _ := cc.placement(t)
	ring := cc.mem.Ring()
	var toS2 uint64
	for _, d := range ms {
		owners, _ := ring.GetN(d.ID(), 2)
		want := owners[0]
		if want == "s2" {
			toS2++
			want = owners[1]
		}
		if got := series[d.ID()]; len(got) != 1 || got[0] != want {
			t.Fatalf("series %d on %v, want only %s", d.ID(), got, want)
		}
	}
	st := cc.router.Stats()
	if toS2 == 0 || st.ForwardFailed < toS2 || st.Owned+st.Forwarded+st.KeptLocal != 360 {
		t.Fatalf("stats %+v (series owned by s2: %d)", st, toS2)
	}
	// replicas that are s1 itself count as kept_local, s3 as a failover forward
	if st.Failovers+st.KeptLocal != st.ForwardFailed {
		t.Fatalf("every failed record should land on its replica: %+v", st)
	}
}

func TestConsumeRouterKeepsLocalWhenEveryPeerIsDown(t *testing.T) {
	cc := newConsumeCluster(t)
	cc.srvs["s2"].Close()
	cc.srvs["s3"].Close()
	ms, ls := consumeData(200, 40)
	if err := cc.router.Write(context.Background(), ms, ls); err != nil {
		t.Fatal(err)
	}
	if got := len(seriesOn(t, cc.locals["s1"])); got != 200 {
		t.Fatalf("s1 holds %d series, want all 200", got)
	}
	if got := len(streamsOn(t, cc.locals["s1"])); got != 40 {
		t.Fatalf("s1 holds %d streams, want all 40", got)
	}
	st := cc.router.Stats()
	// owned, or kept_local after the forwards (both rounds) failed
	if st.Forwarded != 0 || st.Owned+st.KeptLocal != 240 || st.KeptLocal == 0 || st.ForwardFailed < st.KeptLocal {
		t.Fatalf("stats %+v", st)
	}
}

func TestConsumeRouterRetryRoundUsesCurrentRing(t *testing.T) {
	cc := newConsumeCluster(t)
	cc.srvs["s2"].Close()
	cc.srvs["s3"].Close()
	cc.router.RetryDelay = 20 * time.Millisecond
	// gossip notices s3 is dead during the pause; s2 recovers on a new address
	revived := httptest.NewServer(ShardHandler(cc.locals["s2"], nil, cc.mem, "sec", quiet))
	defer revived.Close()
	go func() {
		time.Sleep(5 * time.Millisecond)
		cc.mem.upsert(Member{Name: "s3", Role: "storage", State: "dead"})
		cc.mem.upsert(Member{Name: "s2", Role: "storage", HTTPAddr: strings.TrimPrefix(revived.URL, "http://"), State: "alive"})
	}()
	ms, _ := consumeData(200, 0)
	if err := cc.router.Write(context.Background(), ms, nil); err != nil {
		t.Fatal(err)
	}
	series, _ := cc.placement(t)
	if len(series) != 200 {
		t.Fatalf("%d series placed, want 200", len(series))
	}
	ring := cc.mem.Ring() // now s1 + s2
	for _, d := range ms {
		want, _ := ring.Get(d.ID())
		if got := series[d.ID()]; len(got) != 1 || (got[0] != want && got[0] != "s1") {
			t.Fatalf("series %d on %v, want %s", d.ID(), got, want)
		}
	}
	if st := cc.router.Stats(); st.Failovers == 0 {
		t.Fatalf("retry round delivered nothing: %+v", st)
	}
}

func TestConsumeRouterInterruptedLeavesBatchUncommitted(t *testing.T) {
	cc := newConsumeCluster(t)
	cc.srvs["s2"].Close()
	cc.srvs["s3"].Close()
	cc.router.RetryDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ms, _ := consumeData(100, 0)
	if err := cc.router.Write(ctx, ms, nil); err == nil {
		t.Fatal("want an error so the commit log does not commit the batch")
	}
	if st := cc.router.Stats(); st.KeptLocal != 0 {
		t.Fatalf("an interrupted batch must not be kept locally (it is redelivered): %+v", st)
	}
	if got := len(seriesOn(t, cc.locals["s1"])); got != 0 {
		t.Fatalf("s1 stored %d series of an interrupted batch", got)
	}
}

// countingProducer wraps the commit log to count every Publish call.
type countingProducer struct {
	*commitlog.Memory
	publishes atomic.Int64
}

func (p *countingProducer) Publish(ctx context.Context, recs []commitlog.Record) error {
	p.publishes.Add(1)
	return p.Memory.Publish(ctx, recs)
}

// The consumer drains the commit log through the router: forwarded records
// reach their owners and are never published to the log again.
func TestConsumeRouterNoRepublishLoop(t *testing.T) {
	cc := newConsumeCluster(t)
	log := &countingProducer{Memory: commitlog.NewMemory(10000)}
	log.SetRouteStats(cc.router.Stats)
	var tailed atomic.Int64
	cc.router.OnLogs = func(ls []model.LogEvent) { tailed.Add(int64(len(ls))) }
	ms, ls := consumeData(300, 60)
	var recs []commitlog.Record
	for i := range ms {
		recs = append(recs, commitlog.Record{Metric: &ms[i]})
	}
	for i := range ls {
		recs = append(recs, commitlog.Record{Log: &ls[i]})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = log.Run(ctx, cc.router.Handle); close(done) }()
	if err := log.Publish(ctx, recs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for log.Stats().Delivered < uint64(len(recs)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// give a would-be loop time to show up
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	st := log.Stats()
	if st.Published != uint64(len(recs)) || st.Delivered != uint64(len(recs)) || log.publishes.Load() != 1 {
		t.Fatalf("published %d delivered %d publish calls %d, want %d/%d/1", st.Published, st.Delivered, log.publishes.Load(), len(recs), len(recs))
	}
	if st.Routing == nil || st.Routing.Owned+st.Routing.Forwarded != uint64(len(recs)) {
		t.Fatalf("routing stats %+v", st.Routing)
	}
	if tailed.Load() != int64(len(ls)) {
		t.Fatalf("live tail saw %d log events, want %d", tailed.Load(), len(ls))
	}
	series, _ := cc.placement(t)
	for _, d := range ms {
		want, _ := cc.mem.Ring().Get(d.ID())
		if got := series[d.ID()]; len(got) != 1 || got[0] != want {
			t.Fatalf("series %d on %v, want %s", d.ID(), got, want)
		}
	}
}

func TestNewConsumeRouterNeedsLocalShard(t *testing.T) {
	mem := NewStatic(NodeMeta{Name: "q", Role: "query"}, nil, quiet)
	if _, err := NewConsumeRouter(NewEngine(mem, nil, "", quiet), quiet); err == nil {
		t.Fatal("want an error for a node without a shard")
	}
}
