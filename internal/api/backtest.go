package api

import (
	"math"
	"net/http"
	"time"

	"github.com/zerowatch/zerowatch/internal/alarms"
	"github.com/zerowatch/zerowatch/internal/forecast"
	"github.com/zerowatch/zerowatch/internal/httpx"
	"github.com/zerowatch/zerowatch/internal/query"
)

// Backtest limits: at most maxBacktestEvals forecasts per request, each from
// at most backtestLookback periods of history.
const (
	maxBacktestEvals = 96
	backtestLookback = 720
)

// backtestResult is the answer of POST /api/v1/alarms/backtest.
type backtestResult struct {
	// Forecast is the worst forecast value per period (what the predictive
	// rule would have compared with the threshold); nil = too little history.
	Forecast  []*float64              `json:"forecast"`
	Predicted alarms.SimulationResult `json:"predicted"`
	// Observed is the same rule evaluated on the observed values, to compare.
	Observed alarms.SimulationResult `json:"observed"`
	// LeadSec is how much earlier the first predicted ALARM came than the
	// first observed one (negative: later); nil unless both fired.
	LeadSec *int64 `json:"leadSec"`
	// Stride: the forecast was evaluated every Stride periods and held in between.
	Stride int    `json:"stride"`
	Start  int64  `json:"start"`
	Method string `json:"method"`
}

// backtest replays a predictive rule (forecastHorizonSec > 0) over the last
// `hours` (default 24): at every stride-th period it forecasts from the
// history up to that period with the built-in methods (the model service
// would be too slow for ~100 calls), takes the worst value in the horizon,
// and steps the rule's state machine with it.
func (s *Server) backtest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rule  alarms.Rule `json:"rule"`
		Hours int         `json:"hours"`
	}
	if err := httpx.DecodeJSON(w, r, &body, maxBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	rule := body.Rule
	if rule.Name == "" {
		rule.Name = "backtest"
	}
	if err := rule.Validate(); err != nil {
		httpx.WriteError(w, r, httpx.BadRequest(err.Error()))
		return
	}
	if rule.ForecastHorizonSec <= 0 {
		httpx.WriteError(w, r, httpx.BadRequest("rule.forecastHorizonSec must be set: backtest replays predictive rules (use /alarms/simulate otherwise)"))
		return
	}
	if body.Hours == 0 {
		body.Hours = 24
	}
	if body.Hours < 1 || body.Hours > 7*24 {
		httpx.WriteError(w, r, httpx.BadRequest("hours must be in [1,168]"))
		return
	}
	pMs := int64(rule.PeriodSec) * 1000
	end := query.AlignTimestamp(time.Now().UnixMilli(), pMs)
	start := end - int64(body.Hours)*3600_000
	n := int((end - start) / pMs)
	lookback := min(backtestLookback, max(120, 2*86400/rule.PeriodSec))
	from := start - int64(lookback)*pMs

	qs := []query.MetricDataQuery{{ID: "m1", MetricStat: rule.Metric}}
	target := "m1"
	if rule.Expression != "" {
		f := false
		qs[0].ReturnData = &f
		qs = append(qs, query.MetricDataQuery{ID: "e1", Expression: rule.Expression})
		target = "e1"
	}
	res, err := s.Query.Execute(r.Context(), query.Request{TenantID: httpx.Tenant(r.Context()), Queries: qs, StartMs: from, EndMs: end, Period: rule.PeriodSec})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	dense := make([]float64, lookback+n) // NaN = missing
	for i := range dense {
		dense[i] = math.NaN()
	}
	for _, rr := range res {
		if rr.ID != target {
			continue
		}
		for i, ts := range rr.Timestamps {
			if idx := int((ts - from) / pMs); idx >= 0 && idx < len(dense) {
				dense[idx] = rr.Values[i]
			}
		}
	}

	stride := max(1, (n+maxBacktestEvals-1)/maxBacktestEvals)
	horizon := (rule.ForecastHorizonSec + rule.PeriodSec - 1) / rule.PeriodSec
	less := rule.Config.Comparison == alarms.LessThanThreshold || rule.Config.Comparison == alarms.LessThanOrEqualToThreshold
	out := backtestResult{Forecast: make([]*float64, n), Stride: stride, Start: start, Method: "built-in auto"}
	season := forecast.DefaultSeason(rule.PeriodSec)
	for i := 0; i < n; i += stride {
		hist := dense[i+1 : lookback+i+1] // the lookback periods up to and including period i
		present := 0
		for _, v := range hist {
			if !math.IsNaN(v) {
				present++
			}
		}
		if present < forecast.MinPoints {
			continue
		}
		p, _, _, err := forecast.Predict(forecast.Auto, hist, forecast.Options{Season: season, Horizon: horizon})
		if err != nil || len(p.Forecast) == 0 {
			continue
		}
		worst := p.Forecast[0]
		for _, f := range p.Forecast[1:] {
			if less && f < worst || !less && f > worst {
				worst = f
			}
		}
		for k := i; k < min(n, i+stride); k++ {
			v := worst
			out.Forecast[k] = &v
		}
	}
	observed := make([]*float64, n)
	for i := range observed {
		if v := dense[lookback+i]; !math.IsNaN(v) {
			observed[i] = &v
		}
	}
	if out.Predicted, err = alarms.Simulate(rule.Config, out.Forecast); err == nil {
		out.Observed, err = alarms.Simulate(rule.Config, observed)
	}
	if err != nil {
		httpx.WriteError(w, r, httpx.BadRequest(err.Error()))
		return
	}
	fp, fo := firstAlarm(out.Predicted.States), firstAlarm(out.Observed.States)
	if fp >= 0 && fo >= 0 {
		lead := int64(fo-fp) * int64(rule.PeriodSec)
		out.LeadSec = &lead
	}
	httpx.WriteJSON(w, 200, out)
}

func firstAlarm(states []alarms.State) int {
	for i, s := range states {
		if s == alarms.StateAlarm {
			return i
		}
	}
	return -1
}
