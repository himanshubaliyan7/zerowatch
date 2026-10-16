package alarms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/query"
)

func f(v float64) *float64 { return &v }

func TestSlidingAlarmStateBlueprint(t *testing.T) {
	s := SlidingAlarmState{EvaluationPeriods: 5, DatapointsToAlarm: 3}
	seq := []bool{true, false, true, false, false, true, true, false}
	want := []bool{false, false, false, false, false, false, true, false}
	for i, b := range seq {
		if got := s.RecordEvaluation(b); got != want[i] {
			t.Fatalf("step %d: got %v want %v (mask %b)", i, got, want[i], s.HistoryBitmask)
		}
	}
	// N=64 must not overflow the mask
	s64 := SlidingAlarmState{EvaluationPeriods: 64, DatapointsToAlarm: 64}
	for i := 0; i < 63; i++ {
		if s64.RecordEvaluation(true) {
			t.Fatal("early alarm")
		}
	}
	if !s64.RecordEvaluation(true) {
		t.Fatal("64 of 64 should alarm")
	}
}

func cfg(n, m int, missing string) Config {
	return Config{Threshold: 80, Comparison: GreaterThanThreshold, EvaluationPeriods: n, DatapointsToAlarm: m, TreatMissingData: missing}
}

func TestStateMachineTransitions(t *testing.T) {
	m, err := NewMachine(cfg(3, 2, MissingMissing))
	if err != nil {
		t.Fatal(err)
	}
	if m.State != StateInsufficientData {
		t.Fatal("initial state")
	}
	steps := []struct {
		v    *float64
		want State
	}{
		{f(50), StateOK},
		{f(90), StateOK},    // 1 of 3
		{f(95), StateAlarm}, // 2 of 3
		{f(10), StateAlarm}, // still 2 of 3 (90,95,10)
		{f(10), StateOK},    // 1 of 3
		{nil, StateOK},      // present in window still
		{nil, StateOK},      // [10, nil, nil] → 1 present
		{nil, StateInsufficientData},
	}
	for i, s := range steps {
		got, _, reason := m.Step(s.v)
		if got != s.want {
			t.Fatalf("step %d: got %s want %s (%s)", i, got, s.want, reason)
		}
	}
}

func TestTreatMissingData(t *testing.T) {
	// breaching: missing points push towards ALARM
	m, _ := NewMachine(cfg(2, 2, MissingBreaching))
	m.Step(nil)
	if st, _, _ := m.Step(nil); st != StateAlarm {
		t.Fatalf("breaching: got %s", st)
	}
	// notBreaching: missing points are healthy
	m, _ = NewMachine(cfg(2, 2, MissingNotBreaching))
	m.Step(f(99))
	if st, _, _ := m.Step(nil); st != StateOK {
		t.Fatalf("notBreaching: got %s", st)
	}
	// ignore: state preserved and window not advanced
	m, _ = NewMachine(cfg(2, 2, MissingIgnore))
	m.Step(f(99))
	m.Step(f(99))
	for i := 0; i < 10; i++ {
		if st, changed, _ := m.Step(nil); st != StateAlarm || changed {
			t.Fatalf("ignore: got %s changed=%v", st, changed)
		}
	}
	if st, _, _ := m.Step(f(1)); st != StateOK {
		t.Fatal("ignore: window should still hold [99,99] then 1 -> OK")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{Comparison: GreaterThanThreshold, EvaluationPeriods: 0},
		{Comparison: GreaterThanThreshold, EvaluationPeriods: 65},
		{Comparison: GreaterThanThreshold, EvaluationPeriods: 3, DatapointsToAlarm: 4},
		{Comparison: "Nope", EvaluationPeriods: 3},
		{Comparison: LessThanThreshold, EvaluationPeriods: 3, TreatMissingData: "sometimes"},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
	c := Config{Comparison: LessThanOrEqualToThreshold, EvaluationPeriods: 4}
	if err := c.Validate(); err != nil || c.DatapointsToAlarm != 4 || c.TreatMissingData != MissingMissing {
		t.Fatalf("defaults not applied: %+v %v", c, err)
	}
}

func TestSimulate(t *testing.T) {
	series := []*float64{f(10), f(90), f(91), f(10), f(10), f(90), f(90), nil}
	res, err := Simulate(cfg(2, 2, MissingNotBreaching), series)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlarmCount != 2 || res.BreachCount != 4 || len(res.States) != len(series) {
		t.Fatalf("got %+v", res)
	}
}

func TestDispatcherRetriesAndFormats(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(500) // first attempt fails -> retry
			return
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.backoff = time.Millisecond
	tr := Transition{RuleID: "r1", RuleName: "HighCPU", From: StateOK, To: StateAlarm, Reason: "x", Timestamp: time.Now()}
	if err := d.DeliverSync(context.Background(), Target{Type: "webhook", URL: srv.URL}, tr); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected retry, calls=%d", calls)
	}
	if err := d.DeliverSync(context.Background(), Target{Type: "pagerduty", URL: srv.URL, RoutingKey: "k"}, tr); err != nil {
		t.Fatal(err)
	}
	if err := d.DeliverSync(context.Background(), Target{Type: "slack", URL: srv.URL}, tr); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bodies[1]["ruleName"] != "HighCPU" || bodies[2]["event_action"] != "trigger" || bodies[3]["text"] == nil {
		t.Fatalf("bad payloads %+v", bodies)
	}
	if err := d.DeliverSync(context.Background(), Target{Type: "carrier-pigeon"}, tr); err == nil {
		t.Fatal("unknown type should fail")
	}
}

// ---- engine integration with a fake fetcher

type rules []Rule

func (r rules) ListAlarmRules(context.Context, uint32) ([]Rule, error) { return r, nil }

type sink struct {
	mu  sync.Mutex
	trs []Transition
}

func (s *sink) RecordAlarmTransition(_ context.Context, tr Transition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trs = append(s.trs, tr)
	return nil
}

type fetch struct {
	pts     []model.Point
	partial bool // return pts as a query.PartialError (a shard was down)
}

func (ff *fetch) FetchStat(context.Context, uint32, query.MetricStat, int64, int64, int) ([]model.Point, error) {
	if ff.partial {
		return nil, &query.PartialError{Points: ff.pts, Msg: "1 of 2 shards unavailable"}
	}
	return ff.pts, nil
}

// Partial data must not drive the state machine: a "below threshold" rule
// would otherwise fire on a shard outage. It counts as missing data.
func TestEnginePartialDataIsMissing(t *testing.T) {
	now := time.UnixMilli(600_000 + 5_000)
	ff := &fetch{pts: []model.Point{{T: 420_000, V: 1}, {T: 480_000, V: 1}, {T: 540_000, V: 1}}, partial: true}
	c := cfg(3, 3, MissingMissing)
	c.Comparison, c.Threshold = LessThanThreshold, 50
	r := Rule{ID: "low", Name: "traffic low", Enabled: true, PeriodSec: 60, Metric: &query.MetricStat{Namespace: "n", MetricName: "req"},
		Config: c, UpdatedAt: time.Unix(1, 0)}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	run := func(partial bool) State {
		ff.partial = partial
		e := &Engine{Rules: rules{r}, Sink: &sink{}, Query: &query.Engine{Fetcher: ff}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
		if err := e.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		return e.States()["low"]
	}
	if st := run(false); st != StateAlarm {
		t.Fatalf("complete low traffic should alarm; state %v", st)
	}
	if st := run(true); st == StateAlarm {
		t.Fatalf("the same values as partial data fired the alarm; state %v", st)
	}
}

func TestEngineTick(t *testing.T) {
	now := time.UnixMilli(600_000 + 5_000)
	ff := &fetch{pts: []model.Point{{T: 420_000, V: 95}, {T: 480_000, V: 97}, {T: 540_000, V: 99}}}
	s := &sink{}
	r := Rule{ID: "r1", Name: "cpu", Enabled: true, PeriodSec: 60, Metric: &query.MetricStat{Namespace: "n", MetricName: "cpu"},
		Config: cfg(3, 3, MissingMissing), UpdatedAt: time.Unix(1, 0)}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Rules: rules{r}, Sink: s, Query: &query.Engine{Fetcher: ff}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.States()["r1"] != StateAlarm {
		t.Fatalf("state %v transitions %+v", e.States(), s.trs)
	}
	// re-tick within same period: no duplicate evaluation
	_ = e.Tick(context.Background())
	// first tick: INSUFFICIENT_DATA->OK (warm-up) then OK->ALARM
	if len(s.trs) != 2 {
		t.Fatalf("expected exactly 2 transitions, got %d", len(s.trs))
	}
	// next period has a healthy point -> still ALARM is false since 3-of-3 required
	ff.pts = append(ff.pts, model.Point{T: 600_000, V: 10})
	now = now.Add(time.Minute)
	_ = e.Tick(context.Background())
	if e.States()["r1"] != StateOK {
		t.Fatalf("expected OK, got %v", e.States())
	}
}

func TestRuleValidation(t *testing.T) {
	r := Rule{Name: "x", Metric: &query.MetricStat{Namespace: "n", MetricName: "m"}, Config: cfg(1, 1, ""), Actions: []Target{{Type: "webhook"}}}
	if err := r.Validate(); err == nil {
		t.Fatal("webhook without url should fail")
	}
	r.Actions = nil
	r.Expression = "m1 +"
	if err := r.Validate(); err == nil {
		t.Fatal("bad expression should fail")
	}
	_ = errors.New
}

type fakeForecaster struct {
	fc   []float64
	seen int
}

func (f *fakeForecaster) Predict(_ context.Context, values []*float64, periodSec, horizon int) ([]float64, string, error) {
	f.seen = len(values)
	return f.fc[:horizon], "fake", nil
}

func TestEnginePredictive(t *testing.T) {
	now := time.UnixMilli(600_000 + 5_000)
	ff := &fetch{pts: []model.Point{{T: 420_000, V: 50}, {T: 480_000, V: 60}, {T: 540_000, V: 70}}}
	c := cfg(1, 1, MissingMissing) // > 80
	r := Rule{ID: "p", Name: "disk soon full", Enabled: true, PeriodSec: 60, ForecastHorizonSec: 180,
		Metric: &query.MetricStat{Namespace: "n", MetricName: "disk"}, Config: c, UpdatedAt: time.Unix(1, 0)}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	fc := &fakeForecaster{fc: []float64{80, 95, 85, 200}}
	s := &sink{}
	e := &Engine{Rules: rules{r}, Sink: s, Query: &query.Engine{Fetcher: ff}, Forecaster: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Observed values (≤ 70) never breach; the forecast does (95 within 3 periods; 200 is beyond the horizon).
	if e.States()["p"] != StateAlarm || len(s.trs) != 1 || *s.trs[0].Value != 95 || !strings.Contains(s.trs[0].Reason, "forecast (fake") {
		t.Fatalf("state %v transitions %+v", e.States(), s.trs)
	}
	if fc.seen != predictiveLookback(60) {
		t.Errorf("forecaster got %d periods of history, want %d", fc.seen, predictiveLookback(60))
	}
	// Without a forecaster predictive rules are skipped, not evaluated on raw values.
	e2 := &Engine{Rules: rules{r}, Sink: &sink{}, Query: &query.Engine{Fetcher: ff}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	_ = e2.Tick(context.Background())
	if st := e2.States()["p"]; st != StateInsufficientData {
		t.Fatalf("no forecaster: state %v", st)
	}
	r.ForecastHorizonSec = 30
	if err := r.Validate(); err == nil {
		t.Fatal("horizon below one period should fail validation")
	}
}
