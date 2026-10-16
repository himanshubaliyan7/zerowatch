package procfs

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CPUModes are aggregate jiffies per mode from the first line of /proc/stat.
type CPUModes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total is the sum of every mode (guest time is already in user/nice).
func (c CPUModes) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// ParseCPUModes parses the aggregate "cpu" line of /proc/stat.
func ParseCPUModes(b []byte) (CPUModes, error) {
	line, _, _ := bytes.Cut(b, []byte{'\n'})
	f := strings.Fields(string(line))
	if len(f) < 5 || f[0] != "cpu" {
		return CPUModes{}, errors.New("procfs: malformed /proc/stat")
	}
	v := make([]uint64, 8)
	for i := 0; i < 8 && i+1 < len(f); i++ {
		v[i], _ = strconv.ParseUint(f[i+1], 10, 64)
	}
	return CPUModes{User: v[0], Nice: v[1], System: v[2], Idle: v[3], IOWait: v[4], IRQ: v[5], SoftIRQ: v[6], Steal: v[7]}, nil
}

// ReadCPUModes reads /proc/stat.
func ReadCPUModes(root string) (CPUModes, error) {
	b, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return CPUModes{}, err
	}
	return ParseCPUModes(b)
}

// Memory is /proc/meminfo in bytes.
type Memory struct {
	Total, Available, Free, Buffers, Cached, SwapTotal, SwapFree uint64
}

// ParseMemory parses /proc/meminfo. Kernels before 3.14 lack MemAvailable;
// it is then approximated as free + buffers + cached.
func ParseMemory(b []byte) (Memory, error) {
	var m Memory
	var haveAvail bool
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		n *= 1024
		switch k {
		case "MemTotal":
			m.Total = n
		case "MemAvailable":
			m.Available, haveAvail = n, true
		case "MemFree":
			m.Free = n
		case "Buffers":
			m.Buffers = n
		case "Cached":
			m.Cached = n
		case "SwapTotal":
			m.SwapTotal = n
		case "SwapFree":
			m.SwapFree = n
		}
	}
	if m.Total == 0 {
		return m, errors.New("procfs: no MemTotal in meminfo")
	}
	if !haveAvail {
		m.Available = min(m.Total, m.Free+m.Buffers+m.Cached)
	}
	return m, sc.Err()
}

// ReadMemory reads /proc/meminfo.
func ReadMemory(root string) (Memory, error) {
	b, err := os.ReadFile(filepath.Join(root, "meminfo"))
	if err != nil {
		return Memory{}, err
	}
	return ParseMemory(b)
}

// NetDev is one interface's cumulative counters from /proc/net/dev.
type NetDev struct {
	Name             string
	RxBytes, TxBytes uint64
}

// ParseNetDev parses /proc/net/dev (two header lines, then "name: rx... tx...").
func ParseNetDev(b []byte) []NetDev {
	var out []NetDev
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out = append(out, NetDev{Name: strings.TrimSpace(name), RxBytes: rx, TxBytes: tx})
	}
	return out
}

// ReadNetDev reads /proc/net/dev.
func ReadNetDev(root string) ([]NetDev, error) {
	b, err := os.ReadFile(filepath.Join(root, "net", "dev"))
	if err != nil {
		return nil, err
	}
	return ParseNetDev(b), nil
}

// DiskStat is one block device's cumulative counters from /proc/diskstats.
type DiskStat struct {
	Name                       string
	ReadSectors, WriteSectors  uint64 // 512-byte sectors regardless of the device's block size
	IOTicksMs                  uint64 // time spent doing I/O
	ReadsCompleted, WritesDone uint64
}

// ParseDiskStats parses /proc/diskstats.
func ParseDiskStats(b []byte) []DiskStat {
	var out []DiskStat
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 14 {
			continue
		}
		u := func(i int) uint64 { n, _ := strconv.ParseUint(f[i], 10, 64); return n }
		out = append(out, DiskStat{Name: f[2], ReadsCompleted: u(3), ReadSectors: u(5), WritesDone: u(7), WriteSectors: u(9), IOTicksMs: u(12)})
	}
	return out
}

// ReadDiskStats reads /proc/diskstats.
func ReadDiskStats(root string) ([]DiskStat, error) {
	b, err := os.ReadFile(filepath.Join(root, "diskstats"))
	if err != nil {
		return nil, err
	}
	return ParseDiskStats(b), nil
}

// Mount is one mounted filesystem.
type Mount struct {
	Device, Path, FSType string
}

// pseudoFS are filesystems without real capacity to watch.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "cgroup": true, "cgroup2": true,
	"securityfs": true, "pstore": true, "debugfs": true, "tracefs": true, "configfs": true, "fusectl": true,
	"mqueue": true, "hugetlbfs": true, "bpf": true, "autofs": true, "binfmt_misc": true, "rpc_pipefs": true,
	"nsfs": true, "ramfs": true, "squashfs": true, "efivarfs": true, "selinuxfs": true, "fuse.lxcfs": true,
	"overlay": false, // the container root: worth watching
}

// ParseMounts parses /proc/self/mounts, keeping filesystems with real
// capacity (one entry per device; the first mount point wins) and unescaping
// octal escapes in paths ("\040" = space).
func ParseMounts(b []byte) []Mount {
	var out []Mount
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || pseudoFS[f[2]] {
			continue
		}
		dev, path := f[0], unescapeMount(f[1])
		key := dev
		if f[2] == "overlay" || !strings.HasPrefix(dev, "/") {
			key = dev + path // overlay/network mounts share pseudo device names
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Mount{Device: dev, Path: path, FSType: f[2]})
	}
	return out
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ReadMounts reads <root>/self/mounts.
func ReadMounts(root string) ([]Mount, error) {
	b, err := os.ReadFile(filepath.Join(root, "self", "mounts"))
	if err != nil {
		return nil, err
	}
	return ParseMounts(b), nil
}

// FSUsage is a filesystem's capacity in bytes and inodes.
type FSUsage struct {
	Total, Free, Avail uint64 // Avail = free to unprivileged users
	Inodes, InodesFree uint64
}
