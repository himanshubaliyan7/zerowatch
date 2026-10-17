package supervisor

import (
	"context"
	"fmt"
	"sort"

	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
	"github.com/himanshubaliyan7/zerowatch/internal/hostmetrics"
	"github.com/himanshubaliyan7/zerowatch/internal/metadata"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

// Capacity alarm settings: predictive rules on this node's resources.
const (
	diskFullPct      = 90.0
	diskHorizonSec   = 24 * 3600
	memFullPct       = 95.0
	memHorizonSec    = 3600
	maxHostMounts    = 6 // forecast widgets and disk alarms per host
	maxHostBoardDisk = 4
)

func hostDims(host string, extra ...string) map[string]string {
	d := map[string]string{"Host": host}
	for i := 0; i+1 < len(extra); i += 2 {
		d[extra[i]] = extra[i+1]
	}
	return d
}

// hostSeries lists the distinct values of dim for metric on this host.
func (s *Supervisor) hostSeries(ctx context.Context, metric, dim string) []string {
	ss, err := s.store.ListSeries(ctx, s.cfg.Tenant, storage.SeriesFilter{Namespace: hostmetrics.Namespace, MetricName: metric, Limit: 1000})
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range ss {
		if m.Dimensions["Host"] != s.hostName {
			continue
		}
		v := m.Dimensions[dim]
		if dim == "" {
			v = "-"
		}
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// synthesizeHost builds this node's resource board (with forecasts) and its
// predictive capacity alarms once the collector's series exist.
func (s *Supervisor) synthesizeHost(ctx context.Context, existing map[string]bool, stale map[string]*metadata.Dashboard, have map[string]bool) {
	if len(s.hostSeries(ctx, hostmetrics.MemoryUsedPercent, "")) == 0 {
		return // collector off, or its first cycle not stored yet
	}
	tenant, h, ns := s.cfg.Tenant, s.hostName, hostmetrics.Namespace
	mounts := s.hostSeries(ctx, hostmetrics.DiskUsedPercent, "Mount")
	if len(mounts) > maxHostMounts {
		mounts = mounts[:maxHostMounts]
	}
	devices := s.hostSeries(ctx, hostmetrics.DiskWriteBytes, "Device")
	ifaces := s.hostSeries(ctx, hostmetrics.NetworkRxBytes, "Interface")

	title := "Auto · Host " + h
	if !existing[title] {
		node := hostDims(h)
		anyMount := hostDims(h, "Mount", "*")
		fw := func(t string, q query.MetricDataQuery, period, horizonMin int, threshold float64, x, y, w int) metadata.Widget {
			return metadata.Widget{Type: "forecast", Title: t, X: x, Y: y, W: w, H: 4,
				Query: jsonRaw(map[string]any{"queries": []query.MetricDataQuery{q}, "period": period, "horizonMin": horizonMin, "threshold": threshold})}
		}
		ws := []metadata.Widget{
			{Type: "stat", Title: "CPU used %", Query: ts(mq("c", "cpu", ns, hostmetrics.CPUUtilization, "Average", node)), X: 0, Y: 0, W: 3, H: 2},
			{Type: "stat", Title: "Memory used %", Query: ts(mq("m", "memory", ns, hostmetrics.MemoryUsedPercent, "Average", node)), X: 3, Y: 0, W: 3, H: 2},
			{Type: "stat", Title: "Fullest disk %", Query: ts(mq("d", "disk", ns, hostmetrics.DiskUsedPercent, "Maximum", anyMount)), X: 6, Y: 0, W: 3, H: 2},
			{Type: "stat", Title: "Load average (1 min)", Query: ts(mq("l", "load", ns, hostmetrics.LoadAverage1, "Average", node)), X: 9, Y: 0, W: 3, H: 2},
			fw("Memory used % · next 6 h", mq("m", "memory %", ns, hostmetrics.MemoryUsedPercent, "Average", node), 60, 360, memFullPct, 0, 2, 6),
			fw("CPU used % · next 6 h", mq("c", "cpu %", ns, hostmetrics.CPUUtilization, "Average", node), 60, 360, 90, 6, 2, 6),
		}
		y := 6
		for i, m := range mounts {
			if i >= maxHostBoardDisk {
				break
			}
			ws = append(ws, fw("Disk "+m+" used % · next 24 h", mq("d", m, ns, hostmetrics.DiskUsedPercent, "Maximum", hostDims(h, "Mount", m)), 300, 1440, diskFullPct, (i%2)*6, y+(i/2)*4, 6))
		}
		y += 4 * ((min(len(mounts), maxHostBoardDisk) + 1) / 2)
		cpuQ := []query.MetricDataQuery{
			mq("u", "used", ns, hostmetrics.CPUUtilization, "Average", node),
			mq("w", "iowait", ns, hostmetrics.CPUIOWait, "Average", node),
			mq("s", "steal", ns, hostmetrics.CPUSteal, "Average", node),
		}
		ws = append(ws, metadata.Widget{Type: "timeseries", Title: "CPU %", Query: ts(cpuQ...), X: 0, Y: y, W: 6, H: 4})
		memQ := []query.MetricDataQuery{mq("m", "memory %", ns, hostmetrics.MemoryUsedPercent, "Average", node)}
		if len(s.hostSeries(ctx, hostmetrics.SwapUsedPercent, "")) > 0 {
			memQ = append(memQ, mq("s", "swap %", ns, hostmetrics.SwapUsedPercent, "Average", node))
		}
		ws = append(ws, metadata.Widget{Type: "timeseries", Title: "Memory and swap %", Query: ts(memQ...), X: 6, Y: y, W: 6, H: 4})
		y += 4
		if len(devices) > 0 {
			var q []query.MetricDataQuery
			for i, d := range devices {
				if i >= 4 {
					break
				}
				q = append(q, mq(fmt.Sprintf("r%d", i), d+" read", ns, hostmetrics.DiskReadBytes, "Average", hostDims(h, "Device", d)),
					mq(fmt.Sprintf("w%d", i), d+" write", ns, hostmetrics.DiskWriteBytes, "Average", hostDims(h, "Device", d)))
			}
			ws = append(ws, metadata.Widget{Type: "timeseries", Title: "Disk I/O (bytes/s)", Query: ts(q...), X: 0, Y: y, W: 6, H: 4})
		}
		if len(ifaces) > 0 {
			var q []query.MetricDataQuery
			for i, n := range ifaces {
				if i >= 4 {
					break
				}
				q = append(q, mq(fmt.Sprintf("r%d", i), n+" in", ns, hostmetrics.NetworkRxBytes, "Average", hostDims(h, "Interface", n)),
					mq(fmt.Sprintf("t%d", i), n+" out", ns, hostmetrics.NetworkTxBytes, "Average", hostDims(h, "Interface", n)))
			}
			ws = append(ws, metadata.Widget{Type: "timeseries", Title: "Network (bytes/s)", Query: ts(q...), X: 6, Y: y, W: 6, H: 4})
		}
		s.saveBoard(ctx, &metadata.Dashboard{TenantID: tenant, Title: title, Description: "CPU, memory, disks and network of " + h + ", with forecasts",
			Layout: jsonRaw(map[string]any{"autogen": true, "schema": boardSchema, "range": "1h", "refresh": 30}), Widgets: ws}, stale[title])
	}

	// Predictive capacity alarms: they fire on the forecast, before the resource runs out.
	var want []alarms.Rule
	for _, m := range mounts {
		ms := query.MetricStat{Namespace: ns, MetricName: hostmetrics.DiskUsedPercent, Stat: "Maximum", Dimensions: hostDims(h, "Mount", m)}
		want = append(want, alarms.Rule{Name: fmt.Sprintf("%s · disk %s will fill", h, m), Metric: &ms, PeriodSec: 300, ForecastHorizonSec: diskHorizonSec,
			Description: fmt.Sprintf("Predictive: the 24 h forecast of %s use crosses %.0f %% for 3 of 3 periods of 5 min.", m, diskFullPct),
			Config:      alarms.Config{Threshold: diskFullPct, Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 3, DatapointsToAlarm: 3, TreatMissingData: alarms.MissingMissing}})
	}
	mem := query.MetricStat{Namespace: ns, MetricName: hostmetrics.MemoryUsedPercent, Stat: "Average", Dimensions: hostDims(h)}
	want = append(want, alarms.Rule{Name: h + " · memory will run out", Metric: &mem, PeriodSec: 60, ForecastHorizonSec: memHorizonSec,
		Description: fmt.Sprintf("Predictive: the 1 h forecast of memory use crosses %.0f %% for 3 of 5 minutes.", memFullPct),
		Config:      alarms.Config{Threshold: memFullPct, Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 5, DatapointsToAlarm: 3, TreatMissingData: alarms.MissingMissing}})
	for i := range want {
		r := &want[i]
		if have[r.Name] || s.created[r.Name] {
			continue
		}
		r.TenantID, r.Enabled, r.Actions = tenant, true, []alarms.Target{{Type: "log"}}
		if err := s.meta.SaveAlarmRule(ctx, r); err != nil {
			s.log.Warn("synthesize capacity alarm failed", "name", r.Name, "err", err)
			continue
		}
		s.mu.Lock()
		s.created[r.Name] = true
		s.state.Synthesized = append(s.state.Synthesized, "alarm: "+r.Name)
		s.decide("synthesized predictive alarm %q (forecast %s > %.0f%%)", r.Name, fmtHorizon(r.ForecastHorizonSec), r.Config.Threshold)
		s.mu.Unlock()
	}
}

func fmtHorizon(sec int) string {
	if sec%3600 == 0 {
		return fmt.Sprintf("%dh", sec/3600)
	}
	return fmt.Sprintf("%dm", sec/60)
}
