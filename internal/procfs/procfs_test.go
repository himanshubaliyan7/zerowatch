package procfs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const netTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 0100007F:D431 01 00000010:00000020 00:00000000 00000000  1000        0 4243 1 0000000000000000 20 4 30 10 -1
`

func TestParseNetTCP(t *testing.T) {
	s, err := ParseNetTCP(strings.NewReader(netTCP))
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 {
		t.Fatalf("got %d sockets", len(s))
	}
	if s[0].LocalIP.String() != "127.0.0.1" || s[0].LocalPort != 8080 || s[0].State != TCPListen || s[0].Inode != 4242 {
		t.Fatalf("bad listen socket: %+v", s[0])
	}
	if s[1].RemotePort != 0xD431 || s[1].State != TCPEstablished || s[1].TxQueue != 16 || s[1].RxQueue != 32 {
		t.Fatalf("bad established socket: %+v", s[1])
	}
	procs := []Process{{PID: 1, Inodes: []uint64{4242, 4243}}}
	AttachPorts(procs, s)
	if len(procs[0].Ports) != 1 || procs[0].Ports[0] != 8080 {
		t.Fatalf("ports: %v", procs[0].Ports)
	}
	if _, err := ParseNetTCP(strings.NewReader("hdr\n 0: zz:1 00000000:0000 0A 0:0 0 0 0 0 1\n")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseStatWithParens(t *testing.T) {
	line := "123 (my (weird) proc) S 1 123 123 0 -1 4194560 100 0 0 0 50 25 0 0 20 0 7 0 1000 100000 250 18446744073709551615"
	comm, f, err := ParseStat([]byte(line))
	if err != nil || comm != "my (weird) proc" || f[11] != "50" || f[21] != "250" {
		t.Fatalf("comm=%q f=%v err=%v", comm, f, err)
	}
}

func TestReadProcessesFixture(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "77")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "stat"), []byte("77 (postgres) S 1 77 77 0 -1 0 0 0 0 0 9 1 0 0 20 0 4 0 1 1 10 0"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "cmdline"), []byte("postgres\x00-D\x00/data\x00"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "stat"), []byte("cpu  100 0 50 800 50 0 0 0 0 0\n"), 0o644)
	ps, err := ReadProcesses(root, false, 0)
	if err != nil || len(ps) != 1 {
		t.Fatalf("ps=%v err=%v", ps, err)
	}
	if ps[0].Kind != "postgres" || ps[0].Cmdline != "postgres -D /data" || ps[0].Threads != 4 {
		t.Fatalf("%+v", ps[0])
	}
	c, err := ReadCPU(root)
	if err != nil || c.Total != 1000 || c.Busy != 150 {
		t.Fatalf("cpu=%+v err=%v", c, err)
	}
}

func TestSelfUsage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	_, rss, err := SelfUsage(DefaultRoot)
	if err != nil || rss == 0 {
		t.Fatalf("rss=%d err=%v", rss, err)
	}
	if !kernelAtLeast("5.15.0-1-aws", 5, 8) || kernelAtLeast("4.19.0", 5, 8) {
		t.Fatal("kernelAtLeast")
	}
}
