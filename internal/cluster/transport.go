package cluster

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
	"github.com/himanshubaliyan7/zerowatch/internal/traces"
)

// SecretHeader carries the shared cluster secret (env ZW_CLUSTER_SECRET) on
// internal RPC between members.
const SecretHeader = "X-ZW-Cluster-Secret"

// MaxRawPoints caps the raw samples one shard returns for a percentile query.
// Past it the shard answers with its own percentile plus sample count and the
// coordinator merges approximately (see mergeFetch).
const MaxRawPoints = 100_000

// Internal RPC paths, served only by data-owning nodes.
const (
	pathWrite      = "/internal/v1/write"
	pathFetch      = "/internal/v1/fetch"
	pathSeries     = "/internal/v1/series"
	pathLogSearch  = "/internal/v1/logs/search"
	pathLogGroups  = "/internal/v1/logs/groups"
	pathShardStats = "/internal/v1/stats"
	maxRPCBody     = 64 << 20
)

// RawFetcher is implemented by engines that can return raw samples per
// period (storage.Memory). Without it percentiles merge approximately.
type RawFetcher interface {
	FetchRaw(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, periodSec, limit int) (map[int64][]float64, bool, error)
}

// ---- wire types

type writeReq struct {
	Metrics []model.MetricDatum `json:"metrics,omitempty"`
	Logs    []model.LogEvent    `json:"logs,omitempty"`
}

type empty struct{}

// fetch modes: what a shard must return so the coordinator can merge exactly.
const (
	modeStat = "stat" // Sum/Min/Max/SampleCount: same stat per shard, merged by +/min/max
	modeAgg  = "agg"  // Average: per-bucket (sum, count) so the mean is Σsum/Σcount
	modeRaw  = "raw"  // percentiles: raw values per bucket
)

type fetchReq struct {
	Tenant uint32           `json:"tenant"`
	Stat   query.MetricStat `json:"stat"`
	Start  int64            `json:"start"`
	End    int64            `json:"end"`
	Period int              `json:"period"`
	Mode   string           `json:"mode"`
}

// shardPoint is one bucket from one shard. Which fields are set depends on
// the mode: V (stat, and approximate raw), Sum+Count (agg), Raw (raw), Count
// (approximate raw: the sample count V was computed from).
type shardPoint struct {
	T     int64     `json:"t"`
	V     float64   `json:"v,omitempty"`
	Sum   float64   `json:"sum,omitempty"`
	Count float64   `json:"count,omitempty"`
	Raw   []float64 `json:"raw,omitempty"`
}

type fetchResp struct {
	Points []shardPoint `json:"points"`
	Raw    bool         `json:"raw"` // points carry Raw values (exact percentile possible)
}

type seriesReq struct {
	Tenant uint32               `json:"tenant"`
	Filter storage.SeriesFilter `json:"filter"`
}

type logSearchReq struct {
	Tenant uint32           `json:"tenant"` // LogQuery.TenantID is not serialised
	Query  storage.LogQuery `json:"query"`
}

type logGroupsReq struct {
	Tenant uint32 `json:"tenant"`
}

// ---- shard: the storage-side half of every RPC, over a local engine. The
// coordinator calls these directly for itself and over HTTP for peers, so
// both paths run the same code.

type shard struct {
	local  storage.Engine
	traces *traces.Store // nil when this node keeps no traces
}

func (s shard) write(ctx context.Context, req writeReq) (empty, error) {
	if len(req.Metrics) > 0 {
		if err := s.local.WriteMetrics(ctx, req.Metrics); err != nil && !errors.Is(err, storage.ErrCardinality) {
			return empty{}, err
		}
	}
	if len(req.Logs) > 0 {
		if err := s.local.WriteLogs(ctx, req.Logs); err != nil {
			return empty{}, err
		}
	}
	return empty{}, nil
}

func (s shard) fetch(ctx context.Context, req fetchReq) (fetchResp, error) {
	ms := req.Stat
	switch req.Mode {
	case modeAgg:
		ms.Stat = "Sum"
		sums, err := s.local.FetchStat(ctx, req.Tenant, ms, req.Start, req.End, req.Period)
		if err != nil {
			return fetchResp{}, err
		}
		ms.Stat = "SampleCount"
		counts, err := s.local.FetchStat(ctx, req.Tenant, ms, req.Start, req.End, req.Period)
		if err != nil {
			return fetchResp{}, err
		}
		byT := map[int64]float64{}
		for _, p := range counts {
			byT[p.T] = p.V
		}
		out := fetchResp{Points: make([]shardPoint, 0, len(sums))}
		for _, p := range sums {
			if c := byT[p.T]; c > 0 && !math.IsNaN(p.V) {
				out.Points = append(out.Points, shardPoint{T: p.T, Sum: p.V, Count: c})
			}
		}
		return out, nil
	case modeRaw:
		if rf, ok := s.local.(RawFetcher); ok {
			vals, truncated, err := rf.FetchRaw(ctx, req.Tenant, ms, req.Start, req.End, req.Period, MaxRawPoints)
			if err != nil {
				return fetchResp{}, err
			}
			// A truncated sample would bias the percentile towards whichever
			// series were scanned first, so fall through to the approximate
			// (local percentile, count) answer instead.
			if !truncated {
				out := fetchResp{Raw: true, Points: make([]shardPoint, 0, len(vals))}
				for t, v := range vals {
					out.Points = append(out.Points, shardPoint{T: t, Raw: v})
				}
				sort.Slice(out.Points, func(i, j int) bool { return out.Points[i].T < out.Points[j].T })
				return out, nil
			}
		}
		pcts, err := s.local.FetchStat(ctx, req.Tenant, ms, req.Start, req.End, req.Period)
		if err != nil {
			return fetchResp{}, err
		}
		cms := ms
		cms.Stat = "SampleCount"
		counts, err := s.local.FetchStat(ctx, req.Tenant, cms, req.Start, req.End, req.Period)
		if err != nil {
			return fetchResp{}, err
		}
		byT := map[int64]float64{}
		for _, p := range counts {
			byT[p.T] = p.V
		}
		out := fetchResp{Points: make([]shardPoint, 0, len(pcts))}
		for _, p := range pcts {
			if !math.IsNaN(p.V) {
				out.Points = append(out.Points, shardPoint{T: p.T, V: p.V, Count: byT[p.T]})
			}
		}
		return out, nil
	}
	pts, err := s.local.FetchStat(ctx, req.Tenant, ms, req.Start, req.End, req.Period)
	if err != nil {
		return fetchResp{}, err
	}
	out := fetchResp{Points: make([]shardPoint, 0, len(pts))}
	for _, p := range pts {
		if !math.IsNaN(p.V) { // JSON cannot carry NaN; a missing bucket means the same
			out.Points = append(out.Points, shardPoint{T: p.T, V: p.V})
		}
	}
	return out, nil
}

func (s shard) series(ctx context.Context, req seriesReq) ([]model.SeriesMeta, error) {
	return s.local.ListSeries(ctx, req.Tenant, req.Filter)
}

func (s shard) searchLogs(ctx context.Context, req logSearchReq) (storage.LogResult, error) {
	q := req.Query
	q.TenantID = req.Tenant
	return s.local.SearchLogs(ctx, q)
}

func (s shard) logGroups(ctx context.Context, req logGroupsReq) ([]storage.LogGroupInfo, error) {
	return s.local.ListLogGroups(ctx, req.Tenant)
}

func (s shard) stats(context.Context, empty) (storage.Stats, error) { return s.local.Stats(), nil }

// ---- HTTP server

// ShardHandler serves the internal RPC endpoints over a local engine.
// Requests must carry the shared secret when one is configured; without a
// secret only peers whose IP belongs to a live member are accepted, so a
// standalone port scan cannot write into or read out of the shard.
func ShardHandler(local storage.Engine, tr *traces.Store, mem *Membership, secret string, log *slog.Logger) http.Handler {
	s := shard{local: local, traces: tr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pathWrite, serveRPC(s.write))
	mux.HandleFunc("POST "+pathFetch, serveRPC(s.fetch))
	mux.HandleFunc("POST "+pathSeries, serveRPC(s.series))
	mux.HandleFunc("POST "+pathLogSearch, serveRPC(s.searchLogs))
	mux.HandleFunc("POST "+pathLogGroups, serveRPC(s.logGroups))
	mux.HandleFunc("POST "+pathShardStats, serveRPC(s.stats))
	mux.HandleFunc("POST "+pathTraceAdd, serveRPC(s.traceAdd))
	mux.HandleFunc("POST "+pathTraceGet, serveRPC(s.traceGet))
	mux.HandleFunc("POST "+pathTraceSearch, serveRPC(s.traceSearch))
	mux.HandleFunc("POST "+pathTraceStats, serveRPC(s.traceStats))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, mem, secret) {
			log.Warn("cluster: refused internal request", "path", r.URL.Path, "remote", r.RemoteAddr)
			httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "not a cluster member"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func authorized(r *http.Request, mem *Membership, secret string) bool {
	if secret != "" {
		return subtle.ConstantTimeCompare([]byte(r.Header.Get(SecretHeader)), []byte(secret)) == 1
	}
	// RemoteAddr, never X-Forwarded-For: the latter is caller-controlled.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && mem.HasIP(host)
}

func serveRPC[Req, Resp any](fn func(context.Context, Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if err := json.NewDecoder(io.LimitReader(r.Body, maxRPCBody)).Decode(&req); err != nil {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		resp, err := fn(r.Context(), req)
		if err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, storage.ErrBadStat) || errors.Is(err, query.ErrBadRequest) {
				code = http.StatusBadRequest
			}
			httpx.WriteJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

// ---- HTTP client

type rpcClient struct {
	mem    *Membership
	secret string
	http   *http.Client
}

// RPCError is a non-2xx answer from a peer.
type RPCError struct {
	Node   string
	Status int
	Msg    string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("cluster: %s answered %d: %s", e.Node, e.Status, e.Msg)
}

func (c *rpcClient) call(ctx context.Context, node, path string, req, resp any) error {
	m, ok := c.mem.Lookup(node)
	if !ok || m.HTTPAddr == "" {
		return fmt.Errorf("cluster: no http address for node %q", node)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+m.HTTPAddr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		hr.Header.Set(SecretHeader, c.secret)
	}
	res, err := c.http.Do(hr)
	if err != nil {
		return fmt.Errorf("cluster: %s: %w", node, err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = string(b)
		}
		return &RPCError{Node: node, Status: res.StatusCode, Msg: e.Error}
	}
	return json.NewDecoder(io.LimitReader(res.Body, maxRPCBody)).Decode(resp)
}

func newRPCClient(mem *Membership, secret string) *rpcClient {
	return &rpcClient{mem: mem, secret: secret, http: &http.Client{Timeout: 10 * time.Second}}
}
