package query

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/zerowatch/zerowatch/internal/model"
)

// MetricStat selects a stored series and the statistic to compute per period.
type MetricStat struct {
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metricName"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Stat       string            `json:"stat"` // Average|Sum|Minimum|Maximum|SampleCount|pNN
}

// MetricDataQuery is one entry of a GetMetricData request.
type MetricDataQuery struct {
	ID         string      `json:"id"`
	Label      string      `json:"label,omitempty"`
	Expression string      `json:"expression,omitempty"`
	MetricStat *MetricStat `json:"metricStat,omitempty"`
	ReturnData *bool       `json:"returnData,omitempty"`
}

// Request is a full GetMetricData request.
type Request struct {
	TenantID uint32            `json:"-"`
	Queries  []MetricDataQuery `json:"queries"`
	StartMs  int64             `json:"start"`
	EndMs    int64             `json:"end"`
	Period   int               `json:"period"` // seconds
	Fill     string            `json:"fill,omitempty"`
}

// Result is the evaluated output of one query id.
type Result struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	Timestamps []int64   `json:"timestamps"`
	Values     []float64 `json:"values"`
	StatusCode string    `json:"statusCode"` // Complete | PartialData | InternalError
	Messages   []string  `json:"messages,omitempty"`
}

// Fetcher retrieves per-period aggregated points for a metric stat. Points
// must already be aggregated by the requested statistic and period.
type Fetcher interface {
	FetchStat(ctx context.Context, tenant uint32, ms MetricStat, startMs, endMs int64, periodSec int) ([]model.Point, error)
}

// PartialError lets a Fetcher return the points it could gather together
// with why the rest is missing (e.g. a cluster storage shard was down). The
// engine reports such a query as PartialData instead of InternalError, and
// expressions built on it inherit PartialData.
type PartialError struct {
	Points []model.Point
	Msg    string
}

func (e *PartialError) Error() string { return e.Msg }

// ErrBadRequest wraps all client-side request errors.
var ErrBadRequest = errors.New("bad request")

func badReq(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

var idRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*$`)

// Plan is a validated, topologically sorted execution plan.
type Plan struct {
	Order []string
	ast   map[string]Node
	byID  map[string]*MetricDataQuery
}

// BuildPlan parses every expression, builds the dependency DAG and returns a
// topological order (Kahn's algorithm). Cycles and dangling references are
// rejected with ErrBadRequest.
func BuildPlan(queries []MetricDataQuery) (*Plan, error) {
	if len(queries) == 0 {
		return nil, badReq("at least one query is required")
	}
	if len(queries) > 500 {
		return nil, badReq("at most 500 queries per request")
	}
	p := &Plan{ast: map[string]Node{}, byID: map[string]*MetricDataQuery{}}
	ids := make([]string, 0, len(queries))
	for i := range queries {
		q := &queries[i]
		if !idRe.MatchString(q.ID) {
			return nil, badReq("invalid id %q: must start with a lowercase letter", q.ID)
		}
		if _, dup := p.byID[q.ID]; dup {
			return nil, badReq("duplicate id %q", q.ID)
		}
		if (q.Expression == "") == (q.MetricStat == nil) {
			return nil, badReq("query %q: exactly one of expression or metricStat required", q.ID)
		}
		p.byID[q.ID] = q
		ids = append(ids, q.ID)
	}
	deps := map[string][]string{} // id -> ids it depends on
	for _, id := range ids {
		q := p.byID[id]
		if q.Expression == "" {
			continue
		}
		n, err := Parse(q.Expression)
		if err != nil {
			return nil, badReq("query %q: %v", id, err)
		}
		p.ast[id] = n
		for _, r := range Refs(n) {
			if _, ok := p.byID[r]; !ok {
				return nil, badReq("query %q references unknown id %q", id, r)
			}
			if r == id {
				return nil, badReq("query %q references itself", id)
			}
			deps[id] = append(deps[id], r)
		}
	}
	// Kahn's algorithm, deterministic by original order.
	indeg := map[string]int{}
	rev := map[string][]string{}
	for _, id := range ids {
		indeg[id] = len(deps[id])
		for _, d := range deps[id] {
			rev[d] = append(rev[d], id)
		}
	}
	var queue []string
	for _, id := range ids {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		p.Order = append(p.Order, id)
		for _, nxt := range rev[id] {
			indeg[nxt]--
			if indeg[nxt] == 0 {
				queue = append(queue, nxt)
			}
		}
	}
	if len(p.Order) != len(ids) {
		var cyc []string
		for _, id := range ids {
			if indeg[id] > 0 {
				cyc = append(cyc, id)
			}
		}
		sort.Strings(cyc)
		return nil, badReq("dependency cycle detected among: %s", strings.Join(cyc, ", "))
	}
	return p, nil
}

// Engine executes GetMetricData requests.
type Engine struct {
	Fetcher Fetcher
	// MaxParallel bounds concurrent fetches (scatter); default 16.
	MaxParallel int
}

// Execute runs the request: scatter metric fetches in parallel, align onto the
// grid, then evaluate expressions in topological order.
func (e *Engine) Execute(ctx context.Context, req Request) ([]Result, error) {
	plan, err := BuildPlan(req.Queries)
	if err != nil {
		return nil, err
	}
	grid, err := NewGrid(req.StartMs, req.EndMs, req.Period)
	if err != nil {
		return nil, badReq("%v", err)
	}
	fill, err := ParseFillPolicy(req.Fill)
	if err != nil {
		return nil, badReq("%v", err)
	}
	env := &Env{Grid: grid, Values: map[string]Value{}}
	status := map[string]string{}
	msgs := map[string][]string{}

	// ---- scatter: fetch all metric stats concurrently
	par := e.MaxParallel
	if par <= 0 {
		par = 16
	}
	sem := make(chan struct{}, par)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range plan.Order {
		q := plan.byID[id]
		if q.MetricStat == nil {
			continue
		}
		wg.Add(1)
		go func(id string, ms MetricStat) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pts, ferr := e.Fetcher.FetchStat(ctx, req.TenantID, ms, grid.Start, grid.Start+int64(grid.N)*grid.PeriodMs, req.Period)
			mu.Lock()
			defer mu.Unlock()
			var pe *PartialError
			if errors.As(ferr, &pe) {
				status[id] = "PartialData"
				msgs[id] = append(msgs[id], pe.Msg)
				env.Values[id] = VecV(Fill(Align(pe.Points, grid), fill))
				return
			}
			if ferr != nil {
				status[id] = "InternalError"
				msgs[id] = append(msgs[id], ferr.Error())
				env.Values[id] = VecV(nanVec(grid.N))
				return
			}
			env.Values[id] = VecV(Fill(Align(pts, grid), fill))
			status[id] = "Complete"
		}(id, *q.MetricStat)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ---- evaluate expressions in DAG order
	for _, id := range plan.Order {
		n, ok := plan.ast[id]
		if !ok {
			continue
		}
		v, eerr := Eval(n, env)
		if eerr != nil {
			var ee *EvalError
			if errors.As(eerr, &ee) {
				return nil, badReq("query %q: %v", id, eerr)
			}
			return nil, eerr
		}
		env.Values[id] = v
		status[id] = "Complete"
		for _, r := range Refs(n) {
			if status[r] == "InternalError" || status[r] == "PartialData" {
				status[id] = "PartialData"
			}
		}
	}

	ts := grid.Timestamps()
	var out []Result
	for _, q := range req.Queries {
		if q.ReturnData != nil && !*q.ReturnData {
			continue
		}
		vals := env.Values[q.ID].Materialize(grid.N)
		r := Result{ID: q.ID, Label: q.Label, StatusCode: status[q.ID], Messages: msgs[q.ID]}
		if r.Label == "" {
			r.Label = q.ID
			if q.MetricStat != nil {
				r.Label = q.MetricStat.MetricName
			}
		}
		// CloudWatch omits missing datapoints rather than returning nulls.
		for i, v := range vals {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			r.Timestamps = append(r.Timestamps, ts[i])
			r.Values = append(r.Values, v)
		}
		if r.Timestamps == nil {
			r.Timestamps, r.Values = []int64{}, []float64{}
		}
		out = append(out, r)
	}
	return out, nil
}

func nanVec(n int) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = math.NaN()
	}
	return v
}
