package cluster

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"
)

func freeUDPTCPPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		u, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		u.Close()
		return port
	}
	t.Fatal("no free port")
	return 0
}

// A node whose seed isn't up yet (in Kubernetes: its DNS name doesn't
// resolve yet) must keep retrying and join once the seed appears.
func TestJoinRetriesSeedsUntilPeerAppears(t *testing.T) {
	oldMin, oldMax := rejoinMin, rejoinMax
	rejoinMin, rejoinMax = 50*time.Millisecond, 200*time.Millisecond
	defer func() { rejoinMin, rejoinMax = oldMin, oldMax }()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	seedPort, bPort := freeUDPTCPPort(t), freeUDPTCPPort(t)
	seed := "127.0.0.1:" + strconv.Itoa(seedPort)
	b, err := Join(MembershipConfig{Self: NodeMeta{Name: "b", Role: "query", HTTPAddr: ":8080"},
		BindAddr: "127.0.0.1", BindPort: bPort, Seeds: []string{seed}}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Leave()
	if n := b.ml.NumMembers(); n != 1 {
		t.Fatalf("b members before the seed exists = %d, want 1", n)
	}

	time.Sleep(300 * time.Millisecond) // several failed attempts first
	a, err := Join(MembershipConfig{Self: NodeMeta{Name: "a", Role: "storage", HTTPAddr: ":8080"},
		BindAddr: "127.0.0.1", BindPort: seedPort}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Leave()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := b.Lookup("a"); ok && len(b.Ring().Nodes()) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("b never joined a: members=%v", b.Members())
}

func TestLeaveStopsRejoinLoop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := Join(MembershipConfig{Self: NodeMeta{Name: "solo", Role: "query"},
		BindAddr: "127.0.0.1", BindPort: freeUDPTCPPort(t), Seeds: []string{"127.0.0.1:1"}}, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Leave(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.done:
	default:
		t.Fatal("done not closed by Leave")
	}
}
