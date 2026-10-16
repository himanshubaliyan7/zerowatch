package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

// Roles that own a shard of the data. They mirror config.RoleStorage and
// config.RoleAll (duplicated here so the cluster package has no config import).
const (
	roleStorage = "storage"
	roleAll     = "all"
)

// OwnsData reports whether a node with this role holds series and logs, i.e.
// is placed on the hash ring.
func OwnsData(role string) bool { return role == roleStorage || role == roleAll }

// NodeMeta is gossiped with every member (memberlist node metadata, JSON).
type NodeMeta struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	HTTPAddr string `json:"httpAddr"` // as configured, e.g. ":8080"; host filled from the gossip address when empty
}

// Member is one node as seen by this process.
type Member struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"` // gossip ip:port
	Role     string `json:"role"`
	HTTPAddr string `json:"httpAddr"` // host:port other members dial for internal RPC
	State    string `json:"state"`    // alive | suspect | dead | left
}

// MembershipConfig configures gossip.
type MembershipConfig struct {
	Self     NodeMeta
	BindAddr string // e.g. 0.0.0.0
	BindPort int    // e.g. 7946
	Seeds    []string
}

// Membership tracks cluster members and keeps a Ring of the data-owning ones.
// It is fed either by memberlist (Join) or by a fixed list (NewStatic, used by
// tests and standalone mode), so routing code never depends on gossip.
type Membership struct {
	self      NodeMeta
	ring      *Ring
	log       *slog.Logger
	ml        *memberlist.Memberlist // nil when static
	done      chan struct{}          // closed by Leave; stops the seed re-join loop
	closeOnce sync.Once

	mu      sync.RWMutex
	members map[string]Member
}

// NewStatic builds a membership from a fixed member list. self is included
// automatically (with HTTPAddr as given).
func NewStatic(self NodeMeta, others []Member, log *slog.Logger) *Membership {
	m := &Membership{self: self, ring: NewRing(0), log: log, members: map[string]Member{}}
	m.upsert(Member{Name: self.Name, Role: self.Role, HTTPAddr: self.HTTPAddr, State: "alive"})
	for _, o := range others {
		if o.State == "" {
			o.State = "alive"
		}
		m.upsert(o)
	}
	return m
}

// Seed re-join backoff while the node is alone (see rejoin).
var (
	rejoinMin = 2 * time.Second
	rejoinMax = 30 * time.Second
)

// Join starts gossip on BindAddr:BindPort and joins the seeds (if any). A
// failed seed join is logged, not fatal: the node keeps running, retries the
// seeds in the background while it has no peers, and other members can still
// join it.
func Join(cfg MembershipConfig, log *slog.Logger) (*Membership, error) {
	m := &Membership{self: cfg.Self, ring: NewRing(0), log: log, members: map[string]Member{}, done: make(chan struct{})}
	meta, err := json.Marshal(cfg.Self)
	if err != nil {
		return nil, err
	}
	mc := memberlist.DefaultLANConfig()
	mc.Name = cfg.Self.Name
	if cfg.BindAddr != "" {
		mc.BindAddr = cfg.BindAddr
	}
	if cfg.BindPort > 0 {
		mc.BindPort = cfg.BindPort
		mc.AdvertisePort = cfg.BindPort
	}
	mc.Events = eventDelegate{m}
	mc.Delegate = metaDelegate{meta: meta}
	mc.LogOutput = slogWriter{log}
	ml, err := memberlist.Create(mc)
	if err != nil {
		return nil, fmt.Errorf("cluster: memberlist create: %w", err)
	}
	m.ml = ml
	if len(cfg.Seeds) > 0 {
		if n, err := ml.Join(cfg.Seeds); err != nil {
			log.Warn("cluster: join seeds failed; retrying in the background", "seeds", cfg.Seeds, "err", err)
		} else {
			log.Info("cluster: joined", "contacted", n, "members", ml.NumMembers())
		}
		go m.rejoin(cfg.Seeds, rejoinMin, rejoinMax)
	}
	return m, nil
}

// rejoin retries the seeds whenever this node has no peers: at start-up when
// the seeds' DNS names don't resolve yet (Kubernetes pods start before their
// headless-service records exist, and NXDOMAIN is cached), and after every
// peer has gone. Each attempt resolves the seed names again. The wait doubles
// from rejoinMin to rejoinMax and resets once peers are seen.
func (m *Membership) rejoin(seeds []string, lo, hi time.Duration) {
	wait := lo
	for {
		select {
		case <-m.done:
			return
		case <-time.After(wait):
		}
		if m.ml.NumMembers() > 1 {
			wait = lo
			continue
		}
		if n, err := m.ml.Join(seeds); err == nil && m.ml.NumMembers() > 1 {
			m.log.Info("cluster: joined", "contacted", n, "members", m.ml.NumMembers())
			wait = lo
			continue
		} else if err != nil {
			m.log.Debug("cluster: seed re-join failed", "seeds", seeds, "err", err)
		}
		wait = min(wait*2, hi)
	}
}

// Self returns this node's metadata.
func (m *Membership) Self() NodeMeta { return m.self }

// Ring returns the ring of data-owning members.
func (m *Membership) Ring() *Ring { return m.ring }

// Gossiping reports whether memberlist is running (false when static).
func (m *Membership) Gossiping() bool { return m.ml != nil }

// Members returns all known members sorted by name.
func (m *Membership) Members() []Member {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Member, 0, len(m.members))
	for _, mem := range m.members {
		out = append(out, mem)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns a member by name.
func (m *Membership) Lookup(name string) (Member, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mem, ok := m.members[name]
	return mem, ok
}

// HasIP reports whether ip belongs to a live member. It authorises internal
// RPC when no shared cluster secret is configured.
func (m *Membership) HasIP(ip string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mem := range m.members {
		if mem.State != "alive" && mem.State != "suspect" {
			continue
		}
		for _, a := range []string{mem.Addr, mem.HTTPAddr} {
			if h, _, err := net.SplitHostPort(a); err == nil && h == ip {
				return true
			}
		}
	}
	return false
}

func (m *Membership) upsert(mem Member) {
	m.mu.Lock()
	m.members[mem.Name] = mem
	m.mu.Unlock()
	if OwnsData(mem.Role) && (mem.State == "alive" || mem.State == "suspect") {
		m.ring.Add(mem.Name)
	} else {
		m.ring.Remove(mem.Name)
	}
}

func (m *Membership) remove(name string) {
	m.mu.Lock()
	delete(m.members, name)
	m.mu.Unlock()
	m.ring.Remove(name)
}

// Leave gracefully leaves the cluster (no-op when static).
func (m *Membership) Leave() error {
	if m.ml == nil {
		return nil
	}
	m.closeOnce.Do(func() { close(m.done) })
	if err := m.ml.Leave(0); err != nil {
		return err
	}
	return m.ml.Shutdown()
}

// memberFromNode converts a memberlist node. A meta HTTPAddr without a host
// (":8080") is completed with the node's gossip IP, which is how peers reach it.
func memberFromNode(n *memberlist.Node) Member {
	var meta NodeMeta
	_ = json.Unmarshal(n.Meta, &meta)
	ip := n.Addr.String()
	httpAddr := meta.HTTPAddr
	if host, port, err := net.SplitHostPort(httpAddr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		httpAddr = net.JoinHostPort(ip, port)
	}
	return Member{
		Name:     n.Name,
		Addr:     net.JoinHostPort(ip, strconv.Itoa(int(n.Port))),
		Role:     meta.Role,
		HTTPAddr: httpAddr,
		State:    stateName(n.State),
	}
}

func stateName(s memberlist.NodeStateType) string {
	switch s {
	case memberlist.StateAlive:
		return "alive"
	case memberlist.StateSuspect:
		return "suspect"
	case memberlist.StateDead:
		return "dead"
	case memberlist.StateLeft:
		return "left"
	}
	return "unknown"
}

type eventDelegate struct{ m *Membership }

func (d eventDelegate) NotifyJoin(n *memberlist.Node) {
	mem := memberFromNode(n)
	d.m.upsert(mem)
	d.m.log.Info("cluster member joined", "node", mem.Name, "role", mem.Role, "addr", mem.Addr, "http", mem.HTTPAddr)
}

func (d eventDelegate) NotifyLeave(n *memberlist.Node) {
	d.m.remove(n.Name)
	d.m.log.Warn("cluster member left", "node", n.Name)
}

func (d eventDelegate) NotifyUpdate(n *memberlist.Node) { d.m.upsert(memberFromNode(n)) }

type metaDelegate struct{ meta []byte }

func (m metaDelegate) NodeMeta(limit int) []byte {
	if len(m.meta) > limit {
		return m.meta[:limit]
	}
	return m.meta
}
func (metaDelegate) NotifyMsg([]byte)                           {}
func (metaDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (metaDelegate) LocalState(join bool) []byte                { return nil }
func (metaDelegate) MergeRemoteState(buf []byte, join bool)     {}

type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.l.Debug("memberlist", "msg", string(p))
	return len(p), nil
}
