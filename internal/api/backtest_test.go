package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
	"github.com/himanshubaliyan7/zerowatch/internal/metadata"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

func doPost(mux *http.ServeMux, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.WithTenant(req.Context(), 1))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// A disk filling steadily (+0.1 %/min from 50 %) crosses 90 % after 400 min.
// A 1 h predictive rule must have fired about an hour before the observed one.
func TestBacktestPredictiveRule(t *testing.T) {
	st := storage.NewMemory(storage.MemoryConfig{})
	defer st.Close()
	now := time.Now()
	start := now.Add(-8 * time.Hour).Truncate(time.Minute)
	var ms []model.MetricDatum
	dims := []model.Dimension{{Name: "Host", Value: "h"}, {Name: "Mount", Value: "/"}}
	for ts := start; ts.Before(now); ts = ts.Add(time.Minute) {
		v := 50 + 0.1*ts.Sub(start).Minutes()
		ms = append(ms, model.MetricDatum{TenantID: 1, Namespace: "ZeroWatch/Host", MetricName: "DiskUsedPercent", Dimensions: dims, Timestamp: ts.UnixMilli(), Value: model.Float(v)})
	}
	if err := st.WriteMetrics(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	mux := (&Server{Storage: st, Query: &query.Engine{Fetcher: st}}).Routes()
	rule := alarms.Rule{Metric: &query.MetricStat{Namespace: "ZeroWatch/Host", MetricName: "DiskUsedPercent", Stat: "Maximum", Dimensions: map[string]string{"Host": "h", "Mount": "/"}},
		PeriodSec: 60, ForecastHorizonSec: 3600,
		Config: alarms.Config{Threshold: 90, Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 1, DatapointsToAlarm: 1}}
	rec := doPost(mux, "/api/v1/alarms/backtest", map[string]any{"rule": rule, "hours": 6})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var out backtestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Forecast) != 360 || out.Stride != 4 {
		t.Fatalf("forecast len %d stride %d", len(out.Forecast), out.Stride)
	}
	if out.Predicted.AlarmCount != 1 || out.Observed.AlarmCount != 1 || out.LeadSec == nil {
		t.Fatalf("predicted %d observed %d lead %v", out.Predicted.AlarmCount, out.Observed.AlarmCount, out.LeadSec)
	}
	// Ideal lead is 3600 s; the stride (4 min) and the trend estimate blur it a little.
	if *out.LeadSec < 2700 || *out.LeadSec > 3900 {
		t.Errorf("lead %d s, want about 3600", *out.LeadSec)
	}

	rule.ForecastHorizonSec = 0
	if rec := doPost(mux, "/api/v1/alarms/backtest", map[string]any{"rule": rule}); rec.Code != 400 {
		t.Errorf("a non-predictive rule should be refused, got %d", rec.Code)
	}
}

func TestSavedQueries(t *testing.T) {
	meta, err := metadata.Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	mux := (&Server{Meta: meta}).Routes()
	rec := doPost(mux, "/api/v1/logs/saved", map[string]string{"name": " checkout errors ", "query": "level=error service=checkout"})
	var saved metadata.SavedQuery
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &saved) != nil || saved.ID == "" || saved.Name != "checkout errors" {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if rec := doPost(mux, "/api/v1/logs/saved", map[string]string{"name": "  "}); rec.Code != 400 {
		t.Errorf("empty name: %d", rec.Code)
	}
	var list struct {
		Queries []metadata.SavedQuery `json:"queries"`
	}
	rec = doGet(mux, 1, "/api/v1/logs/saved")
	if json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Queries) != 1 {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec := doGet(mux, 2, "/api/v1/logs/saved"); !bytes.Contains(rec.Body.Bytes(), []byte(`"queries":[]`)) {
		t.Errorf("tenant 2 sees tenant 1's queries: %s", rec.Body)
	}
	del := func(id string) int {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/logs/saved/"+id, nil)
		req = req.WithContext(httpx.WithTenant(req.Context(), 1))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := del(saved.ID); c != 200 {
		t.Errorf("delete: %d", c)
	}
	if c := del(saved.ID); c != 404 {
		t.Errorf("second delete: %d", c)
	}
}
