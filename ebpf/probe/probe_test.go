package probe

import (
	"net"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/procfs"
)

func TestNormalizeRoute(t *testing.T) {
	cases := map[[2]string]string{
		{"GET", "/users/42/orders/7?x=1"}:                     "GET /users/:id/orders/:id",
		{"POST", "/a/3fa85f64-5717-4562-b3fc-2c963f66afa6/b"}: "POST /a/:id/b",
		{"", ""}:                              "UNKNOWN /",
		{"GET", "/blob/deadbeefdeadbeefcafe"}: "GET /blob/:id",
		{"GET", "/a/b/c/d/e/f/g/h/i"}:         "GET /a/b/c/d/e/f/*",
	}
	for in, want := range cases {
		if got := NormalizeRoute(in[0], in[1]); got != want {
			t.Errorf("NormalizeRoute(%q,%q)=%q want %q", in[0], in[1], got, want)
		}
	}
}

func TestAggregatorFlush(t *testing.T) {
	a := NewAggregator(2, 10)
	a.Add("api", REDEvent{Method: "GET", Path: "/u/1", Status: 200, LatencyNs: 5e6})
	a.Add("api", REDEvent{Method: "GET", Path: "/u/2", Status: 503, LatencyNs: 15e6})
	a.Add("api", REDEvent{Method: "POST", Path: "/x", Status: 201, LatencyNs: 1e6})
	a.Add("api", REDEvent{Method: "PUT", Path: "/y", Status: 200, LatencyNs: 1e6}) // overflow -> OTHER
	ms := a.Flush(1_000_000)
	counts := map[string]float64{}
	lat := 0
	for _, m := range ms {
		route := m.DimMap()["Route"]
		switch m.MetricName {
		case "Requests":
			counts[route+"|req"] = *m.Value
		case "Errors":
			counts[route+"|err"] = *m.Value
		case "Latency":
			lat++
		}
	}
	if counts["GET /u/:id|req"] != 2 || counts["GET /u/:id|err"] != 1 {
		t.Fatalf("bad GET counts %v", counts)
	}
	if counts["OTHER|req"] != 1 {
		t.Fatalf("expected overflow bucket, got %v", counts)
	}
	if lat != 4 {
		t.Fatalf("latency samples=%d", lat)
	}
	if len(a.Flush(2_000_000)) != 0 {
		t.Fatal("flush did not reset")
	}
}

func TestProcProbeFold(t *testing.T) {
	p := NewProcProbe(nil)
	p.SetTargets([]Target{{PID: 10, Port: 8080, Service: "web"}})
	sock := func(rport uint16) procfs.Socket {
		return procfs.Socket{LocalPort: 8080, RemoteIP: net.ParseIP("10.0.0.1"), RemotePort: rport,
			State: procfs.TCPEstablished, RxQueue: 10}
	}
	now := time.Unix(1000, 0)
	ms := p.fold(now, []procfs.Socket{sock(1), sock(2), {LocalPort: 9999, State: procfs.TCPEstablished}})
	get := func(name string) float64 {
		for _, m := range ms {
			if m.MetricName == name {
				return *m.Value
			}
		}
		return -1
	}
	if get("ActiveConnections") != 2 || get("ConnectionsOpened") != 2 || get("SocketBacklog") != 20 {
		t.Fatalf("unexpected first fold: %+v", ms)
	}
	ms = p.fold(now.Add(10*time.Second), []procfs.Socket{sock(2), sock(3)})
	if get("ActiveConnections") != 2 || get("ConnectionsOpened") != 1 {
		t.Fatalf("expected 1 new connection on second fold")
	}
}
