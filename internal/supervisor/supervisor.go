// Package supervisor implements RedBrain, the autonomous loop:
// Sense -> Decide -> Act -> Verify -> Synthesize -> SelfHeal, every interval.
// The "decision core" is deterministic rules (no ML model): processes with
// listening ports become probe targets, known workload kinds get boards.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zerowatch/zerowatch/ebpf/probe"
	"github.com/zerowatch/zerowatch/internal/alarms"
	"github.com/zerowatch/zerowatch/internal/hostmetrics"
	"github.com/zerowatch/zerowatch/internal/ingest"
	"github.com/zerowatch/zerowatch/internal/live"
	"github.com/zerowatch/zerowatch/internal/metadata"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/procfs"
	"github.com/zerowatch/zerowatch/internal/query"
	"github.com/zerowatch/zerowatch/internal/storage"
)

// Config tunes the loop.
type Config struct {
	Interval     time.Duration
	LogDiscovery bool
	LogRoots     []string
	Tenant       uint32
	CPUBudgetPct float64 // supervisor's own CPU budget (default 1.0)
	RSSBudgetMB  float64 // process RSS budget (default 100, reported only in role=all)
	// HostMetrics samples this node's CPU, memory, disks and network every
	// cycle (namespace ZeroWatch/Host) and synthesizes its board and
	// predictive capacity alarms.
	HostMetrics bool
	HostFSRoot  string // prefix for statfs when the host's filesystems are mounted elsewhere (e.g. /host)
	HostName    string // the Host dimension; default os.Hostname()
}

// Ingester is what the loop writes to.
type Ingester interface {
	LogWriter
	WriteMetrics(ctx context.Context, tenant uint32, ms []model.MetricDatum) (ingest.Result, error)
}

// Watchdog reports resource self-monitoring.
type Watchdog struct {
	SupervisorCPUPct float64 `json:"supervisorCpuPct"`
	ProcessCPUPct    float64 `json:"processCpuPct"`
	RSSMB            float64 `json:"rssMb"`
	SampleRate       float64 `json:"sampleRate"`
	Throttled        bool    `json:"throttled"`
}

// State is the API-visible snapshot.
type State struct {
	Cycle       uint64             `json:"cycle"`
	LastRun     time.Time          `json:"lastRun"`
	Host        procfs.Host        `json:"host"`
	Services    []procfs.Process   `json:"services"`
	Containers  []procfs.Container `json:"containers,omitempty"`
	Targets     []probe.Target     `json:"targets"`
	Topology    Topology           `json:"topology"`
	Probe       probe.Status       `json:"probe"`
	LogSources  []LogSource        `json:"logSources"`
	Synthesized []string           `json:"synthesized"`
	Watchdog    Watchdog           `json:"watchdog"`
	Decisions   []string           `json:"decisions"`
}

// Supervisor is the RedBrain loop.
type Supervisor struct {
	cfg    Config
	probe  probe.Probe
	ing    Ingester
	meta   metadata.Store
	store  storage.Engine
	hub    *live.Hub
	log    *slog.Logger
	tailer *Tailer
	hostm  *hostmetrics.Collector // nil when HostMetrics is off
	// hostName is this node's Host dimension.
	hostName string

	mu        sync.Mutex
	state     State
	lastTicks uint64
	lastHost  procfs.CPUTimes
	calm      int
	created   map[string]bool
}

// New wires a supervisor.
func New(cfg Config, p probe.Probe, ing Ingester, meta metadata.Store, store storage.Engine, hub *live.Hub, log *slog.Logger) *Supervisor {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.CPUBudgetPct <= 0 {
		cfg.CPUBudgetPct = 1
	}
	if cfg.RSSBudgetMB <= 0 {
		cfg.RSSBudgetMB = 100
	}
	if cfg.Tenant == 0 {
		cfg.Tenant = 1
	}
	host, _ := os.Hostname()
	if cfg.HostName == "" {
		cfg.HostName = host
	}
	s := &Supervisor{cfg: cfg, probe: p, ing: ing, meta: meta, store: store, hub: hub, log: log, hostName: cfg.HostName,
		tailer: NewTailer(ing, cfg.Tenant, host), created: map[string]bool{},
		state: State{Watchdog: Watchdog{SampleRate: 1}, Topology: Topology{Nodes: []TopoNode{}, Edges: []TopoEdge{}}}}
	if cfg.HostMetrics {
		s.hostm = &hostmetrics.Collector{Host: cfg.HostName, FSRoot: cfg.HostFSRoot}
	}
	return s
}

// State returns a snapshot.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	st.Probe = s.probe.Status()
	st.LogSources = s.tailer.Sources()
	return st
}

func (s *Supervisor) decide(msg string, args ...any) {
	m := time.Now().Format("15:04:05 ") + fmt.Sprintf(msg, args...)
	s.state.Decisions = append(s.state.Decisions, m)
	if len(s.state.Decisions) > 50 {
		s.state.Decisions = s.state.Decisions[len(s.state.Decisions)-50:]
	}
}

// Run blocks until ctx is done.
func (s *Supervisor) Run(ctx context.Context) {
	go func() {
		_ = s.probe.Run(ctx, func(ctx context.Context, ms []model.MetricDatum) {
			_, _ = s.ing.WriteMetrics(ctx, s.cfg.Tenant, ms)
		})
	}()
	s.Cycle(ctx)
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Cycle(ctx)
		}
	}
}

// Cycle runs one pass of the loop.
func (s *Supervisor) Cycle(ctx context.Context) {
	start := time.Now()
	s.mu.Lock()
	s.state.Cycle++
	cycle := s.state.Cycle
	throttled := s.state.Watchdog.Throttled
	s.mu.Unlock()

	// 1. Sense.
	host := procfs.DetectHost(procfs.DefaultRoot, "/")
	procs, _ := procfs.ReadProcesses(procfs.DefaultRoot, true, 4000)
	socks, _ := procfs.ReadSockets(procfs.DefaultRoot)
	procfs.AttachPorts(procs, socks)
	topo := BuildTopology(procs, socks) // needs Inodes, dropped below
	var services []procfs.Process
	for _, p := range procs {
		if len(p.Ports) > 0 || p.Kind != "" {
			p.Inodes = nil
			services = append(services, p)
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].PID < services[j].PID })
	var containers []procfs.Container
	if host.DockerSocket != "" && (!throttled || cycle%6 == 0) {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		containers, _ = procfs.ListDockerContainers(cctx, host.DockerSocket)
		cancel()
	}
	if s.cfg.LogDiscovery && (cycle == 1 || cycle%6 == 0) && (!throttled || cycle%30 == 0) {
		s.tailer.Track(DiscoverLogFiles(s.cfg.LogRoots, 3, 50), start)
	}

	if s.hostm != nil {
		if ms, _ := s.hostm.Collect(start, s.cfg.Tenant); len(ms) > 0 {
			if _, err := s.ing.WriteMetrics(ctx, s.cfg.Tenant, ms); err != nil && cycle == 1 {
				s.log.Warn("host metrics not written", "err", err)
			}
		}
	}

	// 2. Decide: every listening process becomes a probe target.
	var targets []probe.Target
	for _, p := range services {
		name := p.Kind
		if name == "" {
			name = p.Comm
		}
		for _, port := range p.Ports {
			targets = append(targets, probe.Target{PID: p.PID, Port: port, Service: name})
		}
	}
	// 3. Act.
	s.probe.SetTargets(targets)
	shipped := 0
	if s.cfg.LogDiscovery {
		shipped = s.tailer.Poll(ctx, start)
	}
	// 4. Verify.
	ps := s.probe.Status()

	s.mu.Lock()
	if len(targets) != len(s.state.Targets) {
		s.decide("probe %s now watches %d port(s)", ps.Mode, len(targets))
	}
	if ps.Error != "" && cycle == 2 {
		s.decide("probe degraded: %s", ps.Error)
	}
	if len(topo.Edges) != len(s.state.Topology.Edges) {
		s.decide("topology: %d node(s), %d edge(s)", len(topo.Nodes), len(topo.Edges))
	}
	s.state.LastRun, s.state.Host, s.state.Services, s.state.Containers, s.state.Targets = start, host, services, containers, targets
	s.state.Topology = topo
	s.mu.Unlock()

	// 5. Synthesize dashboards and self-calibrated alarms.
	if !throttled || cycle%6 == 0 {
		s.synthesize(ctx, services)
	}
	// 6. SelfHeal.
	s.selfHeal(time.Since(start))
	if s.hub != nil {
		s.hub.Publish(live.Event{Kind: live.KindSupervisor, TenantID: s.cfg.Tenant, Time: start.UnixMilli(),
			Data: map[string]any{"cycle": cycle, "targets": len(targets), "logLinesShipped": shipped, "probe": ps}})
	}
}

func (s *Supervisor) selfHeal(spent time.Duration) {
	ticks, rss, err := procfs.SelfUsage(procfs.DefaultRoot)
	hostCPU, _ := procfs.ReadCPU(procfs.DefaultRoot)
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &s.state.Watchdog
	w.SupervisorCPUPct = 100 * spent.Seconds() / s.cfg.Interval.Seconds() / float64(runtime.NumCPU())
	if err == nil {
		if s.lastTicks > 0 && hostCPU.Total > s.lastHost.Total {
			w.ProcessCPUPct = 100 * float64(ticks-s.lastTicks) / float64(hostCPU.Total-s.lastHost.Total)
		}
		s.lastTicks, s.lastHost = ticks, hostCPU
		w.RSSMB = float64(rss) / (1 << 20)
	}
	// Throttle on 1.5x budget, recover after 3 calm cycles under 0.5x.
	if w.SupervisorCPUPct > 1.5*s.cfg.CPUBudgetPct {
		s.calm = 0
		if w.SampleRate > 0.1 {
			w.SampleRate = max(0.1, w.SampleRate/2)
			s.probe.SetSampleRate(w.SampleRate)
			s.decide("watchdog: supervisor CPU %.2f%% over budget, probe sample rate -> %.2f", w.SupervisorCPUPct, w.SampleRate)
		}
		w.Throttled = true
	} else if w.SupervisorCPUPct < 0.5*s.cfg.CPUBudgetPct {
		if s.calm++; s.calm >= 3 && w.Throttled {
			w.SampleRate = min(1, w.SampleRate*2)
			s.probe.SetSampleRate(w.SampleRate)
			w.Throttled = w.SampleRate < 1
			s.decide("watchdog: recovered, sample rate -> %.2f", w.SampleRate)
		}
	}
}

func jsonRaw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func ts(q ...query.MetricDataQuery) json.RawMessage {
	return jsonRaw(map[string]any{"queries": q, "period": 60})
}

func mq(id, label, ns, name, stat string, dims map[string]string) query.MetricDataQuery {
	return query.MetricDataQuery{ID: id, Label: label, MetricStat: &query.MetricStat{Namespace: ns, MetricName: name, Stat: stat, Dimensions: dims}}
}

// Storage matches a MetricStat's dimensions exactly (CloudWatch semantics:
// same key set, "*" = any value), so every query must name all of a series'
// dimensions. RED series carry {Route, Service}; Net series carry {Port, Service}.
func redDims(service string) map[string]string {
	if service == "" {
		service = "*"
	}
	return map[string]string{"Route": "*", "Service": service}
}

func netDims(port string) map[string]string {
	if port == "" {
		port = "*"
	}
	return map[string]string{"Port": port, "Service": "*"}
}

// boardSchema marks synthesized boards whose queries name every dimension.
// Older autogen boards matched no series at all; see repairable.
const boardSchema = 2

// synthesize creates boards/alarms once per name (idempotent: existing
// titles are never overwritten, so user edits are preserved).
func (s *Supervisor) synthesize(ctx context.Context, services []procfs.Process) {
	tenant := s.cfg.Tenant
	existing := map[string]bool{}
	stale := map[string]*metadata.Dashboard{} // autogen boards from before boardSchema, never edited
	if list, err := s.meta.ListDashboards(ctx, tenant); err == nil {
		for _, d := range list {
			existing[d.Title] = true
			if old := s.repairable(ctx, tenant, d.Title, d.ID); old != nil {
				stale[d.Title] = old
				delete(existing, d.Title)
			}
		}
	}
	red, _ := s.store.ListSeries(ctx, tenant, storage.SeriesFilter{Namespace: probe.NamespaceRED, MetricName: "Requests", Limit: 5000})
	svcSet := map[string]bool{}
	for _, m := range red {
		if v := m.Dimensions["Service"]; v != "" {
			svcSet[v] = true
		}
	}
	svcs := make([]string, 0, len(svcSet))
	for k := range svcSet {
		svcs = append(svcs, k)
	}
	sort.Strings(svcs)

	if len(svcs) > 0 && !existing["Auto-RED · Services"] {
		var reqQ, errQ, latQ []query.MetricDataQuery
		for i, sv := range svcs {
			if i >= 10 {
				break
			}
			d := redDims(sv)
			reqQ = append(reqQ, mq(fmt.Sprintf("r%d", i), sv, probe.NamespaceRED, "Requests", "Sum", d))
			errQ = append(errQ, mq(fmt.Sprintf("e%d", i), sv, probe.NamespaceRED, "Errors", "Sum", d))
			latQ = append(latQ, mq(fmt.Sprintf("l%d", i), sv, probe.NamespaceRED, "Latency", "p99", d))
		}
		all := redDims("")
		d := &metadata.Dashboard{TenantID: tenant, Title: "Auto-RED · Services", Description: "Synthesized by RedBrain from discovered RED series",
			Layout: jsonRaw(map[string]any{"autogen": true, "schema": boardSchema, "range": "1h", "refresh": 10}),
			Widgets: []metadata.Widget{
				{Type: "stat", Title: "Total requests / min", Query: ts(mq("t", "requests", probe.NamespaceRED, "Requests", "Sum", all)), X: 0, Y: 0, W: 4, H: 2},
				{Type: "stat", Title: "Total errors / min", Query: ts(mq("t", "errors", probe.NamespaceRED, "Errors", "Sum", all)), X: 4, Y: 0, W: 4, H: 2},
				{Type: "stat", Title: "p99 latency (ms)", Query: ts(mq("t", "p99", probe.NamespaceRED, "Latency", "p99", all)), X: 8, Y: 0, W: 4, H: 2},
				{Type: "timeseries", Title: "Rate · requests per service", Query: ts(reqQ...), X: 0, Y: 2, W: 6, H: 4},
				{Type: "timeseries", Title: "Errors per service", Query: ts(errQ...), X: 6, Y: 2, W: 6, H: 4},
				{Type: "timeseries", Title: "Duration · p99 per service", Query: ts(latQ...), X: 0, Y: 6, W: 12, H: 4},
				{Type: "logs", Title: "Error logs", Query: jsonRaw(map[string]any{"level": "ERROR"}), X: 0, Y: 10, W: 12, H: 4},
			}}
		s.saveBoard(ctx, d, stale[d.Title])
	}
	// Host network board for discovered listening ports.
	if len(services) > 0 && !existing["Auto · Host network"] {
		all := netDims("")
		d := &metadata.Dashboard{TenantID: tenant, Title: "Auto · Host network", Description: "Per-port connection proxies from the probe",
			Layout: jsonRaw(map[string]any{"autogen": true, "schema": boardSchema, "range": "1h"}),
			Widgets: []metadata.Widget{
				{Type: "timeseries", Title: "Active connections", Query: ts(mq("a", "active", probe.NamespaceNet, "ActiveConnections", "Average", all)), X: 0, Y: 0, W: 6, H: 4},
				{Type: "timeseries", Title: "Connections opened", Query: ts(mq("o", "opened", probe.NamespaceNet, "ConnectionsOpened", "Sum", all)), X: 6, Y: 0, W: 6, H: 4},
				{Type: "timeseries", Title: "Socket backlog (bytes)", Query: ts(mq("b", "backlog", probe.NamespaceNet, "SocketBacklog", "Maximum", all)), X: 0, Y: 4, W: 12, H: 3},
			}}
		s.saveBoard(ctx, d, stale[d.Title])
	}
	// Per-kind boards (redis, postgres, nginx, ...).
	kinds := map[string][]procfs.Process{}
	for _, p := range services {
		if p.Kind != "" && len(p.Ports) > 0 {
			kinds[p.Kind] = append(kinds[p.Kind], p)
		}
	}
	for kind, ps := range kinds {
		title := "Auto · " + kind
		if existing[title] {
			continue
		}
		port := fmt.Sprint(ps[0].Ports[0])
		dims := netDims(port)
		s.saveBoard(ctx, &metadata.Dashboard{TenantID: tenant, Title: title, Description: fmt.Sprintf("Detected %s (pid %d, port %s)", kind, ps[0].PID, port),
			Layout: jsonRaw(map[string]any{"autogen": true, "schema": boardSchema}),
			Widgets: []metadata.Widget{
				{Type: "timeseries", Title: kind + " active connections", Query: ts(mq("a", "active", probe.NamespaceNet, "ActiveConnections", "Average", dims)), X: 0, Y: 0, W: 6, H: 4},
				{Type: "timeseries", Title: kind + " new connections", Query: ts(mq("o", "opened", probe.NamespaceNet, "ConnectionsOpened", "Sum", dims)), X: 6, Y: 0, W: 6, H: 4},
			}}, stale[title])
	}
	// Self-calibrating alarms: threshold derived from the last 30 min baseline.
	rules, _ := s.meta.ListAlarmRules(ctx, tenant)
	have := map[string]bool{}
	for _, r := range rules {
		have[r.Name] = true
	}
	now := time.Now().UnixMilli()
	for i, sv := range svcs {
		if i >= 10 {
			break
		}
		name := sv + " · p99 latency high"
		if have[name] || s.created[name] {
			continue
		}
		ms := query.MetricStat{Namespace: probe.NamespaceRED, MetricName: "Latency", Stat: "p99", Dimensions: redDims(sv)}
		pts, err := s.store.FetchStat(ctx, tenant, ms, now-30*60_000, now, 60)
		if err != nil || len(pts) < 5 {
			continue // not enough baseline yet
		}
		mean := 0.0
		for _, p := range pts {
			mean += p.V
		}
		mean /= float64(len(pts))
		thr := max(50, 2*mean)
		r := &alarms.Rule{TenantID: tenant, Name: name, Description: fmt.Sprintf("Auto-calibrated: 2x 30-min baseline p99 (%.1f ms)", mean),
			Metric: &ms, PeriodSec: 60, Enabled: true, Actions: []alarms.Target{{Type: "log"}},
			Config: alarms.Config{Threshold: float64(int(thr)), Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 5, DatapointsToAlarm: 3, TreatMissingData: alarms.MissingNotBreaching}}
		if err := s.meta.SaveAlarmRule(ctx, r); err == nil {
			s.mu.Lock()
			s.created[name] = true
			s.state.Synthesized = append(s.state.Synthesized, "alarm: "+name)
			s.decide("synthesized alarm %q threshold %.0fms", name, r.Config.Threshold)
			s.mu.Unlock()
		}
	}
	s.synthesizeHost(ctx, existing, stale, have)
}

// repairable returns an autogen board saved before boardSchema that nobody
// has edited (version 1), so it can be rebuilt in place. Edited boards are
// left alone: the user's changes win over our fix.
func (s *Supervisor) repairable(ctx context.Context, tenant uint32, title, id string) *metadata.Dashboard {
	if !strings.HasPrefix(title, "Auto") {
		return nil
	}
	s.mu.Lock()
	done := s.created["checked:"+id]
	s.mu.Unlock()
	if done {
		return nil
	}
	d, err := s.meta.GetDashboard(ctx, tenant, id)
	if err != nil {
		return nil
	}
	var layout struct {
		Autogen bool `json:"autogen"`
		Schema  int  `json:"schema"`
	}
	if len(d.Layout) == 0 || json.Unmarshal(d.Layout, &layout) != nil || !layout.Autogen || layout.Schema >= boardSchema {
		s.mu.Lock()
		s.created["checked:"+id] = true
		s.mu.Unlock()
		return nil
	}
	if d.Version != 1 {
		s.mu.Lock()
		s.created["checked:"+id] = true
		s.decide("left edited board %q as is; its queries predate the dimension fix and may show no data", d.Title)
		s.mu.Unlock()
		return nil
	}
	return d
}

// saveBoard creates d, or rebuilds the stale board old in place (same ID,
// compare-and-swap on its version).
func (s *Supervisor) saveBoard(ctx context.Context, d *metadata.Dashboard, old *metadata.Dashboard) {
	verb := "synthesized"
	if old != nil {
		d.ID, d.Version, verb = old.ID, old.Version, "rebuilt"
	}
	if err := s.meta.SaveDashboard(ctx, d); err != nil {
		s.log.Warn("synthesize dashboard failed", "title", d.Title, "err", err)
		return
	}
	s.mu.Lock()
	s.state.Synthesized = append(s.state.Synthesized, "dashboard: "+d.Title)
	s.decide("%s dashboard %q", verb, d.Title)
	s.mu.Unlock()
}
