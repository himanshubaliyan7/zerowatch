// Package hostmetrics samples node resources from /proc and statfs: CPU,
// memory, swap, filesystem usage, disk I/O and network throughput. The
// supervisor writes them every cycle under namespace ZeroWatch/Host, so they
// can be charted, alarmed on and forecast like any other metric (capacity
// alarms such as "disk will fill within 24 h" are synthesized from them).
//
// Rates (CPU %, bytes/second, busy %) come from the difference between two
// samples, so the first Collect after start emits only gauges.
package hostmetrics

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/procfs"
)

// Namespace holds every node metric. Dimensions: Host on all of them, plus
// Mount (filesystems), Device (disk I/O) or Interface (network).
const Namespace = "ZeroWatch/Host"

// Metric names.
const (
	CPUUtilization    = "CPUUtilization" // % busy (not idle or iowait), all CPUs
	CPUIOWait         = "CPUIOWait"
	CPUSteal          = "CPUSteal"
	LoadAverage1      = "LoadAverage1"
	MemoryUsedPercent = "MemoryUsedPercent" // (total - available) / total
	MemoryUsed        = "MemoryUsed"
	MemoryAvailable   = "MemoryAvailable"
	SwapUsedPercent   = "SwapUsedPercent"
	DiskUsedPercent   = "DiskUsedPercent" // used / (used + available to users), as df reports it
	DiskUsed          = "DiskUsed"
	DiskFree          = "DiskFree" // available to unprivileged users
	InodesUsedPercent = "InodesUsedPercent"
	DiskReadBytes     = "DiskReadBytes"  // bytes/second
	DiskWriteBytes    = "DiskWriteBytes" // bytes/second
	DiskBusyPercent   = "DiskBusyPercent"
	NetworkRxBytes    = "NetworkRxBytes" // bytes/second
	NetworkTxBytes    = "NetworkTxBytes" // bytes/second
)

// Collector samples one node. Not safe for concurrent use.
type Collector struct {
	ProcRoot string // default /proc
	SysRoot  string // default /sys; used to tell whole disks from partitions
	// FSRoot prefixes mount paths for statfs, for a container that sees the
	// host's filesystems under e.g. /host (mount paths stay as the host names them).
	FSRoot string
	Host   string // the Host dimension (default: hostname)
	// Caps on per-mount, per-device and per-interface series (default 20 each).
	MaxMounts, MaxDevices, MaxInterfaces int
	// StatFS is procfs.StatFS unless a test replaces it.
	StatFS func(path string) (procfs.FSUsage, error)

	prev *sample
}

type sample struct {
	at    time.Time
	cpu   procfs.CPUModes
	disks map[string]procfs.DiskStat
	nets  map[string]procfs.NetDev
}

func (c *Collector) defaults() {
	if c.ProcRoot == "" {
		c.ProcRoot = procfs.DefaultRoot
	}
	if c.SysRoot == "" {
		c.SysRoot = "/sys"
	}
	if c.Host == "" {
		c.Host, _ = os.Hostname()
	}
	if c.MaxMounts <= 0 {
		c.MaxMounts = 20
	}
	if c.MaxDevices <= 0 {
		c.MaxDevices = 20
	}
	if c.MaxInterfaces <= 0 {
		c.MaxInterfaces = 20
	}
	if c.StatFS == nil {
		c.StatFS = procfs.StatFS
	}
}

// Collect samples the node at now. A source that cannot be read (no /proc on
// this OS, a mount that vanished) is skipped; errs lists why.
func (c *Collector) Collect(now time.Time, tenant uint32) (out []model.MetricDatum, errs []error) {
	c.defaults()
	ts := now.UnixMilli()
	put := func(name, unit string, v float64, extra ...model.Dimension) {
		dims := append([]model.Dimension{{Name: "Host", Value: c.Host}}, extra...)
		out = append(out, model.MetricDatum{TenantID: tenant, Namespace: Namespace, MetricName: name, Dimensions: dims, Timestamp: ts, Value: model.Float(v), Unit: unit})
	}
	cur := &sample{at: now, disks: map[string]procfs.DiskStat{}, nets: map[string]procfs.NetDev{}}
	prev := c.prev
	elapsed := 0.0
	if prev != nil {
		elapsed = now.Sub(prev.at).Seconds()
	}
	rate := elapsed > 0.5

	// CPU.
	if cpu, err := procfs.ReadCPUModes(c.ProcRoot); err == nil {
		cur.cpu = cpu
		if rate && cpu.Total() > prev.cpu.Total() {
			d := float64(cpu.Total() - prev.cpu.Total())
			busy := float64((cpu.Total() - cpu.Idle - cpu.IOWait) - (prev.cpu.Total() - prev.cpu.Idle - prev.cpu.IOWait))
			put(CPUUtilization, "Percent", clampPct(100*busy/d))
			put(CPUIOWait, "Percent", clampPct(100*float64(cpu.IOWait-prev.cpu.IOWait)/d))
			put(CPUSteal, "Percent", clampPct(100*float64(cpu.Steal-prev.cpu.Steal)/d))
		}
	} else {
		errs = append(errs, err)
	}
	if l, err := procfs.LoadAvg(c.ProcRoot); err == nil {
		put(LoadAverage1, "None", l)
	}

	// Memory.
	if m, err := procfs.ReadMemory(c.ProcRoot); err == nil {
		used := m.Total - min(m.Available, m.Total)
		put(MemoryUsedPercent, "Percent", 100*float64(used)/float64(m.Total))
		put(MemoryUsed, "Bytes", float64(used))
		put(MemoryAvailable, "Bytes", float64(m.Available))
		if m.SwapTotal > 0 {
			put(SwapUsedPercent, "Percent", 100*float64(m.SwapTotal-min(m.SwapFree, m.SwapTotal))/float64(m.SwapTotal))
		}
	} else {
		errs = append(errs, err)
	}

	// Filesystems.
	if mounts, err := procfs.ReadMounts(c.ProcRoot); err == nil {
		n := 0
		for _, mt := range mounts {
			if n >= c.MaxMounts {
				break
			}
			p := filepath.Join(c.FSRoot, mt.Path)
			// Containers bind-mount single files (/etc/resolv.conf, /etc/hosts):
			// not a disk anyone fills.
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				continue
			}
			u, err := c.StatFS(p)
			if err != nil || u.Total == 0 {
				continue
			}
			n++
			dim := model.Dimension{Name: "Mount", Value: mt.Path}
			used := u.Total - min(u.Free, u.Total)
			if denom := used + u.Avail; denom > 0 {
				put(DiskUsedPercent, "Percent", 100*float64(used)/float64(denom), dim)
			}
			put(DiskUsed, "Bytes", float64(used), dim)
			put(DiskFree, "Bytes", float64(u.Avail), dim)
			if u.Inodes > 0 {
				put(InodesUsedPercent, "Percent", 100*float64(u.Inodes-min(u.InodesFree, u.Inodes))/float64(u.Inodes), dim)
			}
		}
	} else {
		errs = append(errs, err)
	}

	// Disk I/O, whole disks only.
	if ds, err := procfs.ReadDiskStats(c.ProcRoot); err == nil {
		for _, d := range ds {
			if len(cur.disks) >= c.MaxDevices {
				break
			}
			if !c.wholeDisk(d.Name) {
				continue
			}
			cur.disks[d.Name] = d
			p, ok := prev.disk(d.Name)
			if !rate || !ok || d.ReadSectors < p.ReadSectors || d.WriteSectors < p.WriteSectors {
				continue // first sample or counter reset
			}
			dim := model.Dimension{Name: "Device", Value: d.Name}
			put(DiskReadBytes, "Bytes/Second", float64(d.ReadSectors-p.ReadSectors)*512/elapsed, dim)
			put(DiskWriteBytes, "Bytes/Second", float64(d.WriteSectors-p.WriteSectors)*512/elapsed, dim)
			if d.IOTicksMs >= p.IOTicksMs {
				put(DiskBusyPercent, "Percent", clampPct(float64(d.IOTicksMs-p.IOTicksMs)/(elapsed*10)), dim)
			}
		}
	}

	// Network, except loopback.
	if nets, err := procfs.ReadNetDev(c.ProcRoot); err == nil {
		sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
		for _, n := range nets {
			if n.Name == "lo" || len(cur.nets) >= c.MaxInterfaces {
				continue
			}
			cur.nets[n.Name] = n
			p, ok := prev.net(n.Name)
			if !rate || !ok || n.RxBytes < p.RxBytes || n.TxBytes < p.TxBytes {
				continue
			}
			dim := model.Dimension{Name: "Interface", Value: n.Name}
			put(NetworkRxBytes, "Bytes/Second", float64(n.RxBytes-p.RxBytes)/elapsed, dim)
			put(NetworkTxBytes, "Bytes/Second", float64(n.TxBytes-p.TxBytes)/elapsed, dim)
		}
	}
	c.prev = cur
	return out, errs
}

func (s *sample) disk(name string) (procfs.DiskStat, bool) {
	if s == nil {
		return procfs.DiskStat{}, false
	}
	d, ok := s.disks[name]
	return d, ok
}

func (s *sample) net(name string) (procfs.NetDev, bool) {
	if s == nil {
		return procfs.NetDev{}, false
	}
	n, ok := s.nets[name]
	return n, ok
}

// wholeDisk reports whether a /proc/diskstats name is a disk rather than a
// partition, loop or RAM device: /sys/block lists exactly the disks; without
// /sys, loop/ram/zram are dropped and nothing else is guessed.
func (c *Collector) wholeDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "fd", "sr"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	if _, err := os.Stat(filepath.Join(c.SysRoot, "block")); err != nil {
		return true
	}
	_, err := os.Stat(filepath.Join(c.SysRoot, "block", name))
	return err == nil
}

func clampPct(v float64) float64 { return max(0, min(100, v)) }
