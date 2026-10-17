package reports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
	"github.com/himanshubaliyan7/zerowatch/internal/forecast"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

type fakeAlarms struct {
	rules []alarms.Rule
	hist  []alarms.Transition
}

func (f fakeAlarms) ListAlarmRules(context.Context, uint32) ([]alarms.Rule, error) {
	return f.rules, nil
}
func (f fakeAlarms) ListAlarmHistory(context.Context, uint32, string, int) ([]alarms.Transition, error) {
	return f.hist, nil
}

// seed writes 3 h of per-minute RED data for two services: "api" (100 req,
// 2 errors, 50 ms per minute) and "db" (50 req, 0 errors, latency ramping up).
func seed(t *testing.T, now time.Time) *storage.Memory {
	t.Helper()
	m := storage.NewMemory(storage.MemoryConfig{})
	var ds []model.MetricDatum
	start := now.Add(-3 * time.Hour).Truncate(time.Minute)
	for ts := start; ts.Before(now); ts = ts.Add(time.Minute) {
		i := float64(ts.Sub(start) / time.Minute)
		for _, s := range []struct {
			name      string
			req, errs float64
			lat       float64
		}{{"api", 100, 2, 50}, {"db", 50, 0, 10 + i/2}} {
			dims := []model.Dimension{{Name: "Service", Value: s.name}, {Name: "Route", Value: "/x"}}
			ms := ts.UnixMilli()
			ds = append(ds,
				model.MetricDatum{TenantID: 1, Namespace: REDNamespace, MetricName: "Requests", Dimensions: dims, Timestamp: ms, Value: model.Float(s.req)},
				model.MetricDatum{TenantID: 1, Namespace: REDNamespace, MetricName: "Errors", Dimensions: dims, Timestamp: ms, Value: model.Float(s.errs)},
				model.MetricDatum{TenantID: 1, Namespace: REDNamespace, MetricName: "Latency", Dimensions: dims, Timestamp: ms, Value: model.Float(s.lat)})
		}
	}
	if err := m.WriteMetrics(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuild(t *testing.T) {
	now := time.Now()
	st := seed(t, now)
	qe := &query.Engine{Fetcher: st, MaxParallel: 4}
	lat := &query.MetricStat{Namespace: REDNamespace, MetricName: "Latency", Stat: "p99", Dimensions: map[string]string{"Service": "db", "Route": "*"}}
	rules := []alarms.Rule{
		{ID: "a1", Name: "db slow", Enabled: true, PeriodSec: 60, Metric: lat, State: alarms.StateOK,
			Config: alarms.Config{Threshold: 200, Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 1, DatapointsToAlarm: 1}},
		{ID: "a2", Name: "quiet", Enabled: true, PeriodSec: 60, Metric: lat, State: alarms.StateOK,
			Config: alarms.Config{Threshold: 1e9, Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 1, DatapointsToAlarm: 1}},
	}
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	hist := []alarms.Transition{ // newest first, as the store returns them
		{RuleID: "a1", To: alarms.StateOK, Timestamp: at(30 * time.Minute)},
		{RuleID: "a1", To: alarms.StateAlarm, Timestamp: at(50 * time.Minute)},
		{RuleID: "a1", To: alarms.StateOK, Timestamp: at(2 * time.Hour)},
		{RuleID: "a1", To: alarms.StateAlarm, Timestamp: at(2*time.Hour + 10*time.Minute)},
	}
	b := &Builder{Series: st, Query: qe, Alarms: fakeAlarms{rules, hist}, Forecast: &forecast.Service{Query: qe},
		States: func() map[string]alarms.State { return map[string]alarms.State{"a1": alarms.StateAlarm} }}
	rep, err := b.Build(context.Background(), 1, "24h")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rep.Totals.Requests, 150.0*180; math.Abs(got-want) > 150*6 {
		t.Errorf("total requests %v, want ≈ %v", got, want)
	}
	if rep.Totals.ErrorPct == nil || math.Abs(*rep.Totals.ErrorPct-200.0/150) > 0.05 {
		t.Errorf("total error pct %v", rep.Totals.ErrorPct)
	}
	if len(rep.Services) != 2 || rep.Services[0].Name != "api" {
		t.Fatalf("services %+v", rep.Services)
	}
	api := rep.Services[0]
	if api.ErrorPct == nil || math.Abs(*api.ErrorPct-2) > 0.01 || api.P99 == nil || *api.P99 != 50 {
		t.Errorf("api row %+v", api)
	}
	if api.AvailabilityPct == nil || *api.AvailabilityPct != 100 {
		t.Errorf("api availability %v (2%% errors is a good period)", api.AvailabilityPct)
	}
	if api.P99ChangePct != nil {
		t.Errorf("no previous-period data, change should be null: %v", *api.P99ChangePct)
	}
	// Alarms: a1 fired twice, 10 + 20 minutes in ALARM, live state wins.
	if len(rep.Alarms) != 2 || rep.Alarms[0].ID != "a1" || rep.Alarms[0].Fired != 2 || math.Abs(rep.Alarms[0].MinutesInAlarm-30) > 0.2 || rep.Alarms[0].State != alarms.StateAlarm {
		t.Errorf("alarms %+v", rep.Alarms)
	}
	// Capacity: db latency climbs 0.5 ms/min from ~100 → crosses 200 within a day; "quiet" never.
	if len(rep.Capacity) != 2 || rep.Capacity[0].Label != "db slow" || rep.Capacity[0].CrossesAt == nil || rep.Capacity[1].CrossesAt != nil {
		t.Fatalf("capacity %+v", rep.Capacity)
	}
	if c := rep.Capacity[0]; c.Service != "db" || *c.CrossesAt <= now.UnixMilli() || *c.CrossesAt > now.Add(6*time.Hour).UnixMilli() {
		t.Errorf("capacity row %+v (crosses %v)", c, time.UnixMilli(*c.CrossesAt))
	}
	if !strings.Contains(rep.Narrative, "Served") || !strings.Contains(rep.Narrative, "In alarm now: db slow") {
		t.Errorf("narrative %q", rep.Narrative)
	}
	var buf bytes.Buffer
	if err := RenderHTML(&buf, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<!doctype html>", "db slow", "Last 24h.", "2.00%"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("html lacks %q", want)
		}
	}
	if _, err := b.Build(context.Background(), 1, "1y"); !errors.Is(err, query.ErrBadRequest) {
		t.Errorf("bad range: %v", err)
	}
}

func TestDetectFindsSpike(t *testing.T) {
	const season = 12
	v := make([]float64, 288)
	for i := range v {
		v[i] = 100 + 10*math.Sin(2*math.Pi*float64(i)/season)
	}
	v[200] = 400
	v[10] = math.NaN()
	got := detect("api", "Requests", v, window{0, 288 * 300_000, 300_000}, season)
	if len(got) == 0 || got[0].At != 200*300_000 || got[0].Value != 400 {
		t.Fatalf("anomalies %+v", got)
	}
	if detect("api", "Requests", v[:20], window{0, 20 * 300_000, 300_000}, season) != nil {
		t.Error("less than two seasons should yield nothing")
	}
}

func TestSendAndTargets(t *testing.T) {
	var gotSlack map[string]string
	var gotHook Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/slack/secret":
			_ = json.Unmarshal(b, &gotSlack)
		case "/hook":
			_ = json.Unmarshal(b, &gotHook)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	ts, err := ParseTargets([]string{"slack:" + srv.URL + "/slack/secret", "webhook:" + srv.URL + "/hook", "log", "webhook:" + srv.URL + "/bad"})
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{Targets: ts}
	rep := &Report{Range: RangeInfo{ID: "24h"}, Narrative: "all quiet"}
	sent, err := b.Send(context.Background(), rep)
	if len(sent) != 3 || err == nil || strings.Contains(err.Error(), "/bad") || strings.Contains(strings.Join(sent, ","), "secret") {
		t.Fatalf("sent=%v err=%v (URL paths must not leak)", sent, err)
	}
	if !strings.Contains(gotSlack["text"], "all quiet") || gotHook.Narrative != "all quiet" {
		t.Errorf("slack=%v hook=%+v", gotSlack, gotHook)
	}
	for _, bad := range []string{"slack:", "email:x@y", "webhook:ftp://x"} {
		if _, err := ParseTargets([]string{bad}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
