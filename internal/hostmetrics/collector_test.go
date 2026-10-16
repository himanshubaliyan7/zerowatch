package hostmetrics

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/procfs"
)

func write(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const mounts = `/dev/sda1 / ext4 rw,relatime 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/sdb1 /data\040disk xfs rw 0 0
/dev/sda1 /var/lib/docker ext4 rw 0 0
`

// fakeNode writes a /proc and /sys snapshot; cpu = user idle iowait steal jiffies.
func fakeNode(t *testing.T, proc string, user, idle, iowait, steal, sdaSectors, ethRx uint64) {
	t.Helper()
	write(t, proc, "stat", fmt.Sprintf("cpu  %d 0 0 %d %d 0 0 %d 0 0\ncpu0 1 2 3 4\n", user, idle, iowait, steal))
	write(t, proc, "meminfo", "MemTotal:       8000000 kB\nMemFree:         1000000 kB\nMemAvailable:    2000000 kB\nSwapTotal:       1000000 kB\nSwapFree:         750000 kB\n")
	write(t, proc, "loadavg", "1.50 1.20 0.90 2/345 6789\n")
	write(t, proc, "self/mounts", mounts)
	write(t, proc, "diskstats", fmt.Sprintf("   8       0 sda 100 0 %d 50 200 0 %d 80 0 %d 130\n   8       1 sda1 100 0 %d 50 200 0 0 80 0 100 130\n   7       0 loop0 1 0 2 0 0 0 0 0 0 0 0\n", sdaSectors, sdaSectors, sdaSectors/2, sdaSectors))
	write(t, proc, "net/dev", fmt.Sprintf("Inter-|   Receive\n face |bytes    packets\n    lo: 999 1 0 0 0 0 0 0 999 1 0 0 0 0 0 0\n  eth0: %d 10 0 0 0 0 0 0 %d 5 0 0 0 0 0 0\n", ethRx, ethRx/2))
}

func find(ms []model.MetricDatum, name string, dim ...string) (float64, bool) {
	for _, m := range ms {
		if m.MetricName != name {
			continue
		}
		match := true
		for i := 0; i+1 < len(dim); i += 2 {
			ok := false
			for _, d := range m.Dimensions {
				if d.Name == dim[i] && d.Value == dim[i+1] {
					ok = true
				}
			}
			match = match && ok
		}
		if match {
			return *m.Value, true
		}
	}
	return 0, false
}

func TestCollect(t *testing.T) {
	proc, sys := t.TempDir(), t.TempDir()
	write(t, sys, "block/sda/size", "1")
	fs := map[string]procfs.FSUsage{
		"/":          {Total: 100 << 30, Free: 30 << 30, Avail: 25 << 30, Inodes: 1000, InodesFree: 900},
		"/data disk": {Total: 10 << 30, Free: 9 << 30, Avail: 9 << 30},
	}
	c := &Collector{ProcRoot: proc, SysRoot: sys, Host: "node-1", StatFS: func(p string) (procfs.FSUsage, error) {
		if u, ok := fs[p]; ok {
			return u, nil
		}
		return procfs.FSUsage{}, errors.New("no such mount")
	}}
	t0 := time.Unix(1_800_000_000, 0)
	fakeNode(t, proc, 1000, 9000, 0, 0, 2000, 10_000)
	// A bind-mounted file (as Docker does with /etc/resolv.conf) must not count as a disk.
	file := filepath.Join(t.TempDir(), "resolv.conf")
	write(t, filepath.Dir(file), "resolv.conf", "nameserver 1.1.1.1\n")
	write(t, proc, "self/mounts", mounts+"/dev/sdc1 "+filepath.ToSlash(file)+" ext4 rw 0 0\n")
	fs[file] = procfs.FSUsage{Total: 1 << 30, Free: 1 << 29, Avail: 1 << 29}
	first, errs := c.Collect(t0, 1)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if _, ok := find(first, CPUUtilization); ok {
		t.Error("the first sample cannot have a CPU rate")
	}
	for name, want := range map[string]float64{MemoryUsedPercent: 75, SwapUsedPercent: 25, LoadAverage1: 1.5} {
		if v, ok := find(first, name); !ok || math.Abs(v-want) > 1e-9 {
			t.Errorf("%s = %v (present %v), want %v", name, v, ok, want)
		}
	}
	// df-style: used 70 GiB of (70 + 25) => 73.68 %.
	if v, _ := find(first, DiskUsedPercent, "Mount", "/"); math.Abs(v-100*70.0/95) > 1e-9 {
		t.Errorf("disk used %% of / = %v", v)
	}
	if _, ok := find(first, DiskUsedPercent, "Mount", "/data disk"); !ok {
		t.Error("escaped mount path /data\\040disk not unescaped")
	}
	if _, ok := find(first, DiskUsedPercent, "Mount", filepath.ToSlash(file)); ok {
		t.Error("a bind-mounted file was reported as a disk")
	}
	if _, ok := find(first, DiskUsedPercent, "Mount", "/var/lib/docker"); ok {
		t.Error("a second mount of the same device should be skipped")
	}
	if v, _ := find(first, InodesUsedPercent, "Mount", "/"); v != 10 {
		t.Errorf("inodes used %% = %v", v)
	}
	for _, m := range first {
		if m.Namespace != Namespace || m.Dimensions[0] != (model.Dimension{Name: "Host", Value: "node-1"}) || m.TenantID != 1 {
			t.Fatalf("bad datum %+v", m)
		}
	}

	// 10 s later: 500 busy + 300 idle + 200 iowait jiffies; sda wrote 1000 more sectors.
	fakeNode(t, proc, 1500, 9300, 200, 0, 3000, 60_000)
	second, _ := c.Collect(t0.Add(10*time.Second), 1)
	if v, _ := find(second, CPUUtilization); math.Abs(v-50) > 1e-9 {
		t.Errorf("cpu = %v, want 50", v)
	}
	if v, _ := find(second, CPUIOWait); math.Abs(v-20) > 1e-9 {
		t.Errorf("iowait = %v, want 20", v)
	}
	if v, _ := find(second, DiskWriteBytes, "Device", "sda"); math.Abs(v-1000*512/10.0) > 1e-9 {
		t.Errorf("sda write = %v B/s", v)
	}
	if v, _ := find(second, DiskBusyPercent, "Device", "sda"); math.Abs(v-5) > 1e-9 { // io_ticks 1000 -> 1500: 500 ms busy in 10 s
		t.Errorf("sda busy = %v %%", v)
	}
	if _, ok := find(second, DiskReadBytes, "Device", "sda1"); ok {
		t.Error("partitions must not be reported")
	}
	if _, ok := find(second, DiskReadBytes, "Device", "loop0"); ok {
		t.Error("loop devices must not be reported")
	}
	if v, _ := find(second, NetworkRxBytes, "Interface", "eth0"); v != 5000 {
		t.Errorf("eth0 rx = %v B/s", v)
	}
	if _, ok := find(second, NetworkRxBytes, "Interface", "lo"); ok {
		t.Error("loopback must not be reported")
	}
	for _, m := range second {
		if err := m.Validate(t0.Add(10 * time.Second)); err != nil {
			t.Fatalf("%s fails validation: %v", m.MetricName, err)
		}
	}
}

func TestCollectWithoutProc(t *testing.T) {
	c := &Collector{ProcRoot: filepath.Join(t.TempDir(), "missing"), Host: "h"}
	out, errs := c.Collect(time.Now(), 1)
	if len(out) != 0 || len(errs) == 0 {
		t.Fatalf("out=%d errs=%v", len(out), errs)
	}
}
