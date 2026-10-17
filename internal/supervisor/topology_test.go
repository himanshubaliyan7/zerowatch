package supervisor

import (
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/himanshubaliyan7/zerowatch/internal/procfs"
)

func listen(ip string, port uint16, ino uint64) procfs.Socket {
	return procfs.Socket{LocalIP: net.ParseIP(ip).To4(), LocalPort: port, RemoteIP: net.IPv4zero.To4(), State: procfs.TCPListen, Inode: ino}
}

func est(lip string, lport uint16, rip string, rport uint16, ino uint64) procfs.Socket {
	return procfs.Socket{LocalIP: net.ParseIP(lip).To4(), LocalPort: lport, RemoteIP: net.ParseIP(rip).To4(), RemotePort: rport, State: procfs.TCPEstablished, Inode: ino}
}

// loopback returns both ends of client:cport -> 127.0.0.1:sport.
func loopback(cport, sport uint16, clientIno, serverIno uint64) []procfs.Socket {
	return []procfs.Socket{
		est("127.0.0.1", cport, "127.0.0.1", sport, clientIno),
		est("127.0.0.1", sport, "127.0.0.1", cport, serverIno),
	}
}

func TestBuildTopology(t *testing.T) {
	api := func(extra ...uint64) procfs.Process {
		return procfs.Process{PID: 20, Comm: "api", Inodes: append([]uint64{100}, extra...), Ports: []uint16{8080}}
	}
	redis := procfs.Process{PID: 10, Comm: "redis-server", Kind: "redis", Inodes: []uint64{101, 202}, Ports: []uint16{6379}}
	apiNode := TopoNode{ID: "api", Label: "api", Kind: "service", PID: 20, Ports: []uint16{8080}}
	redisNode := TopoNode{ID: "redis", Label: "redis", Kind: "service", PID: 10, Ports: []uint16{6379}}
	base := []procfs.Socket{listen("0.0.0.0", 8080, 100), listen("127.0.0.1", 6379, 101)}

	var many []procfs.Socket
	for i := 1; i <= 25; i++ {
		for c := 0; c < i; c++ {
			many = append(many, est("10.0.0.5", 8080, fmt.Sprintf("198.51.100.%d", i), uint16(40000+c), uint64(1000+100*i+c)))
		}
	}

	cases := []struct {
		name      string
		procs     []procfs.Process
		socks     []procfs.Socket
		wantNodes []string
		wantEdges []TopoEdge
	}{{
		name:      "local client to service via inode ownership",
		procs:     []procfs.Process{api(201), redis},
		socks:     append(base, loopback(45000, 6379, 201, 202)...),
		wantEdges: []TopoEdge{{Source: "api", Target: "redis", Port: 6379, Connections: 1}},
	}, {
		name: "postgres backend owns the accepted socket",
		procs: []procfs.Process{api(211),
			{PID: 30, Comm: "postgres", Kind: "postgres", Inodes: []uint64{110}, Ports: []uint16{5432}},
			{PID: 31, Comm: "postgres", Kind: "postgres", Inodes: []uint64{210}}},
		socks: append([]procfs.Socket{listen("0.0.0.0", 8080, 100), listen("0.0.0.0", 5432, 110)},
			append(loopback(46000, 5432, 211, 210), loopback(46001, 5432, 211, 210)...)...),
		wantNodes: []string{"api", "postgres"},
		wantEdges: []TopoEdge{{Source: "api", Target: "postgres", Port: 5432, Connections: 2}},
	}, {
		name:      "external peer",
		procs:     []procfs.Process{api(203), redis},
		socks:     append(base, est("10.0.0.5", 8080, "203.0.113.7", 51000, 203)),
		wantNodes: []string{"api", "ext:203.0.113.7", "redis"},
		wantEdges: []TopoEdge{{Source: "ext:203.0.113.7", Target: "api", Port: 8080, Connections: 1}},
	}, {
		name:  "no self edges",
		procs: []procfs.Process{api(212, 213), redis},
		socks: append(base, loopback(47000, 8080, 212, 213)...),
	}, {
		name:      "listener-only services are nodes without edges",
		procs:     []procfs.Process{api(), redis, {PID: 40, Comm: "lonely", Inodes: []uint64{120}, Ports: []uint16{9000}}},
		socks:     append(base, listen("0.0.0.0", 9000, 120)),
		wantNodes: []string{"api", "lonely", "redis"},
	}, {
		name:  "client socket whose ephemeral port equals a listener is ignored",
		procs: []procfs.Process{api(204), redis},
		socks: append(base, est("10.0.0.5", 6379, "192.0.2.1", 443, 204)),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildTopology(tc.procs, tc.socks)
			want := tc.wantNodes
			if want == nil {
				want = []string{"api", "redis"}
			}
			var ids []string
			for _, n := range got.Nodes {
				ids = append(ids, n.ID)
			}
			if !reflect.DeepEqual(ids, want) {
				t.Errorf("nodes = %v, want %v", ids, want)
			}
			if tc.wantEdges == nil {
				tc.wantEdges = []TopoEdge{}
			}
			if !reflect.DeepEqual(got.Edges, tc.wantEdges) {
				t.Errorf("edges = %+v, want %+v", got.Edges, tc.wantEdges)
			}
		})
	}

	t.Run("node details", func(t *testing.T) {
		got := BuildTopology([]procfs.Process{api(201), redis}, append(base, loopback(45000, 6379, 201, 202)...))
		if !reflect.DeepEqual(got.Nodes, []TopoNode{apiNode, redisNode}) {
			t.Errorf("nodes = %+v", got.Nodes)
		}
	})

	t.Run("external peers beyond the cap fold into ext:other", func(t *testing.T) {
		got := BuildTopology([]procfs.Process{api()}, append([]procfs.Socket{listen("0.0.0.0", 8080, 100)}, many...))
		ext := 0
		for _, n := range got.Nodes {
			if n.Kind == "external" && n.ID != extOther {
				ext++
			}
		}
		if ext != maxExternal {
			t.Errorf("%d external nodes, want %d", ext, maxExternal)
		}
		var other, kept int
		for _, e := range got.Edges {
			switch e.Source {
			case extOther:
				other = e.Connections
			case "ext:198.51.100.6":
				kept = e.Connections
			case "ext:198.51.100.5":
				t.Errorf("peer 5 (5 conns) should be folded: %+v", e)
			}
		}
		if other != 1+2+3+4+5 || kept != 6 {
			t.Errorf("ext:other = %d (want 15), peer 6 = %d (want 6)", other, kept)
		}
		if n := got.Nodes[len(got.Nodes)-1]; n.ID != extOther || n.Label != "other external peers" {
			t.Errorf("last node = %+v", n)
		}
	})
}
