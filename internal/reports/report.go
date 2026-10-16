// Package reports builds the periodic summary report (traffic, errors and
// latency per service, alarm activity, anomalies, slow traces and capacity
// forecasts), renders it as JSON or self-contained HTML, sends it to the
// configured targets and schedules it. The JSON shape is Report in
// web/src/lib/api.ts.
package reports

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zerowatch/zerowatch/internal/alarms"
	"github.com/zerowatch/zerowatch/internal/anomaly"
	"github.com/zerowatch/zerowatch/internal/forecast"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/query"
	"github.com/zerowatch/zerowatch/internal/storage"
	"github.com/zerowatch/zerowatch/internal/traces"
)

// REDNamespace is where the probe and generator write Requests/Errors/Latency.
const REDNamespace = "ZeroWatch/RED"

// Range is one supported report window.
type Range struct {
	ID       string
	Duration time.Duration
	Period   int // seconds per bucket
	Season   int // buckets per season for anomaly detection
}

// Ranges are the supported report windows.
var Ranges = map[string]Range{
	"24h": {ID: "24h", Duration: 24 * time.Hour, Period: 300, Season: 12},     // 5 min buckets, 1 h season
	"7d":  {ID: "7d", Duration: 7 * 24 * time.Hour, Period: 3600, Season: 24}, // 1 h buckets, 1 day season
	"30d": {ID: "30d", Duration: 30 * 24 * time.Hour, Period: 14400, Season: 6},
}

// ErrBadRange is returned for an unknown range id.
var ErrBadRange = fmt.Errorf("%w: range must be 24h, 7d or 30d", query.ErrBadRequest)

// Report is the summary (web/src/lib/api.ts Report). Percentiles and
// availability over a range are derived from per-bucket values:
//   - p50/p99 are request-weighted means of the per-bucket percentiles (exact
//     range percentiles would need every raw sample);
//   - availabilityPct is the share of buckets with traffic whose error rate
//     stayed below 5 %.
type Report struct {
	GeneratedAt time.Time             `json:"generatedAt"`
	Range       RangeInfo             `json:"range"`
	Totals      Totals                `json:"totals"`
	Services    []Service             `json:"services"`
	Alarms      []Alarm               `json:"alarms"`
	Anomalies   []Anomaly             `json:"anomalies"`
	SlowTraces  []traces.TraceSummary `json:"slowTraces"`
	Capacity    []Capacity            `json:"capacity"`
	Narrative   string                `json:"narrative,omitempty"`
}

// RangeInfo names the covered window (unix ms).
type RangeInfo struct {
	ID    string `json:"id"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

// Totals are across every service.
type Totals struct {
	Requests        float64  `json:"requests"`
	ErrorPct        *float64 `json:"errorPct"`
	P50             *float64 `json:"p50"`
	P99             *float64 `json:"p99"`
	AvailabilityPct *float64 `json:"availabilityPct"`
}

// Service is one row of the per-service table.
type Service struct {
	Name            string   `json:"name"`
	Requests        float64  `json:"requests"`
	ErrorPct        *float64 `json:"errorPct"`
	P99             *float64 `json:"p99"`
	P99ChangePct    *float64 `json:"p99ChangePct"`
	AvailabilityPct *float64 `json:"availabilityPct"`
}

// Alarm is one rule's activity in the window.
type Alarm struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	Fired          int          `json:"fired"`
	MinutesInAlarm float64      `json:"minutesInAlarm"`
	State          alarms.State `json:"state"`
}

// Anomaly is a bucket outside its robust STL band.
type Anomaly struct {
	Service       string  `json:"service"`
	Metric        string  `json:"metric"`
	At            int64   `json:"at"`
	Value         float64 `json:"value"`
	ExpectedLower float64 `json:"expectedLower"`
	ExpectedUpper float64 `json:"expectedUpper"`
}

// Capacity says when an alarm rule's threshold is forecast to be crossed.
type Capacity struct {
	Label     string  `json:"label"`
	Service   string  `json:"service,omitempty"`
	Metric    string  `json:"metric"`
	Threshold float64 `json:"threshold"`
	CrossesAt *int64  `json:"crossesAt"`
	Method    string  `json:"method"`
}

// SeriesLister finds the services (storage.Engine implements it).
type SeriesLister interface {
	ListSeries(ctx context.Context, tenant uint32, f storage.SeriesFilter) ([]model.SeriesMeta, error)
}

// AlarmSource is the metadata store's alarm side.
type AlarmSource interface {
	ListAlarmRules(ctx context.Context, tenant uint32) ([]alarms.Rule, error)
	ListAlarmHistory(ctx context.Context, tenant uint32, ruleID string, limit int) ([]alarms.Transition, error)
}

// Builder assembles reports; every dependency but Series and Query is optional.
type Builder struct {
	Series   SeriesLister
	Query    *query.Engine
	Alarms   AlarmSource
	States   func() map[string]alarms.State // live alarm states (alarms.Engine.States)
	Traces   traces.Backend
	Forecast *forecast.Service
	Targets  []alarms.Target
	Client   *http.Client
	Log      *slog.Logger
	Now      func() time.Time
}

const (
	maxServices  = 50
	maxCapacity  = 20
	maxAnomalies = 20
	slowTraceN   = 10
	badErrorRate = 0.05
)

// Build assembles the report for range id rangeID.
func (b *Builder) Build(ctx context.Context, tenant uint32, rangeID string) (*Report, error) {
	rg, ok := Ranges[rangeID]
	if !ok {
		return nil, ErrBadRange
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	pMs := int64(rg.Period) * 1000
	end := query.AlignTimestamp(now().UnixMilli(), pMs)
	durMs := rg.Duration.Milliseconds()
	cur, prev := window{end - durMs, end, pMs}, window{end - 2*durMs, end - durMs, pMs}
	rep := &Report{GeneratedAt: now().UTC().Truncate(time.Second), Range: RangeInfo{ID: rg.ID, Start: cur.start, End: cur.end},
		Services: []Service{}, Alarms: []Alarm{}, Anomalies: []Anomaly{}, SlowTraces: []traces.TraceSummary{}, Capacity: []Capacity{}}

	names, err := b.services(ctx, tenant)
	if err != nil {
		return nil, err
	}
	all := append([]string{"*"}, names...)
	cs, err := b.red(ctx, tenant, all, cur, rg.Period, true)
	if err != nil {
		return nil, err
	}
	ps, err := b.red(ctx, tenant, names, prev, rg.Period, false)
	if err != nil {
		return nil, err
	}
	t := cs["*"]
	rep.Totals = Totals{Requests: sum(t.req), ErrorPct: errPct(t.req, t.err), P50: weighted(t.p50, t.req), P99: weighted(t.p99, t.req), AvailabilityPct: availability(t.req, t.err)}
	for _, n := range names {
		c := cs[n]
		s := Service{Name: n, Requests: sum(c.req), ErrorPct: errPct(c.req, c.err), P99: weighted(c.p99, c.req), AvailabilityPct: availability(c.req, c.err)}
		if p := ps[n]; s.P99 != nil {
			if old := weighted(p.p99, p.req); old != nil && *old > 0 {
				s.P99ChangePct = ptr((*s.P99 - *old) / *old * 100)
			}
		}
		rep.Services = append(rep.Services, s)
		rep.Anomalies = append(rep.Anomalies, detect(n, "Latency p99", c.p99, cur, rg.Season)...)
		rep.Anomalies = append(rep.Anomalies, detect(n, "Requests", c.req, cur, rg.Season)...)
	}
	sort.SliceStable(rep.Services, func(i, j int) bool { return rep.Services[i].Requests > rep.Services[j].Requests })
	sort.SliceStable(rep.Anomalies, func(i, j int) bool { return rep.Anomalies[i].At > rep.Anomalies[j].At })
	if len(rep.Anomalies) > maxAnomalies {
		rep.Anomalies = rep.Anomalies[:maxAnomalies]
	}

	if b.Alarms != nil {
		rules, err := b.Alarms.ListAlarmRules(ctx, tenant)
		if err != nil {
			return nil, err
		}
		hist, err := b.Alarms.ListAlarmHistory(ctx, tenant, "", 1000)
		if err != nil {
			return nil, err
		}
		rep.Alarms = alarmActivity(rules, hist, b.liveStates(), cur, now())
		rep.Capacity = b.capacity(ctx, tenant, rules, cur, rg)
	}
	if b.Traces != nil {
		ts := b.Traces.Search(tenant, traces.Query{Start: float64(cur.start), End: float64(now().UnixMilli()), Limit: traces.MaxSearchLimit})
		sort.SliceStable(ts, func(i, j int) bool { return ts[i].DurationMs > ts[j].DurationMs })
		rep.SlowTraces = ts[:min(len(ts), slowTraceN)]
	}
	rep.Narrative = narrative(rep)
	return rep, nil
}

func (b *Builder) liveStates() map[string]alarms.State {
	if b.States == nil {
		return nil
	}
	return b.States()
}

// services lists the distinct Service dimension values of RED Requests.
func (b *Builder) services(ctx context.Context, tenant uint32) ([]string, error) {
	// A partial list (cluster shards down) still makes a useful report; the
	// per-service queries then come back PartialData on their own.
	ss, err := b.Series.ListSeries(ctx, tenant, storage.SeriesFilter{Namespace: REDNamespace, MetricName: "Requests", Limit: 5000})
	if err != nil && !storage.IsPartial(err) {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if n := s.Dimensions["Service"]; n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out[:min(len(out), maxServices)], nil
}

type window struct{ start, end, pMs int64 }

func (w window) n() int { return int((w.end - w.start) / w.pMs) }

type red struct{ req, err, p50, p99 []float64 }

// red fetches per-bucket RED values for each service ("*" = all services).
func (b *Builder) red(ctx context.Context, tenant uint32, services []string, w window, period int, withP50 bool) (map[string]red, error) {
	out := map[string]red{}
	if len(services) == 0 {
		return out, nil
	}
	var qs []query.MetricDataQuery
	ms := func(id, metric, stat, svc string) query.MetricDataQuery {
		return query.MetricDataQuery{ID: id, MetricStat: &query.MetricStat{Namespace: REDNamespace, MetricName: metric, Stat: stat,
			Dimensions: map[string]string{"Service": svc, "Route": "*"}}}
	}
	for i, s := range services {
		qs = append(qs, ms(fmt.Sprintf("r%d", i), "Requests", "Sum", s), ms(fmt.Sprintf("e%d", i), "Errors", "Sum", s), ms(fmt.Sprintf("q%d", i), "Latency", "p99", s))
		if withP50 {
			qs = append(qs, ms(fmt.Sprintf("m%d", i), "Latency", "p50", s))
		}
	}
	res, err := b.Query.Execute(ctx, query.Request{TenantID: tenant, Queries: qs, StartMs: w.start, EndMs: w.end, Period: period})
	if err != nil {
		return nil, err
	}
	byID := map[string][]float64{}
	for _, r := range res {
		v := make([]float64, w.n())
		for i := range v {
			v[i] = math.NaN()
		}
		for i, ts := range r.Timestamps {
			if idx := int((ts - w.start) / w.pMs); idx >= 0 && idx < len(v) {
				v[idx] = r.Values[i]
			}
		}
		byID[r.ID] = v
	}
	for i, s := range services {
		out[s] = red{req: byID[fmt.Sprintf("r%d", i)], err: byID[fmt.Sprintf("e%d", i)], p50: byID[fmt.Sprintf("m%d", i)], p99: byID[fmt.Sprintf("q%d", i)]}
	}
	return out, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func ptr(v float64) *float64 { return &v }

func sum(v []float64) float64 {
	var s float64
	for _, x := range v {
		if finite(x) {
			s += x
		}
	}
	return s
}

func errPct(req, errs []float64) *float64 {
	r := sum(req)
	if r <= 0 {
		return nil
	}
	return ptr(sum(errs) / r * 100)
}

// weighted is the w-weighted mean of p over buckets where both are present
// (the plain mean when no weights are present); nil without data.
func weighted(p, w []float64) *float64 {
	var num, den, plain float64
	var n int
	for i, x := range p {
		if !finite(x) {
			continue
		}
		plain += x
		n++
		if i < len(w) && finite(w[i]) && w[i] > 0 {
			num += x * w[i]
			den += w[i]
		}
	}
	switch {
	case den > 0:
		return ptr(num / den)
	case n > 0:
		return ptr(plain / float64(n))
	}
	return nil
}

func availability(req, errs []float64) *float64 {
	var good, total int
	for i, r := range req {
		if !finite(r) || r <= 0 {
			continue
		}
		total++
		e := 0.0
		if i < len(errs) && finite(errs[i]) {
			e = errs[i]
		}
		if e/r < badErrorRate {
			good++
		}
	}
	if total == 0 {
		return nil
	}
	return ptr(float64(good) / float64(total) * 100)
}

// detect returns up to 3 of the largest robust-STL anomalies in v. It needs
// two seasons of data after the first present bucket, at most half missing.
func detect(service, metric string, v []float64, w window, season int) []Anomaly {
	first := -1
	missing := 0
	for i, x := range v {
		if finite(x) {
			if first < 0 {
				first = i
			}
		} else if first >= 0 {
			missing++
		}
	}
	if first < 0 || len(v)-first < 2*season || missing*2 > len(v)-first {
		return nil
	}
	y := append([]float64(nil), v[first:]...)
	last := 0
	for i := 1; i < len(y); i++ { // linear gap fill; gaps are never reported
		if !finite(y[i]) {
			continue
		}
		for j := last + 1; j < i; j++ {
			y[j] = y[last] + (y[i]-y[last])*float64(j-last)/float64(i-last)
		}
		last = i
	}
	for j := last + 1; j < len(y); j++ {
		y[j] = y[last]
	}
	bands, err := anomaly.DetectSTL(anomaly.STLParams{Period: season, Robust: true}, y, 4)
	if err != nil {
		return nil
	}
	type hit struct {
		a   Anomaly
		dev float64
	}
	var hits []hit
	for i, bd := range bands {
		if !bd.Anomalous || !finite(v[first+i]) {
			continue
		}
		dev := math.Max(bd.Lower-bd.Actual, bd.Actual-bd.Upper) / math.Max(bd.Upper-bd.Lower, 1e-9)
		hits = append(hits, hit{Anomaly{Service: service, Metric: metric, At: w.start + int64(first+i)*w.pMs, Value: bd.Actual,
			ExpectedLower: bd.Lower, ExpectedUpper: bd.Upper}, dev})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].dev > hits[j].dev })
	out := make([]Anomaly, 0, 3)
	for _, h := range hits[:min(len(hits), 3)] {
		out = append(out, h.a)
	}
	return out
}

// alarmActivity counts transitions into ALARM and minutes spent in ALARM per
// rule within w (history is newest first).
func alarmActivity(rules []alarms.Rule, hist []alarms.Transition, live map[string]alarms.State, w window, now time.Time) []Alarm {
	byRule := map[string][]alarms.Transition{}
	for _, tr := range hist {
		byRule[tr.RuleID] = append(byRule[tr.RuleID], tr)
	}
	end := min(w.end, now.UnixMilli())
	out := make([]Alarm, 0, len(rules))
	for _, r := range rules {
		a := Alarm{ID: r.ID, Name: r.Name, State: r.State}
		if s, ok := live[r.ID]; ok {
			a.State = s
		}
		trs := byRule[r.ID]
		sort.SliceStable(trs, func(i, j int) bool { return trs[i].Timestamp.Before(trs[j].Timestamp) })
		inAlarm, since := false, w.start
		var ms int64
		for _, tr := range trs {
			ts := tr.Timestamp.UnixMilli()
			if ts < w.start {
				inAlarm = tr.To == alarms.StateAlarm
				continue
			}
			if ts > end {
				break
			}
			if inAlarm {
				ms += ts - since
			}
			if tr.To == alarms.StateAlarm {
				a.Fired++
			}
			inAlarm, since = tr.To == alarms.StateAlarm, ts
		}
		if inAlarm {
			ms += end - since
		}
		a.MinutesInAlarm = math.Round(float64(ms)/60000*10) / 10
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Fired != out[j].Fired {
			return out[i].Fired > out[j].Fired
		}
		return out[i].MinutesInAlarm > out[j].MinutesInAlarm
	})
	return out
}

// capacity forecasts each enabled rule's metric one range ahead (capped at
// forecast.MaxHorizon periods) and reports when it first crosses the threshold.
func (b *Builder) capacity(ctx context.Context, tenant uint32, rules []alarms.Rule, w window, rg Range) []Capacity {
	out := []Capacity{}
	if b.Forecast == nil {
		return out
	}
	for _, r := range rules {
		if len(out) >= maxCapacity {
			break
		}
		if !r.Enabled || r.Metric == nil {
			continue
		}
		p := int64(r.PeriodSec) * 1000
		horizon := int(min(rg.Duration.Milliseconds()/p, forecast.MaxHorizon))
		start := w.end - min(2*rg.Duration.Milliseconds(), forecast.MaxHistory*p)
		thr := r.Config.Threshold
		res, err := b.Forecast.Forecast(ctx, tenant, forecast.Request{MetricStat: r.Metric, Expression: r.Expression, Start: start, End: w.end,
			Period: r.PeriodSec, Horizon: horizon, Threshold: &thr, Comparison: r.Config.Comparison})
		if err != nil {
			if b.Log != nil {
				b.Log.Debug("capacity forecast skipped", "rule", r.Name, "err", err)
			}
			continue
		}
		c := Capacity{Label: r.Name, Metric: r.Metric.MetricName + " " + r.Metric.Stat, Threshold: thr, CrossesAt: res.CrossesAt, Method: res.Method}
		if s := r.Metric.Dimensions["Service"]; s != "*" {
			c.Service = s
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].CrossesAt, out[j].CrossesAt
		return a != nil && (b == nil || *a < *b)
	})
	return out
}

// narrative is a short plain-text summary for chat targets and the page lede.
func narrative(r *Report) string {
	var sb strings.Builder
	if r.Totals.Requests == 0 {
		sb.WriteString("No RED traffic was recorded in this window.")
	} else {
		fmt.Fprintf(&sb, "Served %s requests", humanCount(r.Totals.Requests))
		if r.Totals.ErrorPct != nil {
			fmt.Fprintf(&sb, " at a %.2f%% error rate", *r.Totals.ErrorPct)
		}
		if r.Totals.P99 != nil {
			fmt.Fprintf(&sb, ", p99 %.0f ms", *r.Totals.P99)
		}
		sb.WriteString(".")
	}
	var worst *Service
	for i := range r.Services {
		s := &r.Services[i]
		if s.P99ChangePct != nil && *s.P99ChangePct > 20 && (worst == nil || *s.P99ChangePct > *worst.P99ChangePct) {
			worst = s
		}
	}
	if worst != nil {
		fmt.Fprintf(&sb, " %s got slower: p99 up %.0f%% on the previous period.", worst.Name, *worst.P99ChangePct)
	}
	fired, firing := 0, []string{}
	for _, a := range r.Alarms {
		fired += a.Fired
		if a.State == alarms.StateAlarm {
			firing = append(firing, a.Name)
		}
	}
	if fired > 0 {
		times := fmt.Sprintf("%d times", fired)
		if fired == 1 {
			times = "once"
		}
		fmt.Fprintf(&sb, " Alarms fired %s.", times)
	}
	if len(firing) > 0 {
		fmt.Fprintf(&sb, " In alarm now: %s.", strings.Join(firing, ", "))
	}
	if len(r.Anomalies) > 0 {
		fmt.Fprintf(&sb, " %d anomalies detected.", len(r.Anomalies))
	}
	for _, c := range r.Capacity {
		if c.CrossesAt != nil {
			fmt.Fprintf(&sb, " %q is forecast to cross %g at %s.", c.Label, c.Threshold, time.UnixMilli(*c.CrossesAt).UTC().Format("Jan 2 15:04 MST"))
			break
		}
	}
	return sb.String()
}

func humanCount(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.1fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}
