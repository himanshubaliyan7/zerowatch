package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/query"
	"github.com/zerowatch/zerowatch/internal/storage"
	"github.com/zerowatch/zerowatch/internal/traces"
)

// ErrNoStorage is returned when the ring has no data-owning members.
var ErrNoStorage = errors.New("cluster: no storage nodes on the ring")

// Engine is a storage.Engine over the cluster. Writes go to the ring owner of
// each series (metrics) or stream (logs), failing over once to the next
// replica on the ring; reads fan out to every data-owning member and merge.
// Each record is written to exactly one node, so merged reads never double
// count; a series written during a failover simply lives on two shards and
// the merge combines them.
//
// When this process itself owns data (role=all in a cluster), Local serves
// its own shard in-process instead of over HTTP.
type Engine struct {
	mem    *Membership
	local  storage.Engine // nil unless this node is on the ring
	traces *traces.Store  // this node's trace shard; nil unless on the ring
	client *rpcClient
	log    *slog.Logger

	forwarded, failovers, dropped, approxPct, partialReads atomic.Uint64
}

// NewEngine builds the cluster engine. local may be nil.
func NewEngine(mem *Membership, local storage.Engine, secret string, log *slog.Logger) *Engine {
	return &Engine{mem: mem, local: local, client: newRPCClient(mem, secret), log: log}
}

// RoutingStats are exposed on /api/v1/cluster.
type RoutingStats struct {
	Forwarded     uint64 `json:"forwarded"` // records delivered to an owner
	Failovers     uint64 `json:"failovers"` // records re-sent to the next replica
	Dropped       uint64 `json:"dropped"`   // records no replica accepted
	ApproxPctMrgs uint64 `json:"approxPercentileMerges"`
	PartialReads  uint64 `json:"partialReads"` // FetchStat answered without some shards
}

// RoutingStats returns write/read routing counters.
func (e *Engine) RoutingStats() RoutingStats {
	return RoutingStats{Forwarded: e.forwarded.Load(), Failovers: e.failovers.Load(), Dropped: e.dropped.Load(), ApproxPctMrgs: e.approxPct.Load(), PartialReads: e.partialReads.Load()}
}

// rpc runs fn on node: in-process for this node's own shard, else over HTTP.
func rpc[Req, Resp any](ctx context.Context, e *Engine, node, path string, req Req, fn func(shard, context.Context, Req) (Resp, error)) (Resp, error) {
	if e.local != nil && node == e.mem.Self().Name {
		return fn(shard{local: e.local, traces: e.traces}, ctx, req)
	}
	var resp Resp
	err := e.client.call(ctx, node, path, req, &resp)
	return resp, err
}

// gather fans req out to every data-owning node. Without partial, any shard
// failure fails the whole read: those APIs have no way to flag a partial
// answer, and a silently partial one would look like a real drop in the data.
// With partial, the successful shards' answers come back together with a
// *PartialError naming the failed ones (all failing is still an error).
func gather[Req, Resp any](ctx context.Context, e *Engine, path string, partial bool, req Req, fn func(shard, context.Context, Req) (Resp, error)) ([]Resp, error) {
	nodes := e.mem.Ring().Nodes()
	if len(nodes) == 0 {
		return nil, ErrNoStorage
	}
	c := &Coordinator[Req, []Resp]{Client: ShardClientFunc[Req, []Resp](func(ctx context.Context, node string, req Req) ([]Resp, error) {
		r, err := rpc(ctx, e, node, path, req, fn)
		if err != nil {
			return nil, err
		}
		return []Resp{r}, nil
	}), MaxParallel: 16, AllowPartial: partial}
	return c.ScatterGather(ctx, nodes, req, func(parts [][]Resp) []Resp {
		var out []Resp
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	})
}

// ---- writes

// route groups record indexes by ring owner, sends each group, and re-sends a
// failed group's records to their next replica. It returns an error only when
// nothing was delivered: the commit log retries a failed batch, and a retry
// after a partial success would duplicate the delivered records.
func (e *Engine) route(ctx context.Context, n int, key func(i int) uint64, send func(ctx context.Context, node string, idx []int) error) error {
	if n == 0 {
		return nil
	}
	ring := e.mem.Ring()
	primary := map[string][]int{}
	alt := make([]string, n)
	for i := 0; i < n; i++ {
		owners, err := ring.GetN(key(i), 2)
		if err != nil {
			return ErrNoStorage
		}
		primary[owners[0]] = append(primary[owners[0]], i)
		if len(owners) > 1 {
			alt[i] = owners[1]
		}
	}
	failedIdx, firstErr := e.sendGroups(ctx, primary, send)
	if len(failedIdx) > 0 {
		retry := map[string][]int{}
		var orphans int
		for _, i := range failedIdx {
			if alt[i] == "" {
				orphans++
				continue
			}
			retry[alt[i]] = append(retry[alt[i]], i)
		}
		e.failovers.Add(uint64(len(failedIdx) - orphans))
		still, _ := e.sendGroups(ctx, retry, send)
		lost := orphans + len(still)
		if lost > 0 {
			e.dropped.Add(uint64(lost))
			e.log.Warn("cluster: records dropped, no replica accepted them", "records", lost, "err", firstErr)
			if lost == n {
				return fmt.Errorf("cluster: write failed on every replica: %w", firstErr)
			}
		}
	}
	return nil
}

func (e *Engine) sendGroups(ctx context.Context, groups map[string][]int, send func(context.Context, string, []int) error) ([]int, error) {
	var (
		mu       sync.Mutex
		failed   []int
		firstErr error
		wg       sync.WaitGroup
	)
	for node, idx := range groups {
		wg.Add(1)
		go func(node string, idx []int) {
			defer wg.Done()
			if err := send(ctx, node, idx); err != nil {
				mu.Lock()
				failed = append(failed, idx...)
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			e.forwarded.Add(uint64(len(idx)))
		}(node, idx)
	}
	wg.Wait()
	return failed, firstErr
}

// WriteMetrics routes each datum to the owner of its series ID.
func (e *Engine) WriteMetrics(ctx context.Context, ms []model.MetricDatum) error {
	return e.route(ctx, len(ms), func(i int) uint64 { return ms[i].ID() }, func(ctx context.Context, node string, idx []int) error {
		req := writeReq{Metrics: make([]model.MetricDatum, len(idx))}
		for j, i := range idx {
			req.Metrics[j] = ms[i]
		}
		_, err := rpc(ctx, e, node, pathWrite, req, shard.write)
		return err
	})
}

// LogStreamKey places a log stream on the ring; a stream stays on one shard so
// its events keep their order.
func LogStreamKey(e *model.LogEvent) uint64 {
	return xxh3.HashString(strconv.FormatUint(uint64(e.TenantID), 10) + "\xff" + e.LogGroup + "\xff" + e.LogStream)
}

// WriteLogs routes each event to the owner of its (tenant, group, stream).
func (e *Engine) WriteLogs(ctx context.Context, ls []model.LogEvent) error {
	return e.route(ctx, len(ls), func(i int) uint64 { return LogStreamKey(&ls[i]) }, func(ctx context.Context, node string, idx []int) error {
		req := writeReq{Logs: make([]model.LogEvent, len(idx))}
		for j, i := range idx {
			req.Logs[j] = ls[i]
		}
		_, err := rpc(ctx, e, node, pathWrite, req, shard.write)
		return err
	})
}

// ---- reads

// FetchStat implements query.Fetcher by scatter-gather. Each statistic is
// requested in a form that merges exactly across shards:
//   - Sum, SampleCount: per-bucket values add up.
//   - Minimum, Maximum: min of mins, max of maxes.
//   - Average: shards return (sum, count) per bucket and the mean is
//     Σsum/Σcount; averaging per-shard averages would weight small shards wrong.
//   - pNN: shards return the raw samples of each bucket (capped at
//     MaxRawPoints) and the percentile is computed here over all of them.
//     A shard over the cap, or one without raw access, returns its own
//     percentile and sample count instead; when such a bucket has data on
//     more than one shard the result is a count-weighted mean of percentiles
//     (approximate; counted in RoutingStats.ApproxPctMrgs).
//
// A shard that fails yields a *query.PartialError carrying the other shards'
// merged points, which query.Engine reports as PartialData.
func (e *Engine) FetchStat(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, periodSec int) ([]model.Point, error) {
	st, err := storage.ParseStat(ms.Stat)
	if err != nil {
		return nil, err
	}
	mode := modeStat
	switch st.Kind {
	case "avg":
		mode = modeAgg
	case "pct":
		mode = modeRaw
	}
	resps, err := gather(ctx, e, pathFetch, true, fetchReq{Tenant: tenant, Stat: ms, Start: startMs, End: endMs, Period: periodSec, Mode: mode}, shard.fetch)
	var pe *PartialError
	if err != nil && (!errors.As(err, &pe) || len(resps) == 0) {
		return nil, err
	}
	pts, approx := mergeFetch(st, resps)
	if approx {
		e.approxPct.Add(1)
	}
	if pe != nil {
		// query.Engine reports this as PartialData with the message, so a
		// dashboard shows the gap's cause instead of a fake drop to zero.
		e.partialReads.Add(1)
		return pts, &query.PartialError{Points: pts, Msg: partialMsg(pe, len(resps)+len(pe.Failed))}
	}
	return pts, nil
}

// gatherPartial is gather with partial answers for the list/search APIs:
// with some shards down it returns the others' answers and a
// *storage.PartialError naming the missing shards. resps is nil only when
// nothing answered, and err is then the failure itself.
func gatherPartial[Req, Resp any](ctx context.Context, e *Engine, path string, req Req, fn func(shard, context.Context, Req) (Resp, error)) ([]Resp, error) {
	resps, err := gather(ctx, e, path, true, req, fn)
	var pe *PartialError
	switch {
	case err == nil:
		return resps, nil
	case errors.As(err, &pe) && len(resps) > 0:
		e.partialReads.Add(1)
		return resps, &storage.PartialError{Msg: partialMsg(pe, len(resps)+len(pe.Failed))}
	}
	return nil, err
}

func partialMsg(pe *PartialError, total int) string {
	names := make([]string, 0, len(pe.Failed))
	for n := range pe.Failed {
		names = append(names, n)
	}
	sort.Strings(names)
	msg := fmt.Sprintf("cluster: %d of %d storage shards unavailable:", len(names), total)
	for _, n := range names {
		msg += fmt.Sprintf(" %s (%v)", n, pe.Failed[n])
	}
	return msg
}

// mergeFetch combines shard answers; approx reports a weighted percentile merge.
func mergeFetch(st storage.Stat, resps []fetchResp) ([]model.Point, bool) {
	type acc struct {
		v, sum, count float64
		n             int // shards contributing
		raw           []float64
		wsum, wcount  float64 // approximate percentile: Σ(v·count), Σcount
		approx        bool
	}
	buckets := map[int64]*acc{}
	get := func(t int64) *acc {
		a := buckets[t]
		if a == nil {
			a = &acc{}
			buckets[t] = a
		}
		return a
	}
	for _, r := range resps {
		for _, p := range r.Points {
			a := get(p.T)
			a.n++
			switch st.Kind {
			case "sum", "count":
				a.v += p.V
			case "min":
				if a.n == 1 || p.V < a.v {
					a.v = p.V
				}
			case "max":
				if a.n == 1 || p.V > a.v {
					a.v = p.V
				}
			case "avg":
				a.sum += p.Sum
				a.count += p.Count
			case "pct":
				if r.Raw {
					a.raw = append(a.raw, p.Raw...)
				} else {
					a.approx = true
					a.wsum += p.V * p.Count
					a.wcount += p.Count
					a.v = p.V
				}
			}
		}
	}
	approxUsed := false
	out := make([]model.Point, 0, len(buckets))
	for t, a := range buckets {
		v := a.v
		switch st.Kind {
		case "avg":
			if a.count == 0 {
				continue
			}
			v = a.sum / a.count
		case "pct":
			switch {
			case !a.approx:
				v = st.Reduce(a.raw)
			case a.n == 1:
				// the only shard with data answered itself: exact
			default:
				approxUsed = true
				if len(a.raw) > 0 {
					a.wsum += st.Reduce(a.raw) * float64(len(a.raw))
					a.wcount += float64(len(a.raw))
				}
				if a.wcount == 0 {
					continue
				}
				v = a.wsum / a.wcount
			}
		}
		if math.IsNaN(v) {
			continue
		}
		out = append(out, model.Point{T: t, V: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, approxUsed
}

// ListSeries merges every shard's series, deduplicated by series ID (a series
// written during a failover exists on two shards), in the Memory engine's
// order, then applies the limit.
func (e *Engine) ListSeries(ctx context.Context, tenant uint32, f storage.SeriesFilter) ([]model.SeriesMeta, error) {
	resps, perr := gatherPartial(ctx, e, pathSeries, seriesReq{Tenant: tenant, Filter: f}, shard.series)
	if resps == nil {
		return nil, perr
	}
	seen := map[uint64]bool{}
	out := []model.SeriesMeta{}
	for _, r := range resps {
		for _, s := range r {
			if !seen[s.ID] {
				seen[s.ID] = true
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.MetricName != b.MetricName {
			return a.MetricName < b.MetricName
		}
		return a.ID < b.ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, perr
}

// SearchLogs asks every shard for its newest `limit` matches and keeps the
// newest `limit` overall, which is exactly the global top-limit.
func (e *Engine) SearchLogs(ctx context.Context, q storage.LogQuery) (storage.LogResult, error) {
	limit := q.Limit
	if limit <= 0 || limit > 10_000 {
		limit = 500 // storage.Memory's default
	}
	q.Limit = limit
	resps, perr := gatherPartial(ctx, e, pathLogSearch, logSearchReq{Tenant: q.TenantID, Query: q}, shard.searchLogs)
	if resps == nil {
		return storage.LogResult{}, perr
	}
	res := storage.LogResult{Events: []model.LogEvent{}}
	var pe *storage.PartialError
	if errors.As(perr, &pe) {
		res.Partial = pe.Msg
	}
	for _, r := range resps {
		res.Events = append(res.Events, r.Events...)
		res.Scanned += r.Scanned
		res.Truncated = res.Truncated || r.Truncated
	}
	sort.SliceStable(res.Events, func(i, j int) bool { return res.Events[i].Timestamp > res.Events[j].Timestamp })
	if len(res.Events) > limit {
		res.Events = res.Events[:limit]
		res.Truncated = true
	}
	return res, nil
}

// ListLogGroups merges groups by name (a group's streams spread over shards).
func (e *Engine) ListLogGroups(ctx context.Context, tenant uint32) ([]storage.LogGroupInfo, error) {
	resps, perr := gatherPartial(ctx, e, pathLogGroups, logGroupsReq{Tenant: tenant}, shard.logGroups)
	if resps == nil {
		return nil, perr
	}
	byName := map[string]*storage.LogGroupInfo{}
	streams := map[string]map[string]bool{}
	for _, r := range resps {
		for _, g := range r {
			cur := byName[g.Name]
			if cur == nil {
				cur = &storage.LogGroupInfo{Name: g.Name}
				byName[g.Name] = cur
				streams[g.Name] = map[string]bool{}
			}
			cur.Events += g.Events
			if g.LastEvent > cur.LastEvent {
				cur.LastEvent = g.LastEvent
			}
			for _, s := range g.Streams {
				streams[g.Name][s] = true
			}
		}
	}
	out := make([]storage.LogGroupInfo, 0, len(byName))
	for name, g := range byName {
		g.Streams = make([]string, 0, len(streams[name]))
		for s := range streams[name] {
			g.Streams = append(g.Streams, s)
		}
		sort.Strings(g.Streams)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if perr != nil {
		return out, perr
	}
	return out, nil
}

// Stats sums the shards' stats. It is informational, so unreachable shards
// are skipped rather than failing /api/v1/stats.
func (e *Engine) Stats() storage.Stats {
	out := storage.Stats{Backend: "cluster"}
	nodes := e.mem.Ring().Nodes()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := &Coordinator[empty, storage.Stats]{AllowPartial: true, Timeout: 2 * time.Second,
		Client: ShardClientFunc[empty, storage.Stats](func(ctx context.Context, node string, req empty) (storage.Stats, error) {
			return rpc(ctx, e, node, pathShardStats, req, shard.stats)
		})}
	if len(nodes) == 0 {
		return out
	}
	merged, _ := c.ScatterGather(ctx, nodes, empty{}, func(parts []storage.Stats) storage.Stats {
		var s storage.Stats
		for _, p := range parts {
			s.Series += p.Series
			s.Samples += p.Samples
			s.ChunkBytes += p.ChunkBytes
			s.RollupBucket += p.RollupBucket
			s.LogEvents += p.LogEvents
		}
		return s
	})
	merged.Backend = out.Backend
	if merged.Samples > 0 {
		merged.BytesPerPt = float64(merged.ChunkBytes) / float64(merged.Samples)
	}
	return merged
}

// Close is a no-op: the local engine (if any) is owned and closed by main.
func (e *Engine) Close() error { return nil }

var _ storage.Engine = (*Engine)(nil)
