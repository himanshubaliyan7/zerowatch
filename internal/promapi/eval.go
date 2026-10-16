package promapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/query"
)

// promError carries a Prometheus errorType.
type promError struct {
	typ string // bad_data | execution | timeout | canceled | internal
	msg string
}

func (e *promError) Error() string { return e.msg }

func badData(format string, a ...any) error {
	return &promError{typ: "bad_data", msg: fmt.Sprintf(format, a...)}
}

func execErr(format string, a ...any) error {
	return &promError{typ: "execution", msg: fmt.Sprintf(format, a...)}
}

// ---- values ----

// vseries is one series of an instant vector evaluated at every step; ok
// says at which steps it has a sample.
type vseries struct {
	labels map[string]string
	vals   []float64
	ok     []bool
}

type vector []*vseries
type scalar []float64

// rawSeries is a selector's stored samples (timestamps are bucket starts).
type rawSeries struct {
	labels map[string]string
	pts    []model.Point
}

type matrix struct {
	series []rawSeries
	rng    int64 // ms
	off    int64 // ms
}

type strVal string

// ---- functions ----

type funcDef struct {
	args     []valType
	optional int
	ret      valType
}

var (
	vec = typVector
	mat = typMatrix
	sca = typScalar
)

var functions = map[string]funcDef{
	"rate": {args: []valType{mat}, ret: vec}, "irate": {args: []valType{mat}, ret: vec},
	"increase": {args: []valType{mat}, ret: vec}, "delta": {args: []valType{mat}, ret: vec},
	"avg_over_time": {args: []valType{mat}, ret: vec}, "sum_over_time": {args: []valType{mat}, ret: vec},
	"min_over_time": {args: []valType{mat}, ret: vec}, "max_over_time": {args: []valType{mat}, ret: vec},
	"count_over_time": {args: []valType{mat}, ret: vec}, "last_over_time": {args: []valType{mat}, ret: vec},
	"stddev_over_time": {args: []valType{mat}, ret: vec}, "stdvar_over_time": {args: []valType{mat}, ret: vec},
	"present_over_time":  {args: []valType{mat}, ret: vec},
	"quantile_over_time": {args: []valType{sca, mat}, ret: vec},
	"abs":                {args: []valType{vec}, ret: vec}, "ceil": {args: []valType{vec}, ret: vec},
	"floor": {args: []valType{vec}, ret: vec}, "sqrt": {args: []valType{vec}, ret: vec},
	"exp": {args: []valType{vec}, ret: vec}, "ln": {args: []valType{vec}, ret: vec},
	"log2": {args: []valType{vec}, ret: vec}, "log10": {args: []valType{vec}, ret: vec},
	"sgn":       {args: []valType{vec}, ret: vec},
	"round":     {args: []valType{vec, sca}, optional: 1, ret: vec},
	"clamp":     {args: []valType{vec, sca, sca}, ret: vec},
	"clamp_min": {args: []valType{vec, sca}, ret: vec}, "clamp_max": {args: []valType{vec, sca}, ret: vec},
	"scalar": {args: []valType{vec}, ret: sca}, "vector": {args: []valType{sca}, ret: vec},
	"time": {ret: sca}, "absent": {args: []valType{vec}, ret: vec},
	"sort": {args: []valType{vec}, ret: vec}, "sort_desc": {args: []valType{vec}, ret: vec},
}

// unsupportedFuncs are real PromQL functions outside the subset; they get a
// clear "unsupported" error instead of "unknown function".
var unsupportedFuncs = map[string]bool{
	"histogram_quantile": true, "histogram_count": true, "histogram_sum": true, "histogram_fraction": true,
	"label_replace": true, "label_join": true, "predict_linear": true, "deriv": true, "holt_winters": true,
	"double_exponential_smoothing": true, "changes": true, "resets": true, "idelta": true, "timestamp": true,
	"absent_over_time": true, "day_of_month": true, "day_of_week": true, "day_of_year": true, "days_in_month": true,
	"hour": true, "minute": true, "month": true, "year": true, "sort_by_label": true, "sort_by_label_desc": true,
	"mad_over_time": true, "info": true, "sin": true, "cos": true, "tan": true, "asin": true, "acos": true,
	"atan": true, "sinh": true, "cosh": true, "tanh": true, "asinh": true, "acosh": true, "atanh": true,
	"deg": true, "rad": true, "pi": true,
}

// ---- evaluator ----

type evaluator struct {
	ctx       context.Context
	st        Store
	tenant    uint32
	cat       *catalog
	ts        []int64 // evaluation timestamps, ms
	lookback  int64
	res       int64 // fetch resolution, ms (a multiple of 60 s)
	maxSeries int
	maxSample int64

	mu       sync.Mutex
	samples  int64
	warnings []string
}

func (ev *evaluator) warn(s string) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	for _, w := range ev.warnings {
		if w == s {
			return
		}
	}
	ev.warnings = append(ev.warnings, s)
}

func (ev *evaluator) n() int { return len(ev.ts) }

func (ev *evaluator) eval(e Expr) (any, error) {
	if err := ev.ctx.Err(); err != nil {
		return nil, err
	}
	switch x := e.(type) {
	case NumberLit:
		s := make(scalar, ev.n())
		for i := range s {
			s[i] = x.V
		}
		return s, nil
	case StringLit:
		return strVal(x.S), nil
	case *Paren:
		return ev.eval(x.Expr)
	case *VectorSel:
		raw, err := ev.fetch(x, ev.lookback)
		if err != nil {
			return nil, err
		}
		return ev.instant(raw, x.Offset.Milliseconds()), nil
	case *MatrixSel:
		raw, err := ev.fetch(x.VS, x.Range.Milliseconds())
		if err != nil {
			return nil, err
		}
		return &matrix{series: raw, rng: x.Range.Milliseconds(), off: x.VS.Offset.Milliseconds()}, nil
	case *Unary:
		v, err := ev.eval(x.Expr)
		if err != nil || x.Op == "+" {
			return v, err
		}
		switch t := v.(type) {
		case scalar:
			out := make(scalar, len(t))
			for i, f := range t {
				out[i] = -f
			}
			return out, nil
		case vector:
			return mapVec(t, func(f float64) float64 { return -f }), nil
		}
		return nil, badData("unary minus needs a scalar or an instant vector")
	case *Call:
		return ev.call(x)
	case *Agg:
		return ev.aggregate(x)
	case *Binary:
		return ev.binary(x)
	}
	return nil, badData("unsupported expression")
}

// fetch loads the samples of every series a selector matches, covering
// every step's window of length window.
func (ev *evaluator) fetch(vs *VectorSel, window int64) ([]rawSeries, error) {
	entries := ev.cat.match(vs.Matchers)
	stats, err := statsFor(vs.Matchers)
	if err != nil {
		return nil, err
	}
	total := len(entries) * len(stats)
	if total > ev.maxSeries {
		return nil, execErr("selector matches %d series; at most %d are allowed per selector, narrow the matchers", total, ev.maxSeries)
	}
	off := vs.Offset.Milliseconds()
	from := ev.ts[0] - off - window
	// A sample is labelled with its bucket start and covers the whole bucket,
	// so the newest bucket may include data up to one resolution after t
	// (the rollup tier cannot split a minute either).
	to := ev.ts[len(ev.ts)-1] - off + ev.res
	out := make([]rawSeries, total)
	errs := make([]error, total)
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, e := range entries {
		for j, stat := range stats {
			k := i*len(stats) + j
			wg.Add(1)
			sem <- struct{}{}
			go func(k int, e entry, stat string) {
				defer func() { <-sem; wg.Done() }()
				ms := query.MetricStat{Namespace: e.meta.Namespace, MetricName: e.meta.MetricName, Dimensions: e.meta.Dimensions, Stat: stat}
				if ms.Dimensions == nil {
					ms.Dimensions = map[string]string{}
				}
				pts, err := ev.st.FetchStat(ev.ctx, ev.tenant, ms, from, to, int(ev.res/1000))
				var pe *query.PartialError
				if errors.As(err, &pe) {
					pts, err = pe.Points, nil
					ev.warn("partial data: " + pe.Msg)
				}
				if err != nil {
					errs[k] = err
					return
				}
				clean := pts[:0:0]
				for _, p := range pts {
					if !math.IsNaN(p.V) && p.T > from-ev.res && p.T < to {
						clean = append(clean, p)
					}
				}
				sort.Slice(clean, func(a, b int) bool { return clean[a].T < clean[b].T })
				l := make(map[string]string, len(e.labels)+1)
				for k, v := range e.labels {
					l[k] = v
				}
				l[statLabel] = stat
				out[k] = rawSeries{labels: l, pts: clean}
			}(k, e, stat)
		}
	}
	wg.Wait()
	var n int64
	for k, err := range errs {
		if err != nil {
			return nil, err
		}
		n += int64(len(out[k].pts))
	}
	ev.mu.Lock()
	ev.samples += n
	over := ev.samples > ev.maxSample
	ev.mu.Unlock()
	if over {
		return nil, execErr("query processing would load too many samples into memory in query execution")
	}
	return out, nil
}

// instant picks, at each step, the newest sample within the lookback delta.
func (ev *evaluator) instant(raw []rawSeries, off int64) vector {
	out := make(vector, 0, len(raw))
	for _, r := range raw {
		s := newSeries(r.labels, ev.n())
		has := false
		for i, t := range ev.ts {
			tt := t - off
			j := sort.Search(len(r.pts), func(k int) bool { return r.pts[k].T > tt }) - 1
			if j >= 0 && r.pts[j].T > tt-ev.lookback {
				s.vals[i], s.ok[i], has = r.pts[j].V, true, true
			}
		}
		if has {
			out = append(out, s)
		}
	}
	return out
}

func newSeries(l map[string]string, n int) *vseries {
	return &vseries{labels: l, vals: make([]float64, n), ok: make([]bool, n)}
}

func dropName(l map[string]string) map[string]string {
	out := make(map[string]string, len(l))
	for k, v := range l {
		if k != nameLabel {
			out[k] = v
		}
	}
	return out
}

func mapVec(v vector, f func(float64) float64) vector {
	out := make(vector, len(v))
	for i, s := range v {
		ns := newSeries(dropName(s.labels), len(s.vals))
		for j := range s.vals {
			if s.ok[j] {
				ns.vals[j], ns.ok[j] = f(s.vals[j]), true
			}
		}
		out[i] = ns
	}
	return out
}

// window returns the samples in (t-off-rng, t-off].
func window(pts []model.Point, t, rng, off int64) []model.Point {
	end := t - off
	lo := sort.Search(len(pts), func(k int) bool { return pts[k].T > end-rng })
	hi := sort.Search(len(pts), func(k int) bool { return pts[k].T > end })
	return pts[lo:hi]
}

func (ev *evaluator) rangeFn(m *matrix, keepName bool, f func(w []model.Point, t int64) (float64, bool)) vector {
	out := make(vector, 0, len(m.series))
	for _, r := range m.series {
		l := r.labels
		if !keepName {
			l = dropName(l)
		}
		s := newSeries(l, ev.n())
		has := false
		for i, t := range ev.ts {
			w := window(r.pts, t, m.rng, m.off)
			if v, ok := f(w, t-m.off); ok {
				s.vals[i], s.ok[i], has = v, true, true
			}
		}
		if has {
			out = append(out, s)
		}
	}
	return out
}

// extrapolatedRate follows Prometheus' rate/increase/delta.
func extrapolatedRate(w []model.Point, end, rng int64, isCounter, isRate bool) (float64, bool) {
	if len(w) < 2 {
		return 0, false
	}
	first, last := w[0], w[len(w)-1]
	result := last.V - first.V
	if isCounter {
		for i := 1; i < len(w); i++ {
			if w[i].V < w[i-1].V {
				result += w[i-1].V
			}
		}
	}
	rangeStart := end - rng
	durToStart := float64(first.T-rangeStart) / 1000
	durToEnd := float64(end-last.T) / 1000
	sampled := float64(last.T-first.T) / 1000
	avgDur := sampled / float64(len(w)-1)
	threshold := avgDur * 1.1
	if durToStart >= threshold {
		durToStart = avgDur / 2
	}
	if isCounter && result > 0 && first.V >= 0 {
		if durToZero := sampled * (first.V / result); durToZero < durToStart {
			durToStart = durToZero
		}
	}
	if durToEnd >= threshold {
		durToEnd = avgDur / 2
	}
	factor := (sampled + durToStart + durToEnd) / sampled
	if isRate {
		factor /= float64(rng) / 1000
	}
	return result * factor, true
}

func quantile(q float64, vals []float64) float64 {
	if len(vals) == 0 || math.IsNaN(q) {
		return math.NaN()
	}
	if q < 0 {
		return math.Inf(-1)
	}
	if q > 1 {
		return math.Inf(1)
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	rank := q * float64(len(s)-1)
	lo := math.Max(0, math.Floor(rank))
	hi := math.Min(float64(len(s)-1), lo+1)
	weight := rank - math.Floor(rank)
	return s[int(lo)]*(1-weight) + s[int(hi)]*weight
}

func meanVar(vals []float64) (mean, variance float64) {
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	for _, v := range vals {
		variance += (v - mean) * (v - mean)
	}
	return mean, variance / float64(len(vals))
}

func ptVals(w []model.Point) []float64 {
	out := make([]float64, len(w))
	for i, p := range w {
		out[i] = p.V
	}
	return out
}

func (ev *evaluator) call(c *Call) (any, error) {
	args := make([]any, len(c.Args))
	for i, a := range c.Args {
		v, err := ev.eval(a)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	m, _ := firstMatrix(args)
	switch c.Func {
	case "rate", "increase", "delta":
		counter, isRate := c.Func != "delta", c.Func == "rate"
		return ev.rangeFn(m, false, func(w []model.Point, end int64) (float64, bool) {
			return extrapolatedRate(w, end, m.rng, counter, isRate)
		}), nil
	case "irate":
		return ev.rangeFn(m, false, func(w []model.Point, _ int64) (float64, bool) {
			if len(w) < 2 {
				return 0, false
			}
			a, b := w[len(w)-2], w[len(w)-1]
			d := b.V - a.V
			if b.V < a.V {
				d = b.V
			}
			return d / (float64(b.T-a.T) / 1000), true
		}), nil
	case "avg_over_time", "sum_over_time", "min_over_time", "max_over_time", "count_over_time",
		"last_over_time", "stddev_over_time", "stdvar_over_time", "present_over_time":
		fn := c.Func
		return ev.rangeFn(m, fn == "last_over_time", func(w []model.Point, _ int64) (float64, bool) {
			if len(w) == 0 {
				return 0, false
			}
			vals := ptVals(w)
			switch fn {
			case "sum_over_time":
				s := 0.0
				for _, v := range vals {
					s += v
				}
				return s, true
			case "min_over_time":
				r := vals[0]
				for _, v := range vals {
					if v < r || math.IsNaN(r) {
						r = v
					}
				}
				return r, true
			case "max_over_time":
				r := vals[0]
				for _, v := range vals {
					if v > r || math.IsNaN(r) {
						r = v
					}
				}
				return r, true
			case "count_over_time":
				return float64(len(vals)), true
			case "last_over_time":
				return vals[len(vals)-1], true
			case "present_over_time":
				return 1, true
			case "stddev_over_time":
				_, v := meanVar(vals)
				return math.Sqrt(v), true
			case "stdvar_over_time":
				_, v := meanVar(vals)
				return v, true
			}
			mean, _ := meanVar(vals)
			return mean, true
		}), nil
	case "quantile_over_time":
		q := args[0].(scalar)
		idx := map[int64]int{}
		for i, t := range ev.ts {
			idx[t-m.off] = i
		}
		return ev.rangeFn(m, false, func(w []model.Point, end int64) (float64, bool) {
			if len(w) == 0 {
				return 0, false
			}
			return quantile(q[idx[end]], ptVals(w)), true
		}), nil
	case "abs", "ceil", "floor", "sqrt", "exp", "ln", "log2", "log10", "sgn":
		f := map[string]func(float64) float64{
			"abs": math.Abs, "ceil": math.Ceil, "floor": math.Floor, "sqrt": math.Sqrt, "exp": math.Exp,
			"ln": math.Log, "log2": math.Log2, "log10": math.Log10,
			"sgn": func(v float64) float64 {
				switch {
				case v > 0:
					return 1
				case v < 0:
					return -1
				}
				return v
			},
		}[c.Func]
		return mapVec(args[0].(vector), f), nil
	case "round", "clamp", "clamp_min", "clamp_max":
		v := args[0].(vector)
		sc := func(i int) scalar {
			if i < len(args) {
				return args[i].(scalar)
			}
			return nil
		}
		a1, a2 := sc(1), sc(2)
		out := make(vector, 0, len(v))
		for _, s := range v {
			ns := newSeries(dropName(s.labels), ev.n())
			for j := range s.vals {
				if !s.ok[j] {
					continue
				}
				x := s.vals[j]
				switch c.Func {
				case "round":
					to := 1.0
					if a1 != nil {
						to = a1[j]
					}
					inv := 1 / to
					x = math.Floor(x*inv+0.5) / inv
				case "clamp":
					if a2[j] < a1[j] {
						continue // Prometheus returns an empty vector when max < min
					}
					x = math.Max(a1[j], math.Min(a2[j], x))
				case "clamp_min":
					x = math.Max(a1[j], x)
				case "clamp_max":
					x = math.Min(a1[j], x)
				}
				ns.vals[j], ns.ok[j] = x, true
			}
			out = append(out, ns)
		}
		return out, nil
	case "scalar":
		v := args[0].(vector)
		out := make(scalar, ev.n())
		for j := range out {
			out[j] = math.NaN()
			n := 0
			for _, s := range v {
				if s.ok[j] {
					n++
					out[j] = s.vals[j]
				}
			}
			if n != 1 {
				out[j] = math.NaN()
			}
		}
		return out, nil
	case "vector":
		s := newSeries(map[string]string{}, ev.n())
		copy(s.vals, args[0].(scalar))
		for j := range s.ok {
			s.ok[j] = true
		}
		return vector{s}, nil
	case "time":
		out := make(scalar, ev.n())
		for i, t := range ev.ts {
			out[i] = float64(t) / 1000
		}
		return out, nil
	case "absent":
		v := args[0].(vector)
		l := map[string]string{}
		if vs, ok := unparen(c.Args[0]).(*VectorSel); ok {
			for _, m := range vs.Matchers {
				if m.Type == MatchEqual && m.Name != nameLabel {
					l[m.Name] = m.Value
				}
			}
		}
		s := newSeries(l, ev.n())
		has := false
		for j := range s.ok {
			present := false
			for _, x := range v {
				present = present || x.ok[j]
			}
			if !present {
				s.vals[j], s.ok[j], has = 1, true, true
			}
		}
		if !has {
			return vector{}, nil
		}
		return vector{s}, nil
	case "sort", "sort_desc":
		return args[0], nil // ordering is applied to instant results by the HTTP layer
	}
	return nil, badData("unsupported: function %s is not supported", c.Func)
}

func firstMatrix(args []any) (*matrix, bool) {
	for _, a := range args {
		if m, ok := a.(*matrix); ok {
			return m, true
		}
	}
	return nil, false
}

func unparen(e Expr) Expr {
	for {
		p, ok := e.(*Paren)
		if !ok {
			return e
		}
		e = p.Expr
	}
}

// ---- aggregation ----

func groupLabels(l map[string]string, grouping []string, without bool) map[string]string {
	out := map[string]string{}
	if without {
		drop := map[string]bool{nameLabel: true}
		for _, g := range grouping {
			drop[g] = true
		}
		for k, v := range l {
			if !drop[k] {
				out[k] = v
			}
		}
		return out
	}
	for _, g := range grouping {
		if v, ok := l[g]; ok && v != "" {
			out[g] = v
		}
	}
	return out
}

func (ev *evaluator) aggregate(a *Agg) (any, error) {
	v0, err := ev.eval(a.Expr)
	if err != nil {
		return nil, err
	}
	in := v0.(vector)
	var param scalar
	if a.Param != nil {
		p, err := ev.eval(a.Param)
		if err != nil {
			return nil, err
		}
		param = p.(scalar)
	}
	type group struct {
		labels  map[string]string
		members []*vseries
	}
	var order []string
	groups := map[string]*group{}
	for _, s := range in {
		gl := groupLabels(s.labels, a.Grouping, a.Without)
		k := labelKey(gl)
		g := groups[k]
		if g == nil {
			g = &group{labels: gl}
			groups[k] = g
			order = append(order, k)
		}
		g.members = append(g.members, s)
	}
	n := ev.n()
	if a.Op == "topk" || a.Op == "bottomk" {
		selected := map[*vseries]*vseries{}
		var outOrder []*vseries
		for _, k := range order {
			g := groups[k]
			for j := 0; j < n; j++ {
				kk := param[j]
				if math.IsNaN(kk) || kk < 1 {
					continue
				}
				var present []*vseries
				for _, s := range g.members {
					if s.ok[j] {
						present = append(present, s)
					}
				}
				sort.SliceStable(present, func(x, y int) bool {
					vx, vy := present[x].vals[j], present[y].vals[j]
					if math.IsNaN(vx) {
						return false
					}
					if math.IsNaN(vy) {
						return true
					}
					if a.Op == "topk" {
						return vx > vy
					}
					return vx < vy
				})
				if int(kk) < len(present) {
					present = present[:int(kk)]
				}
				for _, s := range present {
					o := selected[s]
					if o == nil {
						o = newSeries(s.labels, n)
						selected[s] = o
						outOrder = append(outOrder, o)
					}
					o.vals[j], o.ok[j] = s.vals[j], true
				}
			}
		}
		return vector(outOrder), nil
	}
	out := make(vector, 0, len(order))
	for _, k := range order {
		g := groups[k]
		o := newSeries(g.labels, n)
		has := false
		for j := 0; j < n; j++ {
			var vals []float64
			for _, s := range g.members {
				if s.ok[j] {
					vals = append(vals, s.vals[j])
				}
			}
			if len(vals) == 0 {
				continue
			}
			var r float64
			switch a.Op {
			case "sum":
				for _, v := range vals {
					r += v
				}
			case "avg":
				r, _ = meanVar(vals)
			case "count":
				r = float64(len(vals))
			case "group":
				r = 1
			case "min", "max":
				r = vals[0]
				for _, v := range vals[1:] {
					if math.IsNaN(r) || (a.Op == "min" && v < r) || (a.Op == "max" && v > r) {
						r = v
					}
				}
			case "stddev":
				_, va := meanVar(vals)
				r = math.Sqrt(va)
			case "stdvar":
				_, r = meanVar(vals)
			case "quantile":
				r = quantile(param[j], vals)
			}
			o.vals[j], o.ok[j], has = r, true, true
		}
		if has {
			out = append(out, o)
		}
	}
	return out, nil
}

// ---- binary operators ----

func arith(op string, l, r float64) (float64, bool) {
	switch op {
	case "+":
		return l + r, true
	case "-":
		return l - r, true
	case "*":
		return l * r, true
	case "/":
		return l / r, true
	case "%":
		return math.Mod(l, r), true
	case "^":
		return math.Pow(l, r), true
	case "atan2":
		return math.Atan2(l, r), true
	case "==":
		return l, l == r
	case "!=":
		return l, l != r
	case ">":
		return l, l > r
	case "<":
		return l, l < r
	case ">=":
		return l, l >= r
	case "<=":
		return l, l <= r
	}
	return 0, false
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (ev *evaluator) binary(b *Binary) (any, error) {
	lv, err := ev.eval(b.LHS)
	if err != nil {
		return nil, err
	}
	rv, err := ev.eval(b.RHS)
	if err != nil {
		return nil, err
	}
	cmp := isComparison(b.Op)
	switch l := lv.(type) {
	case scalar:
		switch r := rv.(type) {
		case scalar:
			out := make(scalar, len(l))
			for i := range l {
				v, keep := arith(b.Op, l[i], r[i])
				if cmp {
					v = b2f(keep)
				}
				out[i] = v
			}
			return out, nil
		case vector:
			return ev.vecScalar(b, r, l, true), nil
		}
	case vector:
		switch r := rv.(type) {
		case scalar:
			return ev.vecScalar(b, l, r, false), nil
		case vector:
			if isSetOp(b.Op) {
				return ev.setOp(b, l, r), nil
			}
			return ev.vecVec(b, l, r)
		}
	}
	return nil, badData("binary expression must contain only scalar and instant vector types")
}

func (ev *evaluator) vecScalar(b *Binary, v vector, s scalar, swap bool) vector {
	cmp := isComparison(b.Op)
	out := make(vector, 0, len(v))
	for _, x := range v {
		l := x.labels
		if !cmp || b.Bool {
			l = dropName(l)
		}
		ns := newSeries(l, ev.n())
		has := false
		for j := range x.vals {
			if !x.ok[j] {
				continue
			}
			lf, rf := x.vals[j], s[j]
			if swap {
				lf, rf = rf, lf
			}
			val, keep := arith(b.Op, lf, rf)
			if cmp {
				val = x.vals[j] // a comparison keeps the vector's value
				if b.Bool {
					val, keep = b2f(keep), true
				}
				if !keep {
					continue
				}
			}
			ns.vals[j], ns.ok[j], has = val, true, true
		}
		if has {
			out = append(out, ns)
		}
	}
	return out
}

// signature is the matching key of a series for on/ignoring.
func signature(l map[string]string, b *Binary) string {
	m := map[string]string{}
	if b.On {
		for _, k := range b.Matching {
			if v, ok := l[k]; ok {
				m[k] = v
			}
		}
	} else {
		ign := map[string]bool{nameLabel: true}
		for _, k := range b.Matching {
			ign[k] = true
		}
		for k, v := range l {
			if !ign[k] {
				m[k] = v
			}
		}
	}
	return labelKey(m)
}

func resultLabels(l map[string]string, b *Binary) map[string]string {
	out := map[string]string{}
	drop := !isComparison(b.Op) || b.Bool
	if b.On {
		for _, k := range b.Matching {
			if v, ok := l[k]; ok && k != nameLabel {
				out[k] = v
			}
		}
		return out
	}
	ign := map[string]bool{}
	for _, k := range b.Matching {
		ign[k] = true
	}
	for k, v := range l {
		if ign[k] || (drop && k == nameLabel) {
			continue
		}
		out[k] = v
	}
	return out
}

func (ev *evaluator) vecVec(b *Binary, lhs, rhs vector) (vector, error) {
	cmp := isComparison(b.Op)
	rsig := make([]string, len(rhs))
	bySig := map[string][]int{}
	for i, s := range rhs {
		rsig[i] = signature(s.labels, b)
		bySig[rsig[i]] = append(bySig[rsig[i]], i)
	}
	lsig := make([]string, len(lhs))
	for i, s := range lhs {
		lsig[i] = signature(s.labels, b)
	}
	outBy := map[string]*vseries{}
	var out vector
	n := ev.n()
	for j := 0; j < n; j++ {
		matchedR := map[string]int{}
		for sig, idx := range bySig {
			found := -1
			for _, i := range idx {
				if rhs[i].ok[j] {
					if found >= 0 {
						return nil, execErr("found duplicate series for the match group %s on the right hand-side of the operation: [%s, %s];many-to-many matching not allowed: matching labels must be unique on one side",
							labelString(rhs[i].labels), labelString(rhs[found].labels), labelString(rhs[i].labels))
					}
					found = i
				}
			}
			if found >= 0 {
				matchedR[sig] = found
			}
		}
		usedSig := map[string]int{}
		for li, ls := range lhs {
			if !ls.ok[j] {
				continue
			}
			ri, ok := matchedR[lsig[li]]
			if !ok {
				continue
			}
			if prev, dup := usedSig[lsig[li]]; dup {
				return nil, execErr("multiple matches for labels: many-to-one matching must be explicit (group_left/group_right): %s and %s",
					labelString(lhs[prev].labels), labelString(ls.labels))
			}
			usedSig[lsig[li]] = li
			val, keep := arith(b.Op, ls.vals[j], rhs[ri].vals[j])
			if cmp {
				if b.Bool {
					val, keep = b2f(keep), true
				}
				if !keep {
					continue
				}
			}
			rl := resultLabels(ls.labels, b)
			k := labelKey(rl)
			o := outBy[k]
			if o == nil {
				o = newSeries(rl, n)
				outBy[k] = o
				out = append(out, o)
			}
			o.vals[j], o.ok[j] = val, true
		}
	}
	return out, nil
}

func (ev *evaluator) setOp(b *Binary, lhs, rhs vector) vector {
	n := ev.n()
	present := func(v vector, j int) map[string]bool {
		m := map[string]bool{}
		for _, s := range v {
			if s.ok[j] {
				m[signature(s.labels, b)] = true
			}
		}
		return m
	}
	lsig := make([]string, len(lhs))
	for i, s := range lhs {
		lsig[i] = signature(s.labels, b)
	}
	var out vector
	keepL := make([]*vseries, len(lhs))
	for j := 0; j < n; j++ {
		rp := present(rhs, j)
		for i, s := range lhs {
			if !s.ok[j] {
				continue
			}
			keep := true
			switch b.Op {
			case "and":
				keep = rp[lsig[i]]
			case "unless":
				keep = !rp[lsig[i]]
			}
			if !keep {
				continue
			}
			if keepL[i] == nil {
				keepL[i] = newSeries(s.labels, n)
			}
			keepL[i].vals[j], keepL[i].ok[j] = s.vals[j], true
		}
	}
	for _, s := range keepL {
		if s != nil {
			out = append(out, s)
		}
	}
	if b.Op != "or" {
		return out
	}
	for _, s := range rhs {
		var o *vseries
		sig := signature(s.labels, b)
		for j := 0; j < n; j++ {
			if !s.ok[j] {
				continue
			}
			hit := false
			for i, l := range lhs {
				if l.ok[j] && lsig[i] == sig {
					hit = true
					break
				}
			}
			if hit {
				continue
			}
			if o == nil {
				o = newSeries(s.labels, n)
				out = append(out, o)
			}
			o.vals[j], o.ok[j] = s.vals[j], true
		}
	}
	return out
}

// finalize merges series with the same label set, failing when two of them
// have a sample at the same step (Prometheus' duplicate labelset error).
func finalize(v vector) (vector, error) {
	by := map[string]*vseries{}
	var out vector
	for _, s := range v {
		k := labelKey(s.labels)
		o := by[k]
		if o == nil {
			by[k] = s
			out = append(out, s)
			continue
		}
		for j := range s.ok {
			if !s.ok[j] {
				continue
			}
			if o.ok[j] {
				return nil, execErr("vector cannot contain metrics with the same labelset %s", labelString(s.labels))
			}
			o.vals[j], o.ok[j] = s.vals[j], true
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return labelKey(out[i].labels) < labelKey(out[j].labels) })
	return out, nil
}

// minWindow is the shortest selector window in e (range or lookback).
func minWindow(e Expr, lookback time.Duration) time.Duration {
	best := time.Duration(math.MaxInt64)
	var walk func(Expr)
	walk = func(e Expr) {
		switch x := e.(type) {
		case *VectorSel:
			best = min(best, lookback)
		case *MatrixSel:
			best = min(best, x.Range)
		case *Paren:
			walk(x.Expr)
		case *Unary:
			walk(x.Expr)
		case *Call:
			for _, a := range x.Args {
				walk(a)
			}
		case *Agg:
			walk(x.Expr)
			if x.Param != nil {
				walk(x.Param)
			}
		case *Binary:
			walk(x.LHS)
			walk(x.RHS)
		}
	}
	walk(e)
	return best
}
