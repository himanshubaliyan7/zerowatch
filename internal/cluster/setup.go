package cluster

import (
	"log/slog"
	"net/http"

	"github.com/zerowatch/zerowatch/internal/commitlog"
	"github.com/zerowatch/zerowatch/internal/httpx"
	"github.com/zerowatch/zerowatch/internal/storage"
	"github.com/zerowatch/zerowatch/internal/traces"
)

// Options configure a node (from config.Config plus ZW_CLUSTER_SECRET).
type Options struct {
	NodeName   string
	Role       string // all | ingest | query | storage | supervisor
	HTTPAddr   string // the node's UI/API listen address, advertised to peers
	GossipPort int
	Seeds      []string
	Secret     string // shared secret for /internal/v1/*; empty = members-only by IP
}

// Clustered reports whether a node joins a cluster: whenever seeds are given,
// and for the split roles (a storage node may be the first seed itself).
// role=all and role=supervisor without seeds stay standalone, which keeps the
// single-binary mode free of any gossip or internal RPC.
func (o Options) Clustered() bool {
	if len(o.Seeds) > 0 {
		return true
	}
	switch o.Role {
	case "ingest", "query", roleStorage:
		return true
	}
	return false
}

// Node is the cluster wiring for one process.
type Node struct {
	Membership *Membership
	// Engine is the routing/scatter-gather engine; nil when standalone.
	Engine *Engine
	// Store is what the rest of the process should use as its storage.Engine:
	// the local engine when standalone or role=storage (a storage node serves
	// only its own shard), the cluster Engine otherwise.
	Store storage.Engine
	// Internal serves /internal/v1/* on data-owning clustered nodes; nil otherwise.
	Internal http.Handler
	// Traces is what the process should use for spans: the local store when
	// standalone or role=storage, a TraceRouter (trace on its ring owner) otherwise.
	Traces traces.Backend
	// Consume is the ring-aware commit-log handler on clustered data-owning
	// nodes (Consume.Handle); nil otherwise, where store writes are enough.
	Consume *ConsumeRouter
}

// Start wires the cluster for this process. local is the process's own
// storage engine and localTraces its own trace store.
func Start(o Options, local storage.Engine, localTraces *traces.Store, log *slog.Logger) (*Node, error) {
	self := NodeMeta{Name: o.NodeName, Role: o.Role, HTTPAddr: o.HTTPAddr}
	if !o.Clustered() {
		return &Node{Membership: NewStatic(self, nil, log), Store: local, Traces: localTraces}, nil
	}
	mem, err := Join(MembershipConfig{Self: self, BindAddr: "0.0.0.0", BindPort: o.GossipPort, Seeds: o.Seeds}, log)
	if err != nil {
		return nil, err
	}
	if o.Secret == "" {
		log.Warn("cluster: ZW_CLUSTER_SECRET not set; internal RPC accepts any member IP")
	}
	n := &Node{Membership: mem}
	var shardLocal storage.Engine
	var shardTraces *traces.Store
	if OwnsData(o.Role) {
		shardLocal, shardTraces = local, localTraces
		n.Internal = ShardHandler(local, localTraces, mem, o.Secret, log)
	}
	n.Engine = NewEngine(mem, shardLocal, o.Secret, log)
	n.Engine.traces = shardTraces
	n.Store, n.Traces = n.Engine, &TraceRouter{e: n.Engine}
	if shardLocal != nil {
		n.Consume, _ = NewConsumeRouter(n.Engine, log)
	}
	if o.Role == roleStorage {
		n.Store, n.Traces = local, localTraces
	}
	log.Info("cluster mode", "node", o.NodeName, "role", o.Role, "gossipPort", o.GossipPort, "seeds", o.Seeds)
	return n, nil
}

// Close leaves the gossip cluster.
func (n *Node) Close() error { return n.Membership.Leave() }

// Status is the body of GET /api/v1/cluster.
type Status struct {
	Self      NodeMeta      `json:"self"`
	Clustered bool          `json:"clustered"`
	Members   []Member      `json:"members"`
	Ring      RingStatus    `json:"ring"`
	Routing   *RoutingStats `json:"routing,omitempty"`
	// Consume counts the ring-aware commit-log handler's placements.
	Consume *commitlog.RouteStats `json:"consume,omitempty"`
}

// RingStatus lists the data-owning nodes.
type RingStatus struct {
	Nodes []string `json:"nodes"`
}

// Status reports membership, ring and routing counters. Standalone nodes
// report a single-member cluster.
func (n *Node) Status() Status {
	s := Status{Self: n.Membership.Self(), Clustered: n.Membership.Gossiping(), Members: n.Membership.Members(),
		Ring: RingStatus{Nodes: n.Membership.Ring().Nodes()}}
	if n.Engine != nil {
		rs := n.Engine.RoutingStats()
		s.Routing = &rs
	}
	if n.Consume != nil {
		cs := n.Consume.Stats()
		s.Consume = &cs
	}
	return s
}

// StatusHandler serves GET /api/v1/cluster.
func (n *Node) StatusHandler(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, n.Status())
}
