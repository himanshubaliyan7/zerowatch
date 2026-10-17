package cluster

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"

	"github.com/himanshubaliyan7/zerowatch/internal/traces"
)

const (
	pathTraceAdd    = "/internal/v1/traces/add"
	pathTraceGet    = "/internal/v1/traces/get"
	pathTraceSearch = "/internal/v1/traces/search"
	pathTraceStats  = "/internal/v1/traces/stats"
)

// traceTimeout bounds a trace RPC (the Backend API has no context).
const traceTimeout = 10 * time.Second

type traceAddReq struct {
	Tenant uint32        `json:"tenant"`
	Spans  []traces.Span `json:"spans"`
}

type traceGetReq struct {
	Tenant  uint32 `json:"tenant"`
	TraceID string `json:"traceId"`
}

type traceGetResp struct {
	Found  bool               `json:"found"`
	Detail traces.TraceDetail `json:"detail"`
}

type traceSearchReq struct {
	Tenant uint32       `json:"tenant"`
	Query  traces.Query `json:"query"`
}

var errNoTraceStore = errors.New("cluster: this node keeps no traces")

func (s shard) traceStore() (*traces.Store, error) {
	if s.traces == nil {
		return nil, errNoTraceStore
	}
	return s.traces, nil
}

func (s shard) traceAdd(_ context.Context, req traceAddReq) (int, error) {
	t, err := s.traceStore()
	if err != nil {
		return 0, err
	}
	return t.Add(req.Tenant, req.Spans), nil
}

func (s shard) traceGet(_ context.Context, req traceGetReq) (traceGetResp, error) {
	t, err := s.traceStore()
	if err != nil {
		return traceGetResp{}, err
	}
	d, ok := t.Get(req.Tenant, req.TraceID)
	return traceGetResp{Found: ok, Detail: d}, nil
}

func (s shard) traceSearch(_ context.Context, req traceSearchReq) ([]traces.TraceSummary, error) {
	t, err := s.traceStore()
	if err != nil {
		return nil, err
	}
	return t.Search(req.Tenant, req.Query), nil
}

func (s shard) traceStats(context.Context, empty) (traces.Stats, error) {
	t, err := s.traceStore()
	if err != nil {
		return traces.Stats{}, err
	}
	return t.Stats(), nil
}

// TraceRouter is a traces.Backend over the cluster: every span of a trace goes
// to the trace's ring owner (keyed by tenant and trace id), failing over to
// the next replica, so a trace is whole on one node and visible from any
// node, whatever role received it.
type TraceRouter struct{ e *Engine }

var _ traces.Backend = (*TraceRouter)(nil)

func traceKey(tenant uint32, id string) uint64 {
	return xxh3.HashString(strconv.FormatUint(uint64(tenant), 10) + "/" + id)
}

// Add routes spans by trace and returns how many the owners kept.
func (t *TraceRouter) Add(tenant uint32, spans []traces.Span) int {
	ctx, cancel := context.WithTimeout(context.Background(), traceTimeout)
	defer cancel()
	var kept atomic.Int64
	err := t.e.route(ctx, len(spans), func(i int) uint64 { return traceKey(tenant, spans[i].TraceID) }, func(ctx context.Context, node string, idx []int) error {
		batch := make([]traces.Span, len(idx))
		for j, i := range idx {
			batch[j] = spans[i]
		}
		k, err := rpc(ctx, t.e, node, pathTraceAdd, traceAddReq{Tenant: tenant, Spans: batch}, shard.traceAdd)
		kept.Add(int64(k))
		return err
	})
	if err != nil {
		t.e.log.Warn("cluster: spans not stored", "spans", len(spans), "err", err)
	}
	return int(kept.Load())
}

// Get asks the trace's owner and its replica (a failover can split a trace
// between them) and merges their spans.
func (t *TraceRouter) Get(tenant uint32, traceID string) (traces.TraceDetail, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), traceTimeout)
	defer cancel()
	owners, err := t.e.mem.Ring().GetN(traceKey(tenant, traceID), 2)
	if err != nil {
		return traces.TraceDetail{}, false
	}
	var out traces.TraceDetail
	found := false
	seen := map[string]bool{}
	for _, node := range owners {
		r, err := rpc(ctx, t.e, node, pathTraceGet, traceGetReq{Tenant: tenant, TraceID: traceID}, shard.traceGet)
		if err != nil || !r.Found {
			continue
		}
		found = true
		out.TraceID = r.Detail.TraceID
		for _, sp := range r.Detail.Spans {
			if !seen[sp.SpanID] {
				seen[sp.SpanID] = true
				out.Spans = append(out.Spans, sp)
			}
		}
	}
	sort.SliceStable(out.Spans, func(a, b int) bool { return out.Spans[a].Start < out.Spans[b].Start })
	return out, found
}

// Search fans out to every data node and merges newest first; a trace split
// by a failover appears once, with the summary that has more spans. Nodes
// that do not answer are skipped (the Backend API has no partial flag).
func (t *TraceRouter) Search(tenant uint32, q traces.Query) []traces.TraceSummary {
	ctx, cancel := context.WithTimeout(context.Background(), traceTimeout)
	defer cancel()
	resps, err := gather(ctx, t.e, pathTraceSearch, true, traceSearchReq{Tenant: tenant, Query: q}, shard.traceSearch)
	if err != nil && len(resps) == 0 {
		t.e.log.Warn("cluster: trace search failed", "err", err)
		return []traces.TraceSummary{}
	}
	best := map[string]traces.TraceSummary{}
	for _, r := range resps {
		for _, s := range r {
			if cur, ok := best[s.TraceID]; !ok || s.Spans > cur.Spans {
				best[s.TraceID] = s
			}
		}
	}
	out := make([]traces.TraceSummary, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start > out[j].Start
		}
		return out[i].TraceID < out[j].TraceID
	})
	limit := q.Limit
	if limit <= 0 {
		limit = traces.DefaultSearchLimit
	}
	if len(out) > min(limit, traces.MaxSearchLimit) {
		out = out[:min(limit, traces.MaxSearchLimit)]
	}
	return out
}

// Stats sums the data nodes' trace stats (best effort, like Engine.Stats).
func (t *TraceRouter) Stats() traces.Stats {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resps, _ := gather(ctx, t.e, pathTraceStats, true, empty{}, shard.traceStats)
	var out traces.Stats
	for _, s := range resps {
		out.Tenants = max(out.Tenants, s.Tenants)
		out.Traces += s.Traces
		out.Spans += s.Spans
		out.Accepted += s.Accepted
		out.Dropped += s.Dropped
		out.Evicted += s.Evicted
	}
	return out
}
