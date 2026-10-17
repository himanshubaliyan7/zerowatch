// Package generator produces realistic synthetic RED (Rate, Errors, Duration)
// metrics and correlated structured logs so a fresh `--role=all` install has
// live data on every screen with zero external dependencies.
//
// It models N services × 3 routes with:
//   - a diurnal-ish sinusoidal traffic curve (so Holt-Winters has seasonality),
//   - log-normal latency (long tail, realistic p99/p50 ratio),
//   - periodic "incidents" where one service's error rate and latency spike,
//     which exercises alarms, anomaly bands and metric→log correlation.
//
// Everything flows through ingest.Pipeline exactly like real telemetry,
// except traces: every logged request's trace_id is a real trace written
// straight to the trace store (Config.Traces). Going around the OTLP path is
// deliberate: that path derives RED from SERVER spans, and the generator
// already emits RED, so ingesting its spans there would double count.
package generator

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/ingest"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/traces"
)

// Namespace for synthetic RED metrics (same as the auto-instrumented RED namespace).
const Namespace = "ZeroWatch/RED"

var serviceNames = []string{"checkout", "payments", "catalog", "auth", "search", "inventory", "shipping", "reviews", "cart", "gateway"}

var routes = []string{"GET /", "POST /api", "GET /health"}

// downstreamRoute is the route a service serves to its callers; it is one of
// routes so the downstream server spans line up with the synthetic RED series.
const downstreamRoute = "POST /api"

// callGraph is the fixed, acyclic synthetic topology: gateway-like services
// fan out to the others. Only edges between enabled services are used.
var callGraph = map[string][]string{
	"gateway":  {"checkout", "catalog", "search", "auth", "cart"},
	"checkout": {"payments", "inventory", "auth", "shipping", "cart"},
	"payments": {"auth"},
	"catalog":  {"inventory", "reviews"},
	"search":   {"catalog", "inventory"},
	"cart":     {"inventory", "catalog"},
	"shipping": {"inventory"},
	"reviews":  {"auth"},
}

// Config controls the generator.
type Config struct {
	Services int
	Tenant   uint32
	Interval time.Duration
	// MaxLatencySamples caps raw latency samples per (service, route, tick).
	MaxLatencySamples int
	// IncidentEvery triggers a 2-minute incident on a random service.
	IncidentEvery time.Duration
	Seed          uint64
	// Traces, when set, receives one synthetic trace per logged request.
	Traces traces.Backend
}

type svc struct {
	name       string
	baseRPS    float64
	baseLatMs  float64
	errRate    float64
	phase      float64
	incidentTo time.Time
}

// Generator emits telemetry.
type Generator struct {
	cfg      Config
	pipeline *ingest.Pipeline
	log      *slog.Logger
	rng      *rand.Rand
	services []*svc
	enabled  map[string]bool
	next     time.Time
}

// New builds a generator.
func New(cfg Config, p *ingest.Pipeline, log *slog.Logger) *Generator {
	if cfg.Services <= 0 {
		cfg.Services = 6
	}
	if cfg.Services > len(serviceNames) {
		cfg.Services = len(serviceNames)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.MaxLatencySamples <= 0 {
		cfg.MaxLatencySamples = 20
	}
	if cfg.IncidentEvery <= 0 {
		cfg.IncidentEvery = 15 * time.Minute
	}
	if cfg.Tenant == 0 {
		cfg.Tenant = 1
	}
	if cfg.Seed == 0 {
		cfg.Seed = uint64(time.Now().UnixNano())
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
	g := &Generator{cfg: cfg, pipeline: p, log: log, rng: rng, enabled: map[string]bool{}}
	for i := 0; i < cfg.Services; i++ {
		g.enabled[serviceNames[i]] = true
		g.services = append(g.services, &svc{
			name:      serviceNames[i],
			baseRPS:   8 + rng.Float64()*30,
			baseLatMs: 15 + rng.Float64()*80,
			errRate:   0.002 + rng.Float64()*0.01,
			phase:     rng.Float64() * 2 * math.Pi,
		})
	}
	return g
}

// Services returns the simulated service names.
func (g *Generator) Services() []string {
	out := make([]string, len(g.services))
	for i, s := range g.services {
		out[i] = s.name
	}
	return out
}

// Run emits until ctx is cancelled. It first backfills the last 30 minutes
// (at 10s resolution) so charts and anomaly models are not empty on boot.
func (g *Generator) Run(ctx context.Context) {
	g.log.Info("synthetic generator started", "services", g.cfg.Services, "interval", g.cfg.Interval)
	g.next = time.Now().Add(g.cfg.IncidentEvery / 3)
	g.backfill(ctx, 30*time.Minute, 10*time.Second)
	t := time.NewTicker(g.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			g.maybeIncident(now)
			ms, ls, spans := g.tick(now, g.cfg.Interval, true, g.cfg.Traces != nil)
			g.write(ctx, ms, ls)
			if len(spans) > 0 {
				g.cfg.Traces.Add(g.cfg.Tenant, spans)
			}
		}
	}
}

func (g *Generator) backfill(ctx context.Context, span, step time.Duration) {
	now := time.Now()
	for ts := now.Add(-span); ts.Before(now); ts = ts.Add(step) {
		if ctx.Err() != nil {
			return
		}
		ms, _, _ := g.tick(ts, step, false, false)
		g.write(ctx, ms, nil)
	}
}

func (g *Generator) write(ctx context.Context, ms []model.MetricDatum, ls []model.LogEvent) {
	if len(ms) > 0 {
		if _, err := g.pipeline.WriteMetrics(ctx, g.cfg.Tenant, ms); err != nil && ctx.Err() == nil {
			g.log.Warn("generator metrics write failed", "err", err)
		}
	}
	for len(ls) > 0 && ctx.Err() == nil {
		n := min(len(ls), 1000)
		if _, err := g.pipeline.WriteLogs(ctx, g.cfg.Tenant, ls[:n]); err != nil && ctx.Err() == nil {
			g.log.Warn("generator logs write failed", "err", err)
		}
		ls = ls[n:]
	}
}

// Tick produces one interval of telemetry ending at now (exported for tests).
func (g *Generator) Tick(now time.Time, step time.Duration) ([]model.MetricDatum, []model.LogEvent) {
	g.maybeIncident(now)
	ms, ls, _ := g.tick(now, step, true, false)
	return ms, ls
}

// TickWithTraces is Tick plus the spans of every logged request's trace.
func (g *Generator) TickWithTraces(now time.Time, step time.Duration) ([]model.MetricDatum, []model.LogEvent, []traces.Span) {
	g.maybeIncident(now)
	return g.tick(now, step, true, true)
}

func (g *Generator) maybeIncident(now time.Time) {
	if !g.next.IsZero() && now.After(g.next) {
		s := g.services[g.rng.IntN(len(g.services))]
		s.incidentTo = now.Add(2 * time.Minute)
		g.next = now.Add(g.cfg.IncidentEvery)
		g.log.Info("synthetic incident started", "service", s.name)
	}
}

func (g *Generator) tick(now time.Time, step time.Duration, withLogs, withTraces bool) ([]model.MetricDatum, []model.LogEvent, []traces.Span) {
	var ms []model.MetricDatum
	var ls []model.LogEvent
	var spans []traces.Span
	secs := step.Seconds()
	// 1-hour seasonality: demo-friendly yet a real seasonal signal.
	season := 2 * math.Pi * float64(now.Unix()%3600) / 3600
	base := now.UnixMilli()
	for _, s := range g.services {
		incident := now.Before(s.incidentTo)
		for ri, route := range routes {
			weight := []float64{1, 0.6, 0.25}[ri]
			rps := s.baseRPS * weight * (1 + 0.35*math.Sin(season+s.phase))
			lambda := rps * secs
			reqs := g.poisson(lambda)
			errRate := s.errRate
			latMul := 1.0
			if incident && ri == 1 {
				errRate, latMul = 0.12+g.rng.Float64()*0.1, 4+g.rng.Float64()*3
			}
			errs := 0
			for i := 0; i < reqs; i++ {
				if g.rng.Float64() < errRate {
					errs++
				}
			}
			dims := []model.Dimension{{Name: "Route", Value: route}, {Name: "Service", Value: s.name}}
			ms = append(ms,
				model.MetricDatum{Namespace: Namespace, MetricName: "Requests", Dimensions: dims, Timestamp: base, Value: model.Float(float64(reqs)), Unit: "Count"},
				model.MetricDatum{Namespace: Namespace, MetricName: "Errors", Dimensions: dims, Timestamp: base, Value: model.Float(float64(errs)), Unit: "Count"},
			)
			// Latency: raw samples (so percentiles are exact), spread across the interval.
			n := min(reqs, g.cfg.MaxLatencySamples)
			offs := make([]int64, n)
			for i := range offs {
				offs[i] = -int64(g.rng.Float64() * float64(step.Milliseconds()))
			}
			sort.Slice(offs, func(a, b int) bool { return offs[a] < offs[b] })
			for i := 0; i < n; i++ {
				lat := g.latency(s.baseLatMs*weightLatency(ri)) * latMul
				ms = append(ms, model.MetricDatum{Namespace: Namespace, MetricName: "Latency", Dimensions: dims, Timestamp: base + offs[i], Value: model.Float(round2(lat)), Unit: "Milliseconds"})
				if withLogs && (i%4 == 0 || (i < errs)) {
					req := g.newRequest(s.name, route, base+offs[i], lat, i < errs)
					if withTraces {
						spans = g.buildTrace(spans, req, ri)
					}
					ls = append(ls, g.logLine(req))
				}
			}
		}
		if withLogs && g.rng.IntN(10) == 0 {
			ls = append(ls, g.emfLine(s.name, base))
		}
	}
	return ms, ls, spans
}

func weightLatency(ri int) float64 { return []float64{1, 1.8, 0.1}[ri] }

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// latency samples a log-normal distribution with the given median.
func (g *Generator) latency(medianMs float64) float64 {
	return medianMs * math.Exp(0.55*g.rng.NormFloat64())
}

// poisson uses Knuth for small lambda and a normal approximation otherwise.
func (g *Generator) poisson(lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 {
		return max(0, int(math.Round(lambda+math.Sqrt(lambda)*g.rng.NormFloat64())))
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= g.rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

var errMessages = []string{"upstream timeout after 3000ms", "connection reset by peer", "database deadlock detected", "invalid token signature", "rate limit exceeded on dependency"}

// request is one logged request; its log line and its trace share every field.
type request struct {
	service, route  string
	endMs           int64
	latMs           float64
	status          int
	errMsg          string // non-empty for failed requests
	traceID, spanID string // spanID is the root SERVER span
}

func (g *Generator) newRequest(service, route string, ts int64, lat float64, isErr bool) request {
	req := request{service: service, route: route, endMs: ts, latMs: lat, status: 200,
		traceID: fmt.Sprintf("%016x%016x", g.rng.Uint64(), g.rng.Uint64()), spanID: g.spanID()}
	if isErr {
		req.status = 500 + g.rng.IntN(4)
		req.errMsg = errMessages[g.rng.IntN(len(errMessages))]
	}
	return req
}

func (g *Generator) spanID() string {
	for {
		if v := g.rng.Uint64(); v != 0 { // all-zero ids are invalid in OTLP
			return fmt.Sprintf("%016x", v)
		}
	}
}

func (g *Generator) logLine(req request) model.LogEvent {
	level, msg := "info", fmt.Sprintf("%s completed", req.route)
	if req.errMsg != "" {
		level, msg = "error", req.errMsg
	} else if req.latMs > 500 {
		level, msg = "warn", fmt.Sprintf("slow request %s", req.route)
	}
	line := fmt.Sprintf(`{"ts":%d,"level":%q,"service":%q,"route":%q,"status":%d,"duration_ms":%.2f,"trace_id":%q,"span_id":%q,"msg":%q}`,
		req.endMs, level, req.service, req.route, req.status, req.latMs, req.traceID, req.spanID, msg)
	return model.LogEvent{
		LogGroup: "/zerowatch/synthetic/" + req.service, LogStream: fmt.Sprintf("%s-pod-%d", req.service, g.rng.IntN(3)),
		Timestamp: req.endMs, Message: line, Level: level,
		Attributes: map[string]string{"service": req.service, "route": req.route, "status": fmt.Sprint(req.status),
			"trace_id": req.traceID, "span_id": req.spanID},
	}
}

// buildTrace appends the request's trace: a root SERVER span ending at the
// log timestamp, then CLIENT→SERVER pairs down the call graph (health checks
// call nothing). A failed request fails along one path from a leaf up.
func (g *Generator) buildTrace(out []traces.Span, req request, ri int) []traces.Span {
	root := traces.Span{
		TraceID: req.traceID, SpanID: req.spanID, Service: req.service, Name: req.route, Kind: traces.KindServer,
		Start: float64(req.endMs) - req.latMs, DurationMs: req.latMs, Status: traces.StatusOK,
		Attributes: map[string]string{traces.SyntheticAttr: "true", "http.route": req.route, "http.status_code": fmt.Sprint(req.status)},
	}
	if req.errMsg != "" {
		root.Status, root.StatusMessage = traces.StatusError, req.errMsg
	}
	out = append(out, root)
	if ri == 2 {
		return out
	}
	return g.calls(out, root, 0, req.errMsg)
}

// calls appends CLIENT→SERVER span pairs from parent to 1–3 callees (0–1 at
// the second level). When errMsg is set, one callee fails with it.
func (g *Generator) calls(out []traces.Span, parent traces.Span, depth int, errMsg string) []traces.Span {
	if depth >= 2 {
		return out
	}
	var callees []string
	for _, c := range callGraph[parent.Service] {
		if g.enabled[c] {
			callees = append(callees, c)
		}
	}
	if len(callees) == 0 {
		return out
	}
	g.rng.Shuffle(len(callees), func(a, b int) { callees[a], callees[b] = callees[b], callees[a] })
	n := 1 + g.rng.IntN(min(3, len(callees)))
	if depth > 0 { // keep second-level fan-out small
		n = g.rng.IntN(2)
	}
	if n == 0 {
		return out
	}
	callees = callees[:n]
	failing := -1
	if errMsg != "" {
		failing = g.rng.IntN(n)
	}
	// Calls run one after another inside the middle 80 % of the parent.
	slot := parent.DurationMs * 0.8 / float64(n)
	cursor := parent.Start + parent.DurationMs*0.1
	for j, callee := range callees {
		dur := slot * (0.6 + 0.35*g.rng.Float64())
		net := math.Min(0.5, dur*0.05)
		client := traces.Span{
			TraceID: parent.TraceID, SpanID: g.spanID(), ParentSpanID: parent.SpanID, Service: parent.Service,
			Name: downstreamRoute, Kind: traces.KindClient, Start: cursor, DurationMs: dur, Status: traces.StatusOK,
			Attributes: map[string]string{traces.SyntheticAttr: "true", "peer.service": callee},
		}
		server := traces.Span{
			TraceID: parent.TraceID, SpanID: g.spanID(), ParentSpanID: client.SpanID, Service: callee,
			Name: downstreamRoute, Kind: traces.KindServer, Start: cursor + net, DurationMs: dur - 2*net, Status: traces.StatusOK,
			Attributes: map[string]string{traces.SyntheticAttr: "true", "http.route": downstreamRoute},
		}
		childErr := ""
		if j == failing {
			childErr = errMsg
			client.Status, client.StatusMessage = traces.StatusError, errMsg
			server.Status, server.StatusMessage = traces.StatusError, errMsg
		}
		out = append(out, client, server)
		out = g.calls(out, server, depth+1, childErr)
		cursor += slot
	}
	return out
}

// emfLine emits an Embedded Metric Format line, demonstrating log→metric extraction.
func (g *Generator) emfLine(service string, ts int64) model.LogEvent {
	depth := g.rng.IntN(50)
	line := fmt.Sprintf(`{"_aws":{"Timestamp":%d,"CloudWatchMetrics":[{"Namespace":"ZeroWatch/App","Dimensions":[["Service"]],"Metrics":[{"Name":"QueueDepth","Unit":"Count"}]}]},"Service":%q,"QueueDepth":%d}`,
		ts, service, depth)
	return model.LogEvent{LogGroup: "/zerowatch/synthetic/" + service, LogStream: service + "-emf", Timestamp: ts, Message: line, Level: "info"}
}
