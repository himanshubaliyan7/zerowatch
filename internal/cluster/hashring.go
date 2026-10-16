// Package cluster provides series sharding (consistent hashing with virtual
// nodes), gossip membership (hashicorp/memberlist) and a scatter-gather
// coordinator used by the query tier.
package cluster

import (
	"errors"
	"sort"
	"strconv"
	"sync"

	"github.com/zeebo/xxh3"
)

// ErrEmptyRing is returned when lookups happen against a ring with no nodes.
var ErrEmptyRing = errors.New("cluster: hash ring is empty")

// DefaultVNodes is the number of virtual nodes per physical node. 128 keeps the
// load standard deviation around ~5% for small clusters.
const DefaultVNodes = 128

// Ring is a thread-safe consistent hash ring. Each physical node is placed at
// VNodes positions (hash(node#i)). A key belongs to the first vnode clockwise
// from hash(key). Adding/removing a node only moves ~1/N of keys.
type Ring struct {
	mu     sync.RWMutex
	vnodes int
	points []uint64          // sorted vnode hashes
	owner  map[uint64]string // vnode hash -> node
	nodes  map[string]struct{}
}

// NewRing creates a ring; vnodes<=0 uses DefaultVNodes.
func NewRing(vnodes int) *Ring {
	if vnodes <= 0 {
		vnodes = DefaultVNodes
	}
	return &Ring{vnodes: vnodes, owner: map[uint64]string{}, nodes: map[string]struct{}{}}
}

func vhash(node string, i int) uint64 { return xxh3.HashString(node + "#" + strconv.Itoa(i)) }

// Add inserts a node (idempotent).
func (r *Ring) Add(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[node]; ok {
		return
	}
	r.nodes[node] = struct{}{}
	for i := 0; i < r.vnodes; i++ {
		h := vhash(node, i)
		if _, clash := r.owner[h]; clash {
			continue // astronomically rare; skip rather than steal
		}
		r.owner[h] = node
		r.points = append(r.points, h)
	}
	sort.Slice(r.points, func(a, b int) bool { return r.points[a] < r.points[b] })
}

// Remove deletes a node (idempotent).
func (r *Ring) Remove(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[node]; !ok {
		return
	}
	delete(r.nodes, node)
	kept := r.points[:0]
	for _, p := range r.points {
		if r.owner[p] == node {
			delete(r.owner, p)
			continue
		}
		kept = append(kept, p)
	}
	r.points = kept
}

// Nodes returns the sorted member list.
func (r *Ring) Nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.nodes))
	for n := range r.nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r *Ring) search(h uint64) int {
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i] >= h })
	if i == len(r.points) {
		i = 0
	}
	return i
}

// Get returns the owner of a series ID.
func (r *Ring) Get(key uint64) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.points) == 0 {
		return "", ErrEmptyRing
	}
	return r.owner[r.points[r.search(mixKey(key))]], nil
}

// GetN returns up to n distinct nodes for replication (walks clockwise).
func (r *Ring) GetN(key uint64, n int) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.points) == 0 {
		return nil, ErrEmptyRing
	}
	if n > len(r.nodes) {
		n = len(r.nodes)
	}
	out := make([]string, 0, n)
	seen := map[string]bool{}
	start := r.search(mixKey(key))
	for i := 0; len(out) < n && i < len(r.points); i++ {
		node := r.owner[r.points[(start+i)%len(r.points)]]
		if !seen[node] {
			seen[node] = true
			out = append(out, node)
		}
	}
	return out, nil
}

// mixKey runs a finalizer over the key so sequential IDs spread uniformly
// (series IDs are already XXH3 but callers may pass small integers).
func mixKey(k uint64) uint64 {
	k ^= k >> 33
	k *= 0xff51afd7ed558ccd
	k ^= k >> 33
	k *= 0xc4ceb9fe1a85ec53
	k ^= k >> 33
	return k
}
