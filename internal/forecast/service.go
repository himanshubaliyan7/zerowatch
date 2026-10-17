package forecast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/query"
)

// Limits on one request.
const (
	MaxHistory = 20000
	MaxHorizon = 5000
)

// Request is the body of POST /api/v1/forecast (web/src/lib/api.ts ForecastRequest).
type Request struct {
	MetricStat *query.MetricStat `json:"metricStat,omitempty"`
	// Expression applies metric math to metricStat (referenced as m1), or
	// stands alone when metricStat is absent.
	Expression string     `json:"expression,omitempty"`
	Values     []*float64 `json:"values,omitempty"`
	Start      int64      `json:"start,omitempty"`
	End        int64      `json:"end,omitempty"`
	Period     int        `json:"period"`
	Horizon    int        `json:"horizon"`
	Season     int        `json:"season,omitempty"`
	Method     string     `json:"method,omitempty"`
	Interval   float64    `json:"interval,omitempty"`
	Threshold  *float64   `json:"threshold,omitempty"`
	Comparison string     `json:"comparison,omitempty"`
}

// Point is one future period.
type Point struct {
	T        int64   `json:"t"`
	Forecast float64 `json:"forecast"`
	Lower    float64 `json:"lower"`
	Upper    float64 `json:"upper"`
}

// HistoryPoint is one past period (V nil = no data).
type HistoryPoint struct {
	T int64    `json:"t"`
	V *float64 `json:"v"`
}

// Response is ForecastResult in web/src/lib/api.ts.
type Response struct {
	Method    string         `json:"method"`
	Fallback  string         `json:"fallback,omitempty"`
	History   []HistoryPoint `json:"history"`
	Points    []Point        `json:"points"`
	CrossesAt *int64         `json:"crossesAt"`
}

// Service answers forecast requests from stored metrics or raw values.
type Service struct {
	Query *query.Engine
	// ModelURL is the optional model service base URL (forecaster/); empty
	// means built-in methods only.
	ModelURL string
	Client   *http.Client
	Now      func() time.Time
}

func badRequest(format string, a ...any) error {
	return fmt.Errorf("%w: %s", query.ErrBadRequest, fmt.Sprintf(format, a...))
}

// DefaultSeason is one day of periods (one week when a day has < 2 periods).
func DefaultSeason(periodSec int) int {
	if s := 86400 / periodSec; s >= 2 {
		return s
	}
	return 7 * 86400 / periodSec
}

// Forecast validates req, loads history and predicts.
func (s *Service) Forecast(ctx context.Context, tenant uint32, req Request) (*Response, error) {
	if req.Period < 1 || req.Period > 86400 {
		return nil, badRequest("period must be in [1,86400]")
	}
	if req.Horizon < 1 || req.Horizon > MaxHorizon {
		return nil, badRequest("horizon must be in [1,%d]", MaxHorizon)
	}
	if req.Interval != 0 && (req.Interval <= 0 || req.Interval >= 1) {
		return nil, badRequest("interval must be in (0,1)")
	}
	if req.Season == 0 {
		req.Season = DefaultSeason(req.Period)
	}
	if req.Season < 2 {
		return nil, badRequest("season must be >= 2")
	}
	if req.Threshold != nil {
		if _, err := Breaches(req.Comparison, 0, 0); err != nil {
			return nil, badRequest("comparison: %v", err)
		}
	}
	switch req.Method {
	case "", Auto, HoltWinters, STL, Holt, Model:
	default:
		return nil, badRequest("unknown method %q (auto|holtwinters|stl|holt|model)", req.Method)
	}
	hist, err := s.history(ctx, tenant, &req)
	if err != nil {
		return nil, err
	}
	y := make([]float64, len(hist))
	for i, h := range hist {
		y[i] = math.NaN()
		if h.V != nil {
			y[i] = *h.V
		}
	}
	pred, method, fallback, err := s.predict(ctx, req.Method, y, req.Period, Options{Season: req.Season, Horizon: req.Horizon, Interval: req.Interval})
	if err != nil {
		return nil, badRequest("%v", err)
	}
	pMs := int64(req.Period) * 1000
	last := req.End - pMs
	if len(hist) > 0 {
		last = hist[len(hist)-1].T
	}
	resp := &Response{Method: method, Fallback: fallback, History: hist, Points: make([]Point, req.Horizon)}
	for i := range resp.Points {
		resp.Points[i] = Point{T: last + int64(i+1)*pMs, Forecast: pred.Forecast[i], Lower: pred.Lower[i], Upper: pred.Upper[i]}
	}
	if req.Threshold != nil {
		resp.CrossesAt = CrossesAt(resp.Points, req.Comparison, *req.Threshold)
	}
	return resp, nil
}

// CrossesAt returns the first point whose forecast breaches the threshold.
func CrossesAt(pts []Point, comparison string, threshold float64) *int64 {
	for _, p := range pts {
		if b, _ := Breaches(comparison, p.Forecast, threshold); b {
			t := p.T
			return &t
		}
	}
	return nil
}

// history returns one point per period, oldest first, on an aligned grid.
func (s *Service) history(ctx context.Context, tenant uint32, req *Request) ([]HistoryPoint, error) {
	pMs := int64(req.Period) * 1000
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if req.End == 0 {
		req.End = now().UnixMilli()
	}
	req.End = query.AlignTimestamp(req.End, pMs)
	if len(req.Values) > 0 {
		if len(req.Values) > MaxHistory {
			return nil, badRequest("at most %d values", MaxHistory)
		}
		out := make([]HistoryPoint, len(req.Values))
		for i, v := range req.Values {
			out[i] = HistoryPoint{T: req.End - int64(len(req.Values)-i)*pMs, V: v}
		}
		return out, nil
	}
	if req.MetricStat == nil && req.Expression == "" {
		return nil, badRequest("metricStat, expression or values is required")
	}
	if req.Start == 0 {
		req.Start = req.End - int64(min(2*req.Season, MaxHistory))*pMs
	}
	req.Start = query.AlignTimestamp(req.Start, pMs)
	n := int((req.End - req.Start) / pMs)
	if n < 1 || n > MaxHistory {
		return nil, badRequest("history must cover 1..%d periods (start/end/period)", MaxHistory)
	}
	if s.Query == nil {
		return nil, fmt.Errorf("forecast: no query engine")
	}
	f := false
	var qs []query.MetricDataQuery
	target := "m1"
	if req.MetricStat != nil {
		qs = append(qs, query.MetricDataQuery{ID: "m1", MetricStat: req.MetricStat})
	}
	if req.Expression != "" {
		if len(qs) > 0 {
			qs[0].ReturnData = &f
		}
		qs = append(qs, query.MetricDataQuery{ID: "e1", Expression: req.Expression})
		target = "e1"
	}
	res, err := s.Query.Execute(ctx, query.Request{TenantID: tenant, Queries: qs, StartMs: req.Start, EndMs: req.End, Period: req.Period})
	if err != nil {
		return nil, err
	}
	out := make([]HistoryPoint, n)
	for i := range out {
		out[i].T = req.Start + int64(i)*pMs
	}
	for _, r := range res {
		if r.ID != target {
			continue
		}
		for i, ts := range r.Timestamps {
			idx := int((ts - req.Start) / pMs)
			if idx >= 0 && idx < n && !math.IsNaN(r.Values[i]) && !math.IsInf(r.Values[i], 0) {
				v := r.Values[i]
				out[idx].V = &v
			}
		}
	}
	return out, nil
}

// predict runs the model service for auto/model when configured, falling
// back to the built-in methods (fallback then carries the reason).
func (s *Service) predict(ctx context.Context, method string, y []float64, periodSec int, o Options) (Prediction, string, string, error) {
	var why string
	if method == Model || (method == Auto || method == "") && s.ModelURL != "" {
		if s.ModelURL == "" {
			why = "no model service configured (--forecaster-url)"
		} else {
			p, name, err := s.callModel(ctx, y, periodSec, o)
			if err == nil {
				return p, Model + ":" + name, "", nil
			}
			why = "model service: " + err.Error()
		}
		method = Auto
	}
	p, used, fb, err := Predict(method, y, o)
	if why != "" {
		fb = strings.TrimPrefix(why+"; "+fb, "; ")
		fb = strings.TrimSuffix(fb, "; ")
	}
	return p, used, fb, err
}

// Predict implements alarms.Forecaster: horizon forecasts for per-period
// values (nil = missing) with the service's auto method.
func (s *Service) Predict(ctx context.Context, values []*float64, periodSec, horizon int) ([]float64, string, error) {
	y := make([]float64, len(values))
	for i, v := range values {
		y[i] = math.NaN()
		if v != nil {
			y[i] = *v
		}
	}
	p, used, _, err := s.predict(ctx, Auto, y, periodSec, Options{Season: DefaultSeason(periodSec), Horizon: horizon})
	return p.Forecast, used, err
}

type modelRequest struct {
	Values   []*float64 `json:"values"`
	Period   int        `json:"period"`
	Horizon  int        `json:"horizon"`
	Season   int        `json:"season"`
	Interval float64    `json:"interval"`
}

type modelResponse struct {
	Model    string    `json:"model"`
	Forecast []float64 `json:"forecast"`
	Lower    []float64 `json:"lower"`
	Upper    []float64 `json:"upper"`
}

// callModel POSTs to {ModelURL}/forecast (protocol in forecaster/README.md).
func (s *Service) callModel(ctx context.Context, y []float64, periodSec int, o Options) (Prediction, string, error) {
	if o.Interval == 0 {
		o.Interval = 0.8
	}
	vals := make([]*float64, len(y))
	for i := range y {
		if !math.IsNaN(y[i]) {
			vals[i] = &y[i]
		}
	}
	body, _ := json.Marshal(modelRequest{Values: vals, Period: periodSec, Horizon: o.Horizon, Season: o.Season, Interval: o.Interval})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.ModelURL, "/")+"/forecast", bytes.NewReader(body))
	if err != nil {
		return Prediction{}, "", err
	}
	hr.Header.Set("Content-Type", "application/json")
	c := s.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(hr)
	if err != nil {
		return Prediction{}, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return Prediction{}, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 200)])))
	}
	var mr modelResponse
	if err := json.Unmarshal(b, &mr); err != nil {
		return Prediction{}, "", fmt.Errorf("bad response: %w", err)
	}
	if len(mr.Forecast) != o.Horizon || len(mr.Lower) != o.Horizon || len(mr.Upper) != o.Horizon {
		return Prediction{}, "", fmt.Errorf("bad response: want %d points, got %d", o.Horizon, len(mr.Forecast))
	}
	for i := range mr.Forecast {
		for _, v := range []float64{mr.Forecast[i], mr.Lower[i], mr.Upper[i]} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return Prediction{}, "", fmt.Errorf("bad response: non-finite value")
			}
		}
	}
	return Prediction{Forecast: mr.Forecast, Lower: mr.Lower, Upper: mr.Upper}, mr.Model, nil
}
