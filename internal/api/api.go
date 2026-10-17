// Package api exposes the JSON API, the CloudWatch-compatible and OTLP
// ingest endpoints, live tail (SSE) and the embedded SPA.
package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
	"github.com/himanshubaliyan7/zerowatch/internal/anomaly"
	"github.com/himanshubaliyan7/zerowatch/internal/commitlog"
	"github.com/himanshubaliyan7/zerowatch/internal/forecast"
	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
	"github.com/himanshubaliyan7/zerowatch/internal/ingest"
	"github.com/himanshubaliyan7/zerowatch/internal/live"
	"github.com/himanshubaliyan7/zerowatch/internal/metadata"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
	"github.com/himanshubaliyan7/zerowatch/internal/traces"
)

// Server bundles API dependencies.
type Server struct {
	Storage  storage.Engine
	Meta     metadata.Store
	Query    *query.Engine
	Pipeline *ingest.Pipeline
	Hub      *live.Hub
	Alarms   *alarms.Engine
	Static   fs.FS
	Log      *slog.Logger
	Extra    func() map[string]any // supervisor / commit log stats
	// Traces enables POST /v1/traces and the /api/v1/traces routes when set.
	Traces traces.Backend
	// Forecast enables POST /api/v1/forecast when set.
	Forecast *forecast.Service
}

func init() {
	httpx.RegisterMapper(func(err error) *httpx.APIError {
		switch {
		case errors.Is(err, metadata.ErrNotFound):
			return httpx.NotFound("resource not found")
		case errors.Is(err, metadata.ErrConflict):
			return httpx.Conflict("version conflict; reload and retry")
		case errors.Is(err, query.ErrBadRequest), errors.Is(err, model.ErrValidation), errors.Is(err, storage.ErrBadStat):
			return httpx.BadRequest(err.Error())
		case errors.Is(err, commitlog.ErrBackpressure):
			return httpx.Unavailable("ingest backpressure, retry later")
		case errors.Is(err, storage.ErrCardinality):
			return &httpx.APIError{Status: http.StatusTooManyRequests, Code: "CARDINALITY_LIMIT", Message: err.Error()}
		}
		return nil
	})
}

// Routes builds the mux.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Meta.Ping(r.Context()); err != nil {
			httpx.WriteError(w, r, httpx.Unavailable("metadata store unavailable"))
			return
		}
		httpx.WriteJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /api/v1/query", s.query)
	mux.HandleFunc("GET /api/v1/series", s.series)
	mux.HandleFunc("GET /api/v1/logs/groups", s.logGroups)
	mux.HandleFunc("POST /api/v1/logs/search", s.logSearch)
	mux.HandleFunc("GET /api/v1/logs/tail", s.tail)
	mux.HandleFunc("GET /api/v1/logs/saved", s.listSavedQueries)
	mux.HandleFunc("POST /api/v1/logs/saved", s.saveQuery)
	mux.HandleFunc("DELETE /api/v1/logs/saved/{id}", s.deleteSavedQuery)
	mux.HandleFunc("GET /api/v1/dashboards", s.listDashboards)
	mux.HandleFunc("GET /api/v1/dashboards/{id}", s.getDashboard)
	mux.HandleFunc("POST /api/v1/dashboards", s.saveDashboard)
	mux.HandleFunc("PUT /api/v1/dashboards/{id}", s.saveDashboard)
	mux.HandleFunc("DELETE /api/v1/dashboards/{id}", s.deleteDashboard)
	mux.HandleFunc("GET /api/v1/alarms", s.listAlarms)
	mux.HandleFunc("POST /api/v1/alarms", s.saveAlarm)
	mux.HandleFunc("PUT /api/v1/alarms/{id}", s.saveAlarm)
	mux.HandleFunc("DELETE /api/v1/alarms/{id}", s.deleteAlarm)
	mux.HandleFunc("GET /api/v1/alarms/{id}/history", s.alarmHistory)
	mux.HandleFunc("POST /api/v1/alarms/simulate", s.simulate)
	mux.HandleFunc("POST /api/v1/alarms/backtest", s.backtest)
	mux.HandleFunc("POST /api/v1/anomaly", s.anomaly)
	mux.HandleFunc("GET /api/v1/stats", s.stats)
	if s.Forecast != nil {
		mux.HandleFunc("POST /api/v1/forecast", s.forecast)
	}
	if s.Traces != nil {
		mux.HandleFunc("GET /api/v1/traces", s.searchTraces)
		mux.HandleFunc("GET /api/v1/traces/{traceId}", s.getTrace)
	}
	(&ingest.Handlers{Pipeline: s.Pipeline, Traces: s.Traces}).Register(mux, &ingest.CloudWatchHandler{Pipeline: s.Pipeline, Query: s.Query})
	if s.Static != nil {
		files := http.FileServerFS(s.Static)
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			p := strings.TrimPrefix(r.URL.Path, "/")
			if p == "" || p == "index.html" {
				serveIndex(w, s.Static)
				return
			}
			if _, err := fs.Stat(s.Static, p); err != nil { // SPA fallback
				serveIndex(w, s.Static)
				return
			}
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
		})
	}
	return mux
}

func serveIndex(w http.ResponseWriter, f fs.FS) {
	b, err := fs.ReadFile(f, "index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// IsPublic marks routes that bypass API-key auth.
func IsPublic(r *http.Request) bool {
	p := r.URL.Path
	return p == "/healthz" || p == "/readyz" || !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/v1/") &&
		!strings.HasPrefix(p, "/prometheus/") && p != "/cloudwatch" && !strings.HasPrefix(p, "/cloudwatch/")
}

const maxBody = 1 << 20

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	var req query.Request
	if err := httpx.DecodeJSON(w, r, &req, maxBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req.TenantID = httpx.Tenant(r.Context())
	res, err := s.Query.Execute(r.Context(), req)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"results": res})
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := storage.SeriesFilter{Namespace: q.Get("namespace"), MetricName: q.Get("metricName"), Limit: atoi(q.Get("limit"), 1000)}
	if f.Limit < 1 || f.Limit > 10000 {
		httpx.WriteError(w, r, httpx.BadRequest("limit must be in [1,10000]"))
		return
	}
	out, err := s.Storage.ListSeries(r.Context(), httpx.Tenant(r.Context()), f)
	writeList(w, r, "series", out, err)
}

func (s *Server) logGroups(w http.ResponseWriter, r *http.Request) {
	out, err := s.Storage.ListLogGroups(r.Context(), httpx.Tenant(r.Context()))
	writeList(w, r, "logGroups", out, err)
}

// writeList answers {key: list}, plus "partial" with the reason when the
// storage could only list part of the data (cluster shards down).
func writeList(w http.ResponseWriter, r *http.Request, key string, list any, err error) {
	body := map[string]any{key: list}
	var pe *storage.PartialError
	switch {
	case errors.As(err, &pe):
		body["partial"] = pe.Msg
	case err != nil:
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, body)
}

func (s *Server) logSearch(w http.ResponseWriter, r *http.Request) {
	var q storage.LogQuery
	if err := httpx.DecodeJSON(w, r, &q, maxBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if q.Limit <= 0 || q.Limit > 10000 {
		q.Limit = 1000
	}
	q.TenantID = httpx.Tenant(r.Context())
	res, err := s.Storage.SearchLogs(r.Context(), q)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, res)
}

// tail streams live events as Server-Sent Events.
func (s *Server) tail(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteError(w, r, httpx.BadRequest("streaming unsupported"))
		return
	}
	group, term := r.URL.Query().Get("logGroup"), strings.ToLower(r.URL.Query().Get("query"))
	filter := func(e *live.Event) bool {
		if e.Log == nil {
			return true // alarm / supervisor events always pass
		}
		return (group == "" || e.Log.LogGroup == group) && (term == "" || strings.Contains(strings.ToLower(e.Log.Message), term))
	}
	sub, ok := s.Hub.Subscribe(httpx.Tenant(r.Context()), filter)
	if !ok {
		httpx.WriteError(w, r, httpx.Unavailable("too many live subscribers"))
		return
	}
	defer sub.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			_, _ = w.Write([]byte("event: " + ev.Kind + "\ndata: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			fl.Flush()
		}
	}
}

// Saved Log Explorer searches: a name, the filter text and an optional group.
func (s *Server) listSavedQueries(w http.ResponseWriter, r *http.Request) {
	out, err := s.Meta.ListSavedQueries(r.Context(), httpx.Tenant(r.Context()))
	respond(w, r, map[string]any{"queries": out}, err)
}

func (s *Server) saveQuery(w http.ResponseWriter, r *http.Request) {
	var q metadata.SavedQuery
	if err := httpx.DecodeJSON(w, r, &q, maxBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	q.Name, q.Query = strings.TrimSpace(q.Name), strings.TrimSpace(q.Query)
	if q.Name == "" || len(q.Name) > 200 || len(q.Query) > 4000 || len(q.LogGroup) > 512 {
		httpx.WriteError(w, r, httpx.BadRequest("name is required (max 200 chars); query max 4000, logGroup max 512"))
		return
	}
	q.TenantID = httpx.Tenant(r.Context())
	respond(w, r, &q, s.Meta.SaveQuery(r.Context(), &q))
}

func (s *Server) deleteSavedQuery(w http.ResponseWriter, r *http.Request) {
	err := s.Meta.DeleteSavedQuery(r.Context(), httpx.Tenant(r.Context()), r.PathValue("id"))
	respond(w, r, map[string]bool{"deleted": err == nil}, err)
}

func (s *Server) listDashboards(w http.ResponseWriter, r *http.Request) {
	out, err := s.Meta.ListDashboards(r.Context(), httpx.Tenant(r.Context()))
	respond(w, r, map[string]any{"dashboards": out}, err)
}

func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request) {
	d, err := s.Meta.GetDashboard(r.Context(), httpx.Tenant(r.Context()), r.PathValue("id"))
	respond(w, r, d, err)
}

func (s *Server) saveDashboard(w http.ResponseWriter, r *http.Request) {
	var d metadata.Dashboard
	if err := httpx.DecodeJSON(w, r, &d, maxBody, false); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.TenantID, d.ID = httpx.Tenant(r.Context()), r.PathValue("id")
	if err := d.Validate(); err != nil {
		httpx.WriteError(w, r, httpx.BadRequest(err.Error()))
		return
	}
	respond(w, r, &d, s.Meta.SaveDashboard(r.Context(), &d))
}

func (s *Server) deleteDashboard(w http.ResponseWriter, r *http.Request) {
	err := s.Meta.DeleteDashboard(r.Context(), httpx.Tenant(r.Context()), r.PathValue("id"))
	respond(w, r, map[string]bool{"deleted": err == nil}, err)
}

func (s *Server) listAlarms(w http.ResponseWriter, r *http.Request) {
	rules, err := s.Meta.ListAlarmRules(r.Context(), httpx.Tenant(r.Context()))
	if err == nil && s.Alarms != nil {
		st := s.Alarms.States()
		for i := range rules {
			if v, ok := st[rules[i].ID]; ok {
				rules[i].State = v
			}
		}
	}
	respond(w, r, map[string]any{"alarms": rules}, err)
}

func (s *Server) saveAlarm(w http.ResponseWriter, r *http.Request) {
	var rule alarms.Rule
	if err := httpx.DecodeJSON(w, r, &rule, maxBody, false); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	rule.TenantID, rule.ID = httpx.Tenant(r.Context()), r.PathValue("id")
	if err := rule.Validate(); err != nil {
		httpx.WriteError(w, r, httpx.BadRequest(err.Error()))
		return
	}
	respond(w, r, &rule, s.Meta.SaveAlarmRule(r.Context(), &rule))
}

func (s *Server) deleteAlarm(w http.ResponseWriter, r *http.Request) {
	err := s.Meta.DeleteAlarmRule(r.Context(), httpx.Tenant(r.Context()), r.PathValue("id"))
	respond(w, r, map[string]bool{"deleted": err == nil}, err)
}

func (s *Server) alarmHistory(w http.ResponseWriter, r *http.Request) {
	out, err := s.Meta.ListAlarmHistory(r.Context(), httpx.Tenant(r.Context()), r.PathValue("id"), atoi(r.URL.Query().Get("limit"), 100))
	respond(w, r, map[string]any{"history": out}, err)
}

func (s *Server) simulate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config alarms.Config `json:"config"`
		Values []*float64    `json:"values"`
	}
	if err := httpx.DecodeJSON(w, r, &body, maxBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if len(body.Values) > 10000 {
		httpx.WriteError(w, r, httpx.BadRequest("max 10000 values"))
		return
	}
	res, err := alarms.Simulate(body.Config, body.Values)
	if err != nil {
		err = httpx.BadRequest(err.Error())
	}
	respond(w, r, res, err)
}

// anomaly returns expected-range bands for a series. method "holtwinters"
// (default; online, the first season is warm-up = null) or "stl" (robust
// seasonal-trend decomposition; bands for every point, needs ≥ 2 seasons).
// k is the band width in sigmas (default 2 for Holt-Winters, 4 for robust STL).
func (s *Server) anomaly(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Values []float64 `json:"values"`
		Season int       `json:"season"`
		Method string    `json:"method"`
		K      float64   `json:"k"`
	}
	if err := httpx.DecodeJSON(w, r, &body, maxBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if body.Season < 2 {
		body.Season = 60
	}
	if body.K < 0 || body.K > 10 {
		httpx.WriteError(w, r, httpx.BadRequest("k must be in (0,10]"))
		return
	}
	var bands []anomaly.Band
	var err error
	switch body.Method {
	case "", "holtwinters":
		p := anomaly.DefaultParams(body.Season)
		if body.K > 0 {
			p.K = body.K
		}
		bands, err = anomaly.Detect(p, body.Values)
	case "stl":
		p := anomaly.DefaultSTLParams(body.Season)
		p.Robust = true
		if body.K == 0 {
			body.K = 4
		}
		bands, err = anomaly.DetectSTL(p, body.Values, body.K)
	default:
		err = fmt.Errorf("unknown method %q (holtwinters|stl)", body.Method)
	}
	if err != nil {
		err = httpx.BadRequest(err.Error())
	}
	respond(w, r, map[string]any{"bands": bands, "method": cmp.Or(body.Method, "holtwinters")}, err)
}

// forecast handles POST /api/v1/forecast (forecast.Request → forecast.Response).
func (s *Server) forecast(w http.ResponseWriter, r *http.Request) {
	var req forecast.Request
	if err := httpx.DecodeJSON(w, r, &req, maxBody, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := s.Forecast.Forecast(r.Context(), httpx.Tenant(r.Context()), req)
	respond(w, r, res, err)
}

// searchTraces handles GET /api/v1/traces (newest first).
func (s *Server) searchTraces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tq := traces.Query{Service: q.Get("service"), Name: q.Get("name"), Limit: atoi(q.Get("limit"), traces.DefaultSearchLimit)}
	if tq.Limit < 1 {
		httpx.WriteError(w, r, httpx.BadRequest("limit must be positive"))
		return
	}
	tq.Limit = min(tq.Limit, traces.MaxSearchLimit) // clamp rather than fail a UI that asks for more
	for _, f := range []struct {
		key string
		dst *float64
	}{{"minDurationMs", &tq.MinDurationMs}, {"start", &tq.Start}, {"end", &tq.End}} {
		if v := q.Get(f.key); v != "" {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || n < 0 {
				httpx.WriteError(w, r, httpx.BadRequest(f.key+" must be a non-negative number"))
				return
			}
			*f.dst = n
		}
	}
	switch q.Get("error") {
	case "", "false", "0":
	case "true", "1":
		tq.ErrorsOnly = true
	default:
		httpx.WriteError(w, r, httpx.BadRequest("error must be true or false"))
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"traces": s.Traces.Search(httpx.Tenant(r.Context()), tq)})
}

// getTrace handles GET /api/v1/traces/{traceId}.
func (s *Server) getTrace(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("traceId"))
	if !traces.ValidTraceID(id) {
		httpx.WriteError(w, r, httpx.BadRequest("traceId must be 32 hex characters"))
		return
	}
	t, ok := s.Traces.Get(httpx.Tenant(r.Context()), id)
	if !ok {
		httpx.WriteError(w, r, httpx.NotFound("trace not found (unknown or evicted)"))
		return
	}
	httpx.WriteJSON(w, 200, t)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"storage": s.Storage.Stats(), "ingest": s.Pipeline.Stats(), "live": s.Hub.Stats()}
	if s.Traces != nil {
		out["traces"] = s.Traces.Stats()
	}
	if s.Extra != nil {
		for k, v := range s.Extra() {
			out[k] = v
		}
	}
	httpx.WriteJSON(w, 200, out)
}

func respond(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, v)
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
