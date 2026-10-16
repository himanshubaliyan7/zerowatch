//go:build linux && ebpf

package probe

import (
	"encoding/binary"
	"os"
	"testing"

	cebpf "github.com/cilium/ebpf"
)

// objForTest is produced by `make ebpf` (or set ZW_BPF_OBJ).
func objForTest(t *testing.T) string {
	p := os.Getenv("ZW_BPF_OBJ")
	if p == "" {
		p = "../red_probe.bpf.o"
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("bpf object not built (%s): run `make ebpf`", p)
	}
	return p
}

func TestObjectSpec(t *testing.T) {
	spec, err := cebpf.LoadCollectionSpec(objForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	for prog := range tracepoints {
		if spec.Programs[prog] == nil {
			t.Errorf("program %s missing", prog)
		}
	}
	for _, u := range tlsUprobes {
		for _, prog := range []string{u.entry, u.ret} {
			if prog != "" && spec.Programs[prog] == nil {
				t.Errorf("TLS program %s missing", prog)
			}
		}
	}
	for _, m := range []string{"target_pids", "pidns_ino", "sample_every", "inflight", "active_ssl_reads", "events"} {
		if spec.Maps[m] == nil {
			t.Errorf("map %s missing", m)
		}
	}
}

// TestKernelLoad verifies the programs pass the in-kernel verifier. Needs root.
func TestKernelLoad(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_BPF")
	}
	spec, err := cebpf.LoadCollectionSpec(objForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	coll, err := cebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("verifier rejected programs: %+v", err)
	}
	coll.Close()
}

func TestDecodeEvent(t *testing.T) {
	b := make([]byte, eventSize)
	binary.LittleEndian.PutUint64(b[8:], 1500000)
	binary.LittleEndian.PutUint32(b[16:], 42)
	binary.LittleEndian.PutUint16(b[20:], 404)
	copy(b[24:], "GET")
	copy(b[32:], "/users/7")
	copy(b[96:], "nginx")
	ev, err := decodeEvent(b)
	if err != nil {
		t.Fatal(err)
	}
	if ev.PID != 42 || ev.Status != 404 || ev.Method != "GET" || ev.Path != "/users/7" || ev.Comm != "nginx" || ev.LatencyNs != 1500000 || ev.TLS {
		t.Fatalf("bad decode %+v", ev)
	}
	binary.LittleEndian.PutUint16(b[22:], redFlagTLS)
	if ev, _ := decodeEvent(b); !ev.TLS || ev.Status != 404 {
		t.Fatalf("TLS flag not decoded: %+v", ev)
	}
	if _, err := decodeEvent(b[:10]); err == nil {
		t.Fatal("expected short event error")
	}
}
