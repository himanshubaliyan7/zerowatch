package alarms

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/query"
)

// Rule is a persisted alarm definition.
type Rule struct {
	ID          string            `json:"id"`
	TenantID    uint32            `json:"tenantId"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Metric      *query.MetricStat `json:"metric"`
	// Expression optionally applies metric math to the metric, referenced as m1
	// (e.g. "m1 / PERIOD(m1)").
	Expression string `json:"expression,omitempty"`
	PeriodSec  int    `json:"periodSec"`
	// ForecastHorizonSec makes the alarm predictive: each period it evaluates
	// the forecast for the next ForecastHorizonSec (the worst value in that
	// window for the comparison's direction) instead of the observed value.
	ForecastHorizonSec int       `json:"forecastHorizonSec,omitempty"`
	Config             Config    `json:"config"`
	Actions            []Target  `json:"actions,omitempty"`
	Enabled            bool      `json:"enabled"`
	State              State     `json:"state"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// Validate checks the rule definition.
func (r *Rule) Validate() error {
	if r.Name == "" || len(r.Name) > 255 {
		return fmt.Errorf("name is required (max 255 chars)")
	}
	if r.Metric == nil || r.Metric.Namespace == "" || r.Metric.MetricName == "" {
		return fmt.Errorf("metric.namespace and metric.metricName are required")
	}
	if r.Metric.Stat == "" {
		r.Metric.Stat = "Average"
	}
	if r.PeriodSec == 0 {
		r.PeriodSec = 60
	}
	if r.PeriodSec < 1 || r.PeriodSec > 86400 {
		return fmt.Errorf("periodSec must be in [1,86400]")
	}
	if r.ForecastHorizonSec != 0 && (r.ForecastHorizonSec < r.PeriodSec || r.ForecastHorizonSec > MaxForecastHorizonSec) {
		return fmt.Errorf("forecastHorizonSec must be 0 or in [periodSec,%d]", MaxForecastHorizonSec)
	}
	if r.Expression != "" {
		if _, err := query.Parse(r.Expression); err != nil {
			return fmt.Errorf("expression: %w", err)
		}
	}
	for _, a := range r.Actions {
		switch a.Type {
		case "webhook", "slack":
			if a.URL == "" {
				return fmt.Errorf("%s action requires url", a.Type)
			}
		case "pagerduty":
			if a.RoutingKey == "" {
				return fmt.Errorf("pagerduty action requires routingKey")
			}
		case "sns":
			if _, err := snsRegion(a.URL); err != nil {
				return fmt.Errorf("sns action requires url = topic ARN (arn:aws:sns:<region>:<account>:<topic>)")
			}
		case "log":
		default:
			return fmt.Errorf("unknown action type %q", a.Type)
		}
	}
	return r.Config.Validate()
}

// MaxForecastHorizonSec bounds how far ahead a predictive alarm looks (7 days).
const MaxForecastHorizonSec = 7 * 86400

// Forecaster predicts horizon future values from per-period history (nil =
// missing); implemented by forecast.Service.
type Forecaster interface {
	Predict(ctx context.Context, values []*float64, periodSec, horizon int) (forecast []float64, method string, err error)
}

// RuleSource lists enabled rules (implemented by the metadata store).
type RuleSource interface {
	ListAlarmRules(ctx context.Context, tenant uint32) ([]Rule, error)
}

// TransitionSink persists transitions and the new state.
type TransitionSink interface {
	RecordAlarmTransition(ctx context.Context, tr Transition) error
}

// Engine is the period-driven (tumbling window) evaluator. Every tick it
// closes the most recent full period for each rule, fetches the aggregated
// datapoint, steps that rule's state machine and dispatches transitions.
type Engine struct {
	Rules      RuleSource
	Sink       TransitionSink
	Query      *query.Engine
	Dispatcher *Dispatcher
	Log        *slog.Logger
	// IngestLag delays evaluation of a period so late points can land.
	IngestLag time.Duration
	Now       func() time.Time
	// OnTransition is an optional hook (e.g. live SSE fan-out).
	OnTransition func(Transition)
	// Forecaster evaluates predictive rules (ForecastHorizonSec > 0); they
	// are skipped without one.
	Forecaster Forecaster

	mu       sync.Mutex
	machines map[string]*entry
}

type entry struct {
	m        *Machine
	version  time.Time
	lastEval int64
}

// Run blocks, evaluating every interval until ctx is cancelled.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Tick(ctx); err != nil {
				e.Log.Warn("alarm tick failed", "err", err)
			}
		}
	}
}

// States returns the current in-memory state per rule id.
func (e *Engine) States() map[string]State {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]State, len(e.machines))
	for id, en := range e.machines {
		out[id] = en.m.State
	}
	return out
}

// Tick performs one evaluation pass (exported for tests).
func (e *Engine) Tick(ctx context.Context) error {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	rules, err := e.Rules.ListAlarmRules(ctx, 0)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.machines == nil {
		e.machines = map[string]*entry{}
	}
	e.mu.Unlock()
	live := map[string]bool{}
	for i := range rules {
		r := rules[i]
		live[r.ID] = true
		if !r.Enabled {
			continue
		}
		e.mu.Lock()
		en := e.machines[r.ID]
		if en == nil || !en.version.Equal(r.UpdatedAt) {
			m, err := NewMachine(r.Config)
			if err != nil {
				e.mu.Unlock()
				e.Log.Warn("invalid alarm rule", "rule", r.ID, "err", err)
				continue
			}
			en = &entry{m: m, version: r.UpdatedAt}
			e.machines[r.ID] = en
		}
		e.mu.Unlock()

		pMs := int64(r.PeriodSec) * 1000
		end := query.AlignTimestamp(now().Add(-e.IngestLag).UnixMilli(), pMs)
		if end <= en.lastEval {
			continue
		}
		if r.ForecastHorizonSec > 0 {
			if e.Forecaster == nil {
				continue
			}
			e.stepPredictive(ctx, r, en, end)
			en.lastEval = end
			continue
		}
		start := end - pMs
		if en.lastEval == 0 && r.Config.EvaluationPeriods > 1 {
			// Warm the window on first sight so restarts don't reset state to
			// INSUFFICIENT_DATA for N periods.
			start = end - pMs*int64(r.Config.EvaluationPeriods)
		}
		vals, err := e.fetch(ctx, r, start, end)
		if err != nil {
			e.Log.Warn("alarm fetch failed", "rule", r.Name, "err", err)
			continue
		}
		for i, v := range vals {
			from := en.m.State
			st, changed, reason := en.m.Step(v)
			if changed {
				tr := Transition{RuleID: r.ID, RuleName: r.Name, TenantID: r.TenantID, From: from, To: st, Reason: reason, Value: v,
					Timestamp: time.UnixMilli(start + int64(i+1)*pMs)}
				e.emit(ctx, r, tr)
			}
		}
		en.lastEval = end
	}
	e.mu.Lock()
	for id := range e.machines {
		if !live[id] {
			delete(e.machines, id)
		}
	}
	e.mu.Unlock()
	return nil
}

// predictiveLookback is the history (in periods) a predictive rule forecasts
// from: two days, at least 120 and at most 4000 periods.
func predictiveLookback(periodSec int) int {
	return max(120, min(2*86400/periodSec, 4000))
}

// stepPredictive steps r's machine once for the period ending at end with
// the worst forecast over the rule's horizon (nil when there is too little
// history or the forecast fails, so TreatMissingData decides).
func (e *Engine) stepPredictive(ctx context.Context, r Rule, en *entry, end int64) {
	pMs := int64(r.PeriodSec) * 1000
	vals, err := e.fetch(ctx, r, end-int64(predictiveLookback(r.PeriodSec))*pMs, end)
	if err != nil {
		e.Log.Warn("alarm fetch failed", "rule", r.Name, "err", err)
		return
	}
	var v *float64
	method := ""
	present := 0
	for _, x := range vals {
		if x != nil {
			present++
		}
	}
	if present >= 3 {
		horizon := (r.ForecastHorizonSec + r.PeriodSec - 1) / r.PeriodSec
		fc, m, err := e.Forecaster.Predict(ctx, vals, r.PeriodSec, horizon)
		if err != nil {
			e.Log.Warn("alarm forecast failed", "rule", r.Name, "err", err)
		} else if len(fc) > 0 {
			worst := fc[0]
			less := r.Config.Comparison == LessThanThreshold || r.Config.Comparison == LessThanOrEqualToThreshold
			for _, f := range fc[1:] {
				if less && f < worst || !less && f > worst {
					worst = f
				}
			}
			v, method = &worst, m
		}
	}
	from := en.m.State
	st, changed, reason := en.m.Step(v)
	if changed {
		reason = fmt.Sprintf("forecast (%s, next %s): %s", method, time.Duration(r.ForecastHorizonSec)*time.Second, reason)
		e.emit(ctx, r, Transition{RuleID: r.ID, RuleName: r.Name, TenantID: r.TenantID, From: from, To: st, Reason: reason, Value: v,
			Timestamp: time.UnixMilli(end)})
	}
}

func (e *Engine) emit(ctx context.Context, r Rule, tr Transition) {
	if e.Sink != nil {
		if err := e.Sink.RecordAlarmTransition(ctx, tr); err != nil {
			e.Log.Warn("record transition failed", "err", err)
		}
	}
	if e.OnTransition != nil {
		e.OnTransition(tr)
	}
	if e.Dispatcher != nil {
		targets := r.Actions
		if len(targets) == 0 {
			targets = []Target{{Type: "log"}}
		}
		e.Dispatcher.Dispatch(ctx, targets, tr)
	}
}

// fetch returns one value pointer per period in [start, end) (nil = missing).
func (e *Engine) fetch(ctx context.Context, r Rule, start, end int64) ([]*float64, error) {
	f := false
	qs := []query.MetricDataQuery{{ID: "m1", MetricStat: r.Metric}}
	target := "m1"
	if r.Expression != "" {
		qs[0].ReturnData = &f
		qs = append(qs, query.MetricDataQuery{ID: "e1", Expression: r.Expression})
		target = "e1"
	}
	res, err := e.Query.Execute(ctx, query.Request{TenantID: r.TenantID, Queries: qs, StartMs: start, EndMs: end, Period: r.PeriodSec})
	if err != nil {
		return nil, err
	}
	pMs := int64(r.PeriodSec) * 1000
	n := int((end - start) / pMs)
	out := make([]*float64, n)
	for _, rr := range res {
		if rr.ID != target {
			continue
		}
		// A partial result (e.g. a cluster storage shard down) would look like
		// low traffic and could fire "below threshold" alarms, so it counts as
		// missing data and TreatMissingData decides.
		if rr.StatusCode == "PartialData" {
			if e.Log != nil {
				e.Log.Warn("alarm data partial, treated as missing", "rule", r.Name, "messages", rr.Messages)
			}
			break
		}
		for i, ts := range rr.Timestamps {
			idx := int((ts - start) / pMs)
			if idx >= 0 && idx < n {
				v := rr.Values[i]
				out[idx] = &v
			}
		}
	}
	return out, nil
}
