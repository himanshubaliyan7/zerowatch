// Package promapi serves a Prometheus-compatible HTTP API (a PromQL subset)
// over ZeroWatch storage, so Grafana's built-in Prometheus datasource can
// query ZeroWatch. Mounted under a prefix such as /prometheus, the
// datasource URL is http://host:8080/prometheus.
//
// Series map to Prometheus label sets as described in names.go. Each sample
// is the series' statistic over one fetch bucket (60 s by default, bucket
// start as the timestamp); instant selectors take the newest sample within
// the 5 m lookback delta, range selectors take the samples in (t-range, t].
// Range queries align start down to a multiple of step and allow at most
// 11,000 points per series, like Prometheus.
package promapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

// Store is the part of storage.Engine the API needs.
type Store interface {
	query.Fetcher
	ListSeries(ctx context.Context, tenant uint32, f storage.SeriesFilter) ([]model.SeriesMeta, error)
}

// Handler serves the Prometheus API.
type Handler struct {
	Store Store
	// Tenant resolves the request's tenant; default httpx.Tenant (set by the
	// httpx.Auth middleware).
	Tenant func(*http.Request) uint32
	Log    *slog.Logger
	// Defaults: 5 m lookback, 60 s resolution, 10,000 series per selector,
	// 50 M samples per query, 2 m timeout, 10 s series-list cache.
	LookbackDelta time.Duration
	Resolution    time.Duration
	MaxSeries     int
	MaxSamples    int64
	Timeout       time.Duration
	CatalogTTL    time.Duration
	Now           func() time.Time

	cache catalogCache
}

const maxPointsPerSeries = 11000

// Register mounts the routes under prefix (e.g. "/prometheus").
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	routes := map[string]http.HandlerFunc{
		"/api/v1/query":               h.query,
		"/api/v1/query_range":         h.queryRange,
		"/api/v1/series":              h.series,
		"/api/v1/labels":              h.labels,
		"/api/v1/label/{name}/values": h.labelValues,
		"/api/v1/metadata":            h.metadata,
		"/api/v1/status/buildinfo":    h.buildinfo,
		"/api/v1/query_exemplars":     h.empty([]any{}),
		"/api/v1/rules":               h.empty(map[string]any{"groups": []any{}}),
		"/api/v1/alerts":              h.empty(map[string]any{"alerts": []any{}}),
	}
	for p, fn := range routes {
		mux.HandleFunc("GET "+prefix+p, fn)
		mux.HandleFunc("POST "+prefix+p, fn)
	}
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) tenant(r *http.Request) uint32 {
	if h.Tenant != nil {
		return h.Tenant(r)
	}
	return httpx.Tenant(r.Context())
}

func orDur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// ---- response encoding ----

type response struct {
	Status    string   `json:"status"`
	Data      any      `json:"data,omitempty"`
	ErrorType string   `json:"errorType,omitempty"`
	Error     string   `json:"error,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// sample marshals as [unixSeconds, "value"].
type sample struct {
	T int64 // ms
	V float64
}

func (s sample) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 40)
	b = append(b, '[')
	b = strconv.AppendFloat(b, float64(s.T)/1000, 'f', -1, 64)
	b = append(b, ',', '"')
	b = append(b, formatValue(s.V)...)
	return append(b, '"', ']'), nil
}

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

type vecSample struct {
	Metric map[string]string `json:"metric"`
	Value  sample            `json:"value"`
}

type matSeries struct {
	Metric map[string]string `json:"metric"`
	Values []sample          `json:"values"`
}

type queryData struct {
	ResultType string `json:"resultType"`
	Result     any    `json:"result"`
}

func writeJSON(w http.ResponseWriter, status int, v response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) ok(w http.ResponseWriter, data any, warnings []string) {
	writeJSON(w, http.StatusOK, response{Status: "success", Data: data, Warnings: warnings})
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var pe *promError
	switch {
	case errors.As(err, &pe):
	case errors.Is(err, context.DeadlineExceeded):
		pe = &promError{typ: "timeout", msg: "query timed out in query execution"}
	case errors.Is(err, context.Canceled):
		pe = &promError{typ: "canceled", msg: "query was canceled in query execution"}
	case errors.Is(err, storage.ErrBadStat), errors.Is(err, query.ErrBadRequest):
		pe = &promError{typ: "bad_data", msg: err.Error()}
	default:
		if h.Log != nil {
			h.Log.Error("prometheus api", "err", err, "path", r.URL.Path)
		}
		pe = &promError{typ: "internal", msg: "an internal error occurred"}
	}
	status := map[string]int{
		"bad_data": http.StatusBadRequest, "execution": http.StatusUnprocessableEntity,
		"timeout": http.StatusServiceUnavailable, "canceled": http.StatusServiceUnavailable,
	}[pe.typ]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, response{Status: "error", ErrorType: pe.typ, Error: pe.msg})
}

// ---- parameter parsing ----

func (h *Handler) form(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		return badData("invalid form: %v", err)
	}
	return nil
}

// parseTime accepts unix seconds (fractional) or RFC 3339.
func parseTime(s string) (time.Time, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(math.Round(frac*1000))*int64(time.Millisecond)).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Time{}, badData("cannot parse %q to a valid timestamp", s)
}

// parseStep accepts seconds (fractional) or a Prometheus duration.
func parseStep(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) || f > float64(math.MaxInt64)/1e9 {
			return 0, badData("cannot parse %q to a valid duration", s)
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	if d, err := parseDuration(s); err == nil {
		return d, nil
	}
	return 0, badData("cannot parse %q to a valid duration", s)
}

func (h *Handler) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	to := orDur(h.Timeout, 2*time.Minute)
	if s := r.Form.Get("timeout"); s != "" {
		if d, err := parseStep(s); err == nil && d > 0 && d < to {
			to = d
		}
	}
	return context.WithTimeout(r.Context(), to)
}

// ---- query endpoints ----

func (h *Handler) query(w http.ResponseWriter, r *http.Request) {
	if err := h.form(w, r); err != nil {
		h.fail(w, r, err)
		return
	}
	t := h.now()
	if s := r.Form.Get("time"); s != "" {
		var err error
		if t, err = parseTime(s); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	data, warn, err := h.Instant(ctx, h.tenant(r), r.Form.Get("query"), t)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, data, warn)
}

func (h *Handler) queryRange(w http.ResponseWriter, r *http.Request) {
	if err := h.form(w, r); err != nil {
		h.fail(w, r, err)
		return
	}
	start, err := parseTime(r.Form.Get("start"))
	if err != nil {
		h.fail(w, r, badData("invalid parameter \"start\": %v", err))
		return
	}
	end, err := parseTime(r.Form.Get("end"))
	if err != nil {
		h.fail(w, r, badData("invalid parameter \"end\": %v", err))
		return
	}
	step, err := parseStep(r.Form.Get("step"))
	if err != nil {
		h.fail(w, r, badData("invalid parameter \"step\": %v", err))
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	data, warn, err := h.Range(ctx, h.tenant(r), r.Form.Get("query"), start, end, step)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, data, warn)
}

func (h *Handler) newEval(ctx context.Context, tenant uint32, ts []int64, step time.Duration, e Expr) (*evaluator, error) {
	cat, err := h.cache.get(ctx, h.Store, tenant, h.now(), orDur(h.CatalogTTL, 10*time.Second))
	if err != nil {
		return nil, err
	}
	lookback := orDur(h.LookbackDelta, 5*time.Minute)
	res := orDur(h.Resolution, time.Minute)
	if res%time.Minute != 0 {
		res = time.Minute
	}
	// Long spans: coarsen the fetch resolution towards the step, but keep at
	// least two buckets inside the shortest window.
	span := time.Duration(ts[len(ts)-1]-ts[0]) * time.Millisecond
	if span/res > 20000 && step > res {
		c := min(step, minWindow(e, lookback)/2).Truncate(time.Minute)
		if c > res {
			res = c
		}
	}
	maxSeries := h.MaxSeries
	if maxSeries <= 0 {
		maxSeries = 10000
	}
	maxSamples := h.MaxSamples
	if maxSamples <= 0 {
		maxSamples = 50_000_000
	}
	ev := &evaluator{ctx: ctx, st: h.Store, tenant: tenant, cat: cat, ts: ts, lookback: lookback.Milliseconds(),
		res: res.Milliseconds(), maxSeries: maxSeries, maxSample: maxSamples}
	if cat.partial != "" {
		ev.warn("partial series list: " + cat.partial)
	}
	return ev, nil
}

func parseQuery(q string) (Expr, error) {
	if strings.TrimSpace(q) == "" {
		return nil, badData("invalid parameter \"query\": empty query")
	}
	e, err := ParseExpr(q)
	if err != nil {
		return nil, badData("invalid parameter \"query\": %v", err)
	}
	return e, nil
}

// Instant evaluates q at t and returns the Prometheus "data" object.
func (h *Handler) Instant(ctx context.Context, tenant uint32, q string, t time.Time) (any, []string, error) {
	e, err := parseQuery(q)
	if err != nil {
		return nil, nil, err
	}
	ms := t.UnixMilli()
	ev, err := h.newEval(ctx, tenant, []int64{ms}, 0, e)
	if err != nil {
		return nil, nil, err
	}
	v, err := ev.eval(e)
	if err != nil {
		return nil, nil, err
	}
	switch x := v.(type) {
	case scalar:
		return queryData{ResultType: "scalar", Result: sample{T: ms, V: x[0]}}, ev.warnings, nil
	case strVal:
		return queryData{ResultType: "string", Result: []any{float64(ms) / 1000, string(x)}}, ev.warnings, nil
	case *matrix:
		out := []matSeries{}
		for _, s := range x.series {
			w := window(s.pts, ms, x.rng, x.off)
			if len(w) == 0 {
				continue
			}
			vals := make([]sample, len(w))
			for i, p := range w {
				vals[i] = sample{T: p.T + x.off, V: p.V}
			}
			out = append(out, matSeries{Metric: s.labels, Values: vals})
		}
		sort.SliceStable(out, func(i, j int) bool { return labelKey(out[i].Metric) < labelKey(out[j].Metric) })
		return queryData{ResultType: "matrix", Result: out}, ev.warnings, nil
	case vector:
		fv, err := finalize(x)
		if err != nil {
			return nil, nil, err
		}
		out := []vecSample{}
		for _, s := range fv {
			if s.ok[0] {
				out = append(out, vecSample{Metric: s.labels, Value: sample{T: ms, V: s.vals[0]}})
			}
		}
		if c, ok := unparen(e).(*Call); ok && (c.Func == "sort" || c.Func == "sort_desc") {
			desc := c.Func == "sort_desc"
			sort.SliceStable(out, func(i, j int) bool {
				if desc {
					return out[i].Value.V > out[j].Value.V
				}
				return out[i].Value.V < out[j].Value.V
			})
		}
		return queryData{ResultType: "vector", Result: out}, ev.warnings, nil
	}
	return nil, nil, badData("unexpected result type")
}

// Range evaluates q over [start, end] every step and returns a matrix.
func (h *Handler) Range(ctx context.Context, tenant uint32, q string, start, end time.Time, step time.Duration) (any, []string, error) {
	if end.Before(start) {
		return nil, nil, badData("invalid parameter \"end\": end timestamp must not be before start time")
	}
	if step <= 0 {
		return nil, nil, badData("invalid parameter \"step\": zero or negative query resolution step widths are not accepted. Try a positive integer")
	}
	if end.Sub(start)/step > maxPointsPerSeries {
		return nil, nil, badData("exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)")
	}
	e, err := parseQuery(q)
	if err != nil {
		return nil, nil, err
	}
	if t := typeOf(e); t != typScalar && t != typVector {
		return nil, nil, badData("invalid expression type %q for range query, must be Scalar or instant Vector", t)
	}
	stepMs := step.Milliseconds()
	if stepMs <= 0 {
		stepMs = 1
	}
	s := start.UnixMilli()
	s -= ((s % stepMs) + stepMs) % stepMs // align start down to a step multiple
	var ts []int64
	for t := s; t <= end.UnixMilli(); t += stepMs {
		ts = append(ts, t)
	}
	if len(ts) > maxPointsPerSeries {
		return nil, nil, badData("exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)")
	}
	ev, err := h.newEval(ctx, tenant, ts, step, e)
	if err != nil {
		return nil, nil, err
	}
	v, err := ev.eval(e)
	if err != nil {
		return nil, nil, err
	}
	var vec vector
	switch x := v.(type) {
	case scalar:
		ss := newSeries(map[string]string{}, len(ts))
		copy(ss.vals, x)
		for i := range ss.ok {
			ss.ok[i] = true
		}
		vec = vector{ss}
	case vector:
		if vec, err = finalize(x); err != nil {
			return nil, nil, err
		}
	}
	out := []matSeries{}
	for _, sr := range vec {
		var vals []sample
		for i, t := range ts {
			if sr.ok[i] {
				vals = append(vals, sample{T: t, V: sr.vals[i]})
			}
		}
		if len(vals) > 0 {
			out = append(out, matSeries{Metric: sr.labels, Values: vals})
		}
	}
	return queryData{ResultType: "matrix", Result: out}, ev.warnings, nil
}

// ---- metadata endpoints ----

// matchSets returns the catalog entries matching any match[] selector, or
// all entries when there is none.
func (h *Handler) matchSets(ctx context.Context, r *http.Request, required bool) ([]entry, []string, error) {
	cat, err := h.cache.get(ctx, h.Store, h.tenant(r), h.now(), orDur(h.CatalogTTL, 10*time.Second))
	if err != nil {
		return nil, nil, err
	}
	var warn []string
	if cat.partial != "" {
		warn = []string{"partial series list: " + cat.partial}
	}
	sels := r.Form["match[]"]
	if len(sels) == 0 {
		if required {
			return nil, nil, badData("no match[] parameter provided")
		}
		return cat.all, warn, nil
	}
	seen := map[string]bool{}
	var out []entry
	for _, s := range sels {
		e, err := ParseExpr(s)
		if err != nil {
			return nil, nil, badData("invalid parameter \"match[]\": %v", err)
		}
		vs, ok := e.(*VectorSel)
		if !ok {
			return nil, nil, badData("invalid parameter \"match[]\": %q is not a series selector", s)
		}
		for _, en := range cat.match(vs.Matchers) {
			if k := labelKey(en.labels); !seen[k] {
				seen[k] = true
				out = append(out, en)
			}
		}
	}
	return out, warn, nil
}

func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.Form.Get("limit"))
	return n
}

func (h *Handler) series(w http.ResponseWriter, r *http.Request) {
	if err := h.form(w, r); err != nil {
		h.fail(w, r, err)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	ents, warn, err := h.matchSets(ctx, r, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]map[string]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.labels)
	}
	sort.SliceStable(out, func(i, j int) bool { return labelKey(out[i]) < labelKey(out[j]) })
	if n := limitParam(r); n > 0 && len(out) > n {
		out = out[:n]
	}
	h.ok(w, out, warn)
}

func (h *Handler) labels(w http.ResponseWriter, r *http.Request) {
	if err := h.form(w, r); err != nil {
		h.fail(w, r, err)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	ents, warn, err := h.matchSets(ctx, r, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	set := map[string]bool{statLabel: true}
	for _, e := range ents {
		for k := range e.labels {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if n := limitParam(r); n > 0 && len(out) > n {
		out = out[:n]
	}
	h.ok(w, out, warn)
}

func (h *Handler) labelValues(w http.ResponseWriter, r *http.Request) {
	if err := h.form(w, r); err != nil {
		h.fail(w, r, err)
		return
	}
	name := r.PathValue("name")
	if strings.HasPrefix(name, "U__") { // Prometheus' escaped label names
		name = name[3:]
	}
	if name == "" {
		h.fail(w, r, badData("invalid label name: %q", r.PathValue("name")))
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	ents, warn, err := h.matchSets(ctx, r, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := []string{}
	if name == statLabel {
		out = append(out, baseStats...)
	} else {
		set := map[string]bool{}
		for _, e := range ents {
			if v, ok := e.labels[name]; ok && v != "" {
				set[v] = true
			}
		}
		for v := range set {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if n := limitParam(r); n > 0 && len(out) > n {
		out = out[:n]
	}
	h.ok(w, out, warn)
}

type metaEntry struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

func (h *Handler) metadata(w http.ResponseWriter, r *http.Request) {
	if err := h.form(w, r); err != nil {
		h.fail(w, r, err)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	cat, err := h.cache.get(ctx, h.Store, h.tenant(r), h.now(), orDur(h.CatalogTTL, 10*time.Second))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	want := r.Form.Get("metric")
	out := map[string][]metaEntry{}
	for _, e := range cat.all {
		n := e.labels[nameLabel]
		if want != "" && n != want {
			continue
		}
		if len(out[n]) > 0 {
			continue
		}
		out[n] = []metaEntry{{Type: "gauge", Help: "ZeroWatch metric " + e.meta.Namespace + " " + e.meta.MetricName, Unit: e.meta.Unit}}
	}
	if lim := limitParam(r); lim > 0 && len(out) > lim {
		names := make([]string, 0, len(out))
		for n := range out {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names[lim:] {
			delete(out, n)
		}
	}
	h.ok(w, out, nil)
}

func (h *Handler) buildinfo(w http.ResponseWriter, r *http.Request) {
	h.ok(w, map[string]string{
		"version": "2.53.0", "revision": "zerowatch", "branch": "zerowatch", "buildUser": "zerowatch",
		"buildDate": "", "goVersion": runtime.Version(),
	}, nil)
}

func (h *Handler) empty(data any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.ok(w, data, nil) }
}
