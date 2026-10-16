package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/ebpf/probe"
	"github.com/zerowatch/zerowatch/internal/hostmetrics"
	"github.com/zerowatch/zerowatch/internal/ingest"
	"github.com/zerowatch/zerowatch/internal/metadata"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/procfs"
	"github.com/zerowatch/zerowatch/internal/query"
	"github.com/zerowatch/zerowatch/internal/storage"
)

type nopProbe struct{}

func (nopProbe) Name() string { return "nop" }
func (nopProbe) Run(ctx context.Context, _ probe.Sink) error {
	<-ctx.Done()
	return nil
}
func (nopProbe) SetTargets([]probe.Target) {}
func (nopProbe) SetSampleRate(float64)     {}
func (nopProbe) Status() probe.Status {
	return probe.Status{Name: "nop", Mode: "procfs", Healthy: true}
}

type nopIngest struct{}

func (nopIngest) WriteLogs(context.Context, uint32, []model.LogEvent) (ingest.Result, error) {
	return ingest.Result{}, nil
}
func (nopIngest) WriteMetrics(context.Context, uint32, []model.MetricDatum) (ingest.Result, error) {
	return ingest.Result{}, nil
}

// fixture stores 30 minutes of RED series shaped exactly like the probe and
// generator write them ({Route, Service}) plus Net series ({Port, Service}).
func fixture(t *testing.T) (*Supervisor, *metadata.SQLStore, *storage.Memory) {
	t.Helper()
	ctx := context.Background()
	meta, err := metadata.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meta.Close() })
	store := storage.NewMemory(storage.MemoryConfig{})
	t.Cleanup(func() { store.Close() })

	now := time.Now()
	var ms []model.MetricDatum
	for m := 30; m >= 0; m-- {
		ts := now.Add(-time.Duration(m) * time.Minute).UnixMilli()
		for _, route := range []string{"GET /", "POST /api"} {
			red := []model.Dimension{{Name: "Route", Value: route}, {Name: "Service", Value: "checkout"}}
			ms = append(ms,
				model.MetricDatum{TenantID: 1, Namespace: probe.NamespaceRED, MetricName: "Requests", Dimensions: red, Timestamp: ts, Value: model.Float(10)},
				model.MetricDatum{TenantID: 1, Namespace: probe.NamespaceRED, MetricName: "Errors", Dimensions: red, Timestamp: ts, Value: model.Float(0)},
				model.MetricDatum{TenantID: 1, Namespace: probe.NamespaceRED, MetricName: "Latency", Dimensions: red, Timestamp: ts, Value: model.Float(40)},
			)
		}
		net := []model.Dimension{{Name: "Port", Value: "6379"}, {Name: "Service", Value: "redis"}}
		ms = append(ms,
			model.MetricDatum{TenantID: 1, Namespace: probe.NamespaceNet, MetricName: "ActiveConnections", Dimensions: net, Timestamp: ts, Value: model.Float(3)},
			model.MetricDatum{TenantID: 1, Namespace: probe.NamespaceNet, MetricName: "ConnectionsOpened", Dimensions: net, Timestamp: ts, Value: model.Float(1)},
			model.MetricDatum{TenantID: 1, Namespace: probe.NamespaceNet, MetricName: "SocketBacklog", Dimensions: net, Timestamp: ts, Value: model.Float(0)},
		)
	}
	if err := store.WriteMetrics(ctx, ms); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Config{Tenant: 1}, nopProbe{}, nopIngest{}, meta, store, nil, log), meta, store
}

var redis = []procfs.Process{{PID: 10, Comm: "redis-server", Kind: "redis", Ports: []uint16{6379}}}

// Every metric query in every synthesized board must find data. Before the
// fix the queries named {Service} or nothing, matched no series, and the
// self-calibrated alarm was never created for lack of a baseline.
func TestSynthesizeQueriesMatchStoredSeries(t *testing.T) {
	ctx := context.Background()
	sup, meta, store := fixture(t)
	sup.synthesize(ctx, redis)

	rules, err := meta.ListAlarmRules(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rules {
		if r.Name == "checkout · p99 latency high" {
			found = true
			if r.Config.Threshold != 80 { // 2 x 40 ms baseline
				t.Errorf("threshold = %v, want 80", r.Config.Threshold)
			}
		}
	}
	if !found {
		t.Fatalf("self-calibrated alarm not created; rules: %+v", rules)
	}

	boards, err := meta.ListDashboards(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	end := time.Now().UnixMilli() + 60_000
	for _, b := range boards {
		titles[b.Title] = true
		d, err := meta.GetDashboard(ctx, 1, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range d.Widgets {
			var q struct {
				Queries []query.MetricDataQuery `json:"queries"`
			}
			if w.Type == "logs" || json.Unmarshal(w.Query, &q) != nil {
				continue
			}
			for _, mq := range q.Queries {
				if mq.MetricStat == nil {
					continue
				}
				pts, err := store.FetchStat(ctx, 1, *mq.MetricStat, end-30*60_000, end, 60)
				if err != nil || len(pts) == 0 {
					t.Errorf("%s / %s: query %+v found no data (err %v)", d.Title, w.Title, *mq.MetricStat, err)
				}
			}
		}
	}
	for _, want := range []string{"Auto-RED · Services", "Auto · Host network", "Auto · redis"} {
		if !titles[want] {
			t.Errorf("board %q not synthesized; got %v", want, titles)
		}
	}
}

// Boards synthesized before the fix are rebuilt in place when untouched, and
// left alone (with a note in the decision log) when a person edited them.
func TestSynthesizeRebuildsOnlyUneditedStaleBoards(t *testing.T) {
	ctx := context.Background()
	sup, meta, _ := fixture(t)
	oldQuery := json.RawMessage(`{"queries":[{"id":"t","metricStat":{"namespace":"ZeroWatch/RED","metricName":"Requests","stat":"Sum","dimensions":{"Service":"checkout"}}}],"period":60}`)
	oldLayout := json.RawMessage(`{"autogen":true,"range":"1h"}`)

	untouched := &metadata.Dashboard{TenantID: 1, Title: "Auto-RED · Services", Layout: oldLayout,
		Widgets: []metadata.Widget{{Type: "timeseries", Title: "old", Query: oldQuery, W: 12, H: 4}}}
	if err := meta.SaveDashboard(ctx, untouched); err != nil {
		t.Fatal(err)
	}
	edited := &metadata.Dashboard{TenantID: 1, Title: "Auto · Host network", Layout: oldLayout,
		Widgets: []metadata.Widget{{Type: "timeseries", Title: "mine", Query: oldQuery, W: 12, H: 4}}}
	if err := meta.SaveDashboard(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if err := meta.SaveDashboard(ctx, edited); err != nil { // a user edit: version 2
		t.Fatal(err)
	}

	sup.synthesize(ctx, redis)
	sup.synthesize(ctx, redis) // second cycle must not rebuild again or repeat the note

	got, err := meta.GetDashboard(ctx, 1, untouched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || len(got.Widgets) < 2 || !strings.Contains(string(got.Layout), `"schema":2`) {
		t.Errorf("stale untouched board not rebuilt in place: version %d, %d widgets, layout %s", got.Version, len(got.Widgets), got.Layout)
	}
	if !strings.Contains(string(got.Widgets[0].Query), `"Route":"*"`) {
		t.Errorf("rebuilt board still has old queries: %s", got.Widgets[0].Query)
	}

	kept, err := meta.GetDashboard(ctx, 1, edited.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Version != 2 || kept.Widgets[0].Title != "mine" {
		t.Errorf("edited board was overwritten: version %d, widget %q", kept.Version, kept.Widgets[0].Title)
	}
	notes := 0
	for _, d := range sup.State().Decisions {
		if strings.Contains(d, "left edited board") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("want exactly one note about the edited board, got %d: %v", notes, sup.State().Decisions)
	}
}

// The host board's every query (including forecast widgets) must find the
// collector's series, and capacity alarms must be predictive.
func TestSynthesizeHostBoardAndCapacityAlarms(t *testing.T) {
	ctx := context.Background()
	sup, meta, store := fixture(t)
	sup.hostName = "node-1"
	now := time.Now()
	var ms []model.MetricDatum
	host := model.Dimension{Name: "Host", Value: "node-1"}
	other := model.Dimension{Name: "Host", Value: "node-2"} // another node's series: not ours to synthesize
	add := func(name string, v float64, dims ...model.Dimension) {
		for m := 30; m >= 0; m-- {
			ms = append(ms, model.MetricDatum{TenantID: 1, Namespace: hostmetrics.Namespace, MetricName: name, Dimensions: dims,
				Timestamp: now.Add(-time.Duration(m) * time.Minute).UnixMilli(), Value: model.Float(v)})
		}
	}
	for _, n := range []string{hostmetrics.CPUUtilization, hostmetrics.CPUIOWait, hostmetrics.CPUSteal, hostmetrics.LoadAverage1, hostmetrics.MemoryUsedPercent, hostmetrics.SwapUsedPercent} {
		add(n, 40, host)
	}
	add(hostmetrics.MemoryUsedPercent, 40, other)
	for _, mnt := range []string{"/", "/data"} {
		add(hostmetrics.DiskUsedPercent, 60, host, model.Dimension{Name: "Mount", Value: mnt})
	}
	add(hostmetrics.DiskReadBytes, 1e5, host, model.Dimension{Name: "Device", Value: "sda"})
	add(hostmetrics.DiskWriteBytes, 1e5, host, model.Dimension{Name: "Device", Value: "sda"})
	add(hostmetrics.NetworkRxBytes, 1e4, host, model.Dimension{Name: "Interface", Value: "eth0"})
	add(hostmetrics.NetworkTxBytes, 1e4, host, model.Dimension{Name: "Interface", Value: "eth0"})
	if err := store.WriteMetrics(ctx, ms); err != nil {
		t.Fatal(err)
	}
	sup.synthesize(ctx, nil)

	boards, _ := meta.ListDashboards(ctx, 1)
	var board *metadata.Dashboard
	for _, b := range boards {
		if b.Title == "Auto · Host node-2" {
			t.Error("synthesized a board for another node")
		}
		if b.Title == "Auto · Host node-1" {
			board, _ = meta.GetDashboard(ctx, 1, b.ID)
		}
	}
	if board == nil {
		t.Fatalf("host board not synthesized; boards %+v", boards)
	}
	forecasts := 0
	end := now.UnixMilli() + 60_000
	for _, w := range board.Widgets {
		var q struct {
			Queries    []query.MetricDataQuery `json:"queries"`
			HorizonMin int                     `json:"horizonMin"`
		}
		if err := json.Unmarshal(w.Query, &q); err != nil || len(q.Queries) == 0 {
			t.Fatalf("widget %q has no queries: %s", w.Title, w.Query)
		}
		if w.Type == "forecast" {
			forecasts++
			if q.HorizonMin <= 0 {
				t.Errorf("forecast widget %q has no horizon", w.Title)
			}
		}
		for _, mq := range q.Queries {
			if pts, err := store.FetchStat(ctx, 1, *mq.MetricStat, end-30*60_000, end, 60); err != nil || len(pts) == 0 {
				t.Errorf("%s: query %+v found no data (err %v)", w.Title, *mq.MetricStat, err)
			}
		}
	}
	if forecasts != 4 { // memory, CPU, two disks
		t.Errorf("forecast widgets = %d, want 4", forecasts)
	}

	rules, _ := meta.ListAlarmRules(ctx, 1)
	got := map[string]int{}
	for _, r := range rules {
		got[r.Name] = r.ForecastHorizonSec
	}
	for name, horizon := range map[string]int{"node-1 · disk / will fill": 86400, "node-1 · disk /data will fill": 86400, "node-1 · memory will run out": 3600} {
		if got[name] != horizon {
			t.Errorf("rule %q horizon = %d, want %d (rules %v)", name, got[name], horizon, got)
		}
	}
	n := len(rules)
	sup.synthesize(ctx, nil)
	if rules, _ = meta.ListAlarmRules(ctx, 1); len(rules) != n {
		t.Errorf("second cycle duplicated rules: %d -> %d", n, len(rules))
	}
}
