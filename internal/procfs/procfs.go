// Package procfs is a small, dependency-free reader for the Linux /proc
// pseudo-filesystem: processes, sockets (/proc/net/tcp{,6}), CPU, memory and
// host facts. Every function takes the proc root as a parameter so tests can
// point it at a fixture directory, and every reader degrades gracefully on
// non-Linux systems (returns empty results / ErrUnsupported, never panics).
package procfs

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// DefaultRoot is the standard procfs mount point.
const DefaultRoot = "/proc"

// ErrUnsupported is returned on platforms without procfs.
var ErrUnsupported = errors.New("procfs: not available on this platform")

// TCP socket states (include/net/tcp_states.h).
const (
	TCPEstablished = 0x01
	TCPListen      = 0x0A
)

// Socket is one row of /proc/net/tcp or tcp6.
type Socket struct {
	LocalIP    net.IP `json:"localIp"`
	LocalPort  uint16 `json:"localPort"`
	RemoteIP   net.IP `json:"remoteIp"`
	RemotePort uint16 `json:"remotePort"`
	State      uint8  `json:"state"`
	TxQueue    uint64 `json:"txQueue"`
	RxQueue    uint64 `json:"rxQueue"`
	UID        uint32 `json:"uid"`
	Inode      uint64 `json:"inode"`
}

// ParseNetTCP parses the /proc/net/tcp format:
//
//	sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt   uid  timeout inode
//	 0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 ...
//
// Addresses are hex, in host byte order per 32-bit word (little-endian on
// x86/arm64), which is why IPv4 0100007F is 127.0.0.1.
func ParseNetTCP(r io.Reader) ([]Socket, error) {
	var out []Socket
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		lip, lport, err := parseAddr(f[1])
		if err != nil {
			return nil, fmt.Errorf("procfs: local addr %q: %w", f[1], err)
		}
		rip, rport, err := parseAddr(f[2])
		if err != nil {
			return nil, fmt.Errorf("procfs: remote addr %q: %w", f[2], err)
		}
		st, err := strconv.ParseUint(f[3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("procfs: state %q: %w", f[3], err)
		}
		s := Socket{LocalIP: lip, LocalPort: lport, RemoteIP: rip, RemotePort: rport, State: uint8(st)}
		if tx, rx, ok := strings.Cut(f[4], ":"); ok {
			s.TxQueue, _ = strconv.ParseUint(tx, 16, 64)
			s.RxQueue, _ = strconv.ParseUint(rx, 16, 64)
		}
		uid, _ := strconv.ParseUint(f[7], 10, 32)
		s.UID = uint32(uid)
		s.Inode, _ = strconv.ParseUint(f[9], 10, 64)
		out = append(out, s)
	}
	return out, sc.Err()
}

func parseAddr(s string) (net.IP, uint16, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, errors.New("missing port")
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return nil, 0, err
	}
	raw, err := hex.DecodeString(h)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, errors.New("bad ip")
	}
	// Each 32-bit word is stored in host (little-endian) order: reverse per word.
	ip := make(net.IP, len(raw))
	for w := 0; w < len(raw); w += 4 {
		ip[w], ip[w+1], ip[w+2], ip[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
	}
	return ip, uint16(port), nil
}

// ReadSockets reads tcp and tcp6 tables under root.
func ReadSockets(root string) ([]Socket, error) {
	if runtime.GOOS != "linux" && root == DefaultRoot {
		return nil, ErrUnsupported
	}
	var all []Socket
	var firstErr error
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		f, err := os.Open(filepath.Join(root, name))
		if err != nil {
			if firstErr == nil && !errors.Is(err, os.ErrNotExist) {
				firstErr = err
			}
			continue
		}
		s, err := ParseNetTCP(f)
		f.Close()
		if err != nil && firstErr == nil {
			firstErr = err
		}
		all = append(all, s...)
	}
	return all, firstErr
}

// Process is a summarised /proc/<pid> entry.
type Process struct {
	PID     int      `json:"pid"`
	PPID    int      `json:"ppid"`
	Comm    string   `json:"comm"`
	Cmdline string   `json:"cmdline"`
	Kind    string   `json:"kind,omitempty"`
	UTicks  uint64   `json:"-"`
	STicks  uint64   `json:"-"`
	RSSKB   uint64   `json:"rssKb"`
	Threads int      `json:"threads"`
	Inodes  []uint64 `json:"-"`
	Ports   []uint16 `json:"ports,omitempty"`
}

// ParseStat parses /proc/<pid>/stat. comm may contain spaces and ')' so we
// split on the LAST ')'.
func ParseStat(b []byte) (comm string, fields []string, err error) {
	l := bytes.IndexByte(b, '(')
	r := bytes.LastIndexByte(b, ')')
	if l < 0 || r < l {
		return "", nil, errors.New("procfs: malformed stat")
	}
	comm = string(b[l+1 : r])
	fields = strings.Fields(string(b[r+1:]))
	if len(fields) < 22 {
		return "", nil, errors.New("procfs: short stat")
	}
	return comm, fields, nil
}

// ReadProcesses scans every numeric directory under root. Unreadable
// processes (permissions, races with exit) are skipped silently. When
// withFDs is set, socket inodes are collected from /proc/<pid>/fd so ports
// can be attributed to processes.
func ReadProcesses(root string, withFDs bool, limit int) ([]Process, error) {
	if runtime.GOOS != "linux" && root == DefaultRoot {
		return nil, ErrUnsupported
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	pageKB := uint64(os.Getpagesize() / 1024)
	var out []Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		dir := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		comm, f, err := ParseStat(b)
		if err != nil {
			continue
		}
		p := Process{PID: pid, Comm: comm}
		p.PPID, _ = strconv.Atoi(f[1])
		p.UTicks, _ = strconv.ParseUint(f[11], 10, 64)
		p.STicks, _ = strconv.ParseUint(f[12], 10, 64)
		p.Threads, _ = strconv.Atoi(f[17])
		rss, _ := strconv.ParseUint(f[21], 10, 64)
		p.RSSKB = rss * pageKB
		if cl, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			cl = bytes.TrimRight(cl, "\x00")
			p.Cmdline = string(bytes.ReplaceAll(cl, []byte{0}, []byte{' '}))
			if len(p.Cmdline) > 512 {
				p.Cmdline = p.Cmdline[:512]
			}
		}
		p.Kind = ClassifyProcess(p.Comm, p.Cmdline)
		if withFDs {
			p.Inodes = socketInodes(filepath.Join(dir, "fd"))
		}
		out = append(out, p)
	}
	return out, nil
}

func socketInodes(fdDir string) []uint64 {
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var out []uint64
	for _, e := range ents {
		l, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil || !strings.HasPrefix(l, "socket:[") {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]"), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ClassifyProcess maps a process to a well-known workload kind used by the
// supervisor to pick dashboards and probes ("" = not interesting).
func ClassifyProcess(comm, cmdline string) string {
	c := strings.ToLower(comm)
	switch {
	case c == "postgres" || strings.HasPrefix(c, "postgres"):
		return "postgres"
	case strings.HasPrefix(c, "redis"):
		return "redis"
	case c == "nginx":
		return "nginx"
	case c == "mysqld" || c == "mariadbd":
		return "mysql"
	case c == "java":
		return "jvm"
	case c == "node" || c == "bun" || c == "deno":
		return "node"
	case strings.HasPrefix(c, "python"), c == "gunicorn", c == "uvicorn":
		return "python"
	case c == "dockerd" || c == "containerd":
		return "container-runtime"
	case c == "clickhouse-serv" || c == "clickhouse":
		return "clickhouse"
	}
	if strings.Contains(cmdline, "kafka.Kafka") {
		return "kafka"
	}
	return ""
}

// AttachPorts fills Process.Ports with LISTEN ports owned by each process.
func AttachPorts(procs []Process, socks []Socket) {
	listen := map[uint64]uint16{}
	for _, s := range socks {
		if s.State == TCPListen {
			listen[s.Inode] = s.LocalPort
		}
	}
	for i := range procs {
		seen := map[uint16]bool{}
		for _, ino := range procs[i].Inodes {
			if p, ok := listen[ino]; ok && !seen[p] {
				seen[p] = true
				procs[i].Ports = append(procs[i].Ports, p)
			}
		}
	}
}

// CPUTimes are aggregate jiffies from the first line of /proc/stat.
type CPUTimes struct {
	Busy, Total uint64
}

// ReadCPU reads /proc/stat aggregate CPU counters.
func ReadCPU(root string) (CPUTimes, error) {
	b, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return CPUTimes{}, err
	}
	line, _, _ := bytes.Cut(b, []byte{'\n'})
	f := strings.Fields(string(line))
	if len(f) < 5 || f[0] != "cpu" {
		return CPUTimes{}, errors.New("procfs: malformed /proc/stat")
	}
	var t CPUTimes
	for i, s := range f[1:] {
		v, _ := strconv.ParseUint(s, 10, 64)
		t.Total += v
		if i != 3 && i != 4 { // idle, iowait
			t.Busy += v
		}
	}
	return t, nil
}

// MemInfo holds selected /proc/meminfo values in kB.
type MemInfo struct {
	TotalKB     uint64 `json:"totalKb"`
	AvailableKB uint64 `json:"availableKb"`
}

// ReadMemInfo parses /proc/meminfo.
func ReadMemInfo(root string) (MemInfo, error) {
	f, err := os.Open(filepath.Join(root, "meminfo"))
	if err != nil {
		return MemInfo{}, err
	}
	defer f.Close()
	var m MemInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			m.TotalKB = n
		case "MemAvailable":
			m.AvailableKB = n
		}
	}
	return m, sc.Err()
}

// LoadAvg reads the 1-minute load average.
func LoadAvg(root string) (float64, error) {
	b, err := os.ReadFile(filepath.Join(root, "loadavg"))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("procfs: empty loadavg")
	}
	return strconv.ParseFloat(f[0], 64)
}

// SelfUsage returns this process's cumulative CPU ticks and RSS bytes.
func SelfUsage(root string) (ticks uint64, rssBytes uint64, err error) {
	b, err := os.ReadFile(filepath.Join(root, "self", "stat"))
	if err != nil {
		return 0, 0, err
	}
	_, f, err := ParseStat(b)
	if err != nil {
		return 0, 0, err
	}
	u, _ := strconv.ParseUint(f[11], 10, 64)
	s, _ := strconv.ParseUint(f[12], 10, 64)
	rss, _ := strconv.ParseUint(f[21], 10, 64)
	return u + s, rss * uint64(os.Getpagesize()), nil
}

// ClockTicks is USER_HZ; 100 on every mainstream Linux build.
const ClockTicks = 100
