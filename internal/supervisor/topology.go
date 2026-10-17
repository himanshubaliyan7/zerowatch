package supervisor

import (
	"net"
	"sort"

	"github.com/himanshubaliyan7/zerowatch/internal/procfs"
)

// TopoNode is a service (a local process with listening ports) or an
// external peer seen on inbound connections.
type TopoNode struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Kind  string   `json:"kind"` // "service" | "external"
	PID   int      `json:"pid,omitempty"`
	Ports []uint16 `json:"ports,omitempty"`
}

// TopoEdge aggregates established connections from Source to Target's Port.
type TopoEdge struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Port        uint16 `json:"port"`
	Connections int    `json:"connections"`
}

// Topology is the who-talks-to-whom graph for this host.
type Topology struct {
	Nodes []TopoNode `json:"nodes"`
	Edges []TopoEdge `json:"edges"`
}

// maxExternal caps distinct external nodes; the rest fold into extOther.
const (
	maxExternal = 20
	extOther    = "ext:other"
)

// serviceID is the name the probe reports as the RED "Service" dimension.
func serviceID(p procfs.Process) string {
	if p.Kind != "" {
		return p.Kind
	}
	return p.Comm
}

// BuildTopology derives service-to-service edges from one /proc snapshot.
// procs must carry Inodes and Ports (see procfs.AttachPorts). Only the
// server side of each connection is counted (LocalPort is a listening
// port), so loopback connections, which appear twice, count once.
func BuildTopology(procs []procfs.Process, socks []procfs.Socket) Topology {
	nodes := map[string]*TopoNode{}
	portSvc := map[uint16]string{} // listening port -> service id
	owner := map[uint64]int{}      // socket inode -> index into procs
	for i, p := range procs {
		for _, ino := range p.Inodes {
			owner[ino] = i
		}
		if len(p.Ports) == 0 {
			continue
		}
		id := serviceID(p)
		n := nodes[id]
		if n == nil {
			n = &TopoNode{ID: id, Label: id, Kind: "service", PID: p.PID}
			nodes[id] = n
		}
		n.PID = min(n.PID, p.PID)
		for _, port := range p.Ports {
			n.Ports = appendUnique(n.Ports, port)
			if cur, ok := portSvc[port]; !ok || id < cur {
				portSvc[port] = id
			}
		}
	}

	// Local addresses of this host, and established sockets by port pair so
	// the client end of a loopback connection can be found.
	local := map[string]bool{}
	type pair struct{ l, r uint16 }
	byPair := map[pair][]procfs.Socket{}
	for _, s := range socks {
		if s.LocalIP != nil && !s.LocalIP.IsUnspecified() {
			local[s.LocalIP.String()] = true
		}
		if s.State == procfs.TCPEstablished {
			byPair[pair{s.LocalPort, s.RemotePort}] = append(byPair[pair{s.LocalPort, s.RemotePort}], s)
		}
	}
	isLocal := func(ip net.IP) bool { return ip.IsLoopback() || local[ip.String()] }
	// svcOf names the socket's owner. Workers that hold accepted sockets but
	// not the listener (postgres backends) share their service's name.
	svcOf := func(ino uint64) (string, bool) {
		i, ok := owner[ino]
		if !ok {
			return "", false
		}
		return serviceID(procs[i]), true
	}

	type key struct {
		src, dst string
		port     uint16
	}
	conns := map[key]int{}
	for _, s := range socks {
		if s.State != procfs.TCPEstablished || s.RemoteIP == nil {
			continue
		}
		dst, ok := portSvc[s.LocalPort]
		if !ok {
			continue
		}
		// A known owner that is not dst means this is a client socket whose
		// ephemeral port happens to equal a listening port.
		if id, known := svcOf(s.Inode); known && id != dst {
			continue
		}
		src := "ext:" + s.RemoteIP.String()
		if isLocal(s.RemoteIP) {
			for _, c := range byPair[pair{s.RemotePort, s.LocalPort}] {
				if c.LocalIP.Equal(s.RemoteIP) && c.RemoteIP.Equal(s.LocalIP) {
					if id, _ := svcOf(c.Inode); nodes[id] != nil {
						src = id
					}
					break
				}
			}
		}
		if src != dst {
			conns[key{src, dst, s.LocalPort}]++
		}
	}

	// Keep the busiest external peers, fold the long tail into one node.
	extTotal := map[string]int{}
	for k, n := range conns {
		if nodes[k.src] == nil {
			extTotal[k.src] += n
		}
	}
	keep := map[string]bool{}
	if len(extTotal) > maxExternal {
		ids := make([]string, 0, len(extTotal))
		for id := range extTotal {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if extTotal[ids[i]] != extTotal[ids[j]] {
				return extTotal[ids[i]] > extTotal[ids[j]]
			}
			return ids[i] < ids[j]
		})
		for _, id := range ids[:maxExternal] {
			keep[id] = true
		}
	} else {
		for id := range extTotal {
			keep[id] = true
		}
	}
	folded := map[key]int{}
	for k, n := range conns {
		if nodes[k.src] == nil {
			if !keep[k.src] {
				k.src = extOther
			}
			if nodes[k.src] == nil {
				label := k.src[len("ext:"):]
				if k.src == extOther {
					label = "other external peers"
				}
				nodes[k.src] = &TopoNode{ID: k.src, Label: label, Kind: "external"}
			}
		}
		folded[k] += n
	}

	t := Topology{Nodes: make([]TopoNode, 0, len(nodes)), Edges: make([]TopoEdge, 0, len(folded))}
	for _, n := range nodes {
		sort.Slice(n.Ports, func(i, j int) bool { return n.Ports[i] < n.Ports[j] })
		t.Nodes = append(t.Nodes, *n)
	}
	for k, n := range folded {
		t.Edges = append(t.Edges, TopoEdge{Source: k.src, Target: k.dst, Port: k.port, Connections: n})
	}
	sort.Slice(t.Nodes, func(i, j int) bool { return t.Nodes[i].ID < t.Nodes[j].ID })
	sort.Slice(t.Edges, func(i, j int) bool {
		a, b := t.Edges[i], t.Edges[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Port < b.Port
	})
	return t
}

func appendUnique(s []uint16, v uint16) []uint16 {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
