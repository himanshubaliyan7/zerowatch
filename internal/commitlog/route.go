package commitlog

import "sync/atomic"

// RouteStats count what a ring-aware consumer did with the records it
// consumed (cluster mode: see cluster.ConsumeRouter). A consumer stores what
// its node owns, forwards the rest to the ring owner, and keeps a record
// locally only when no owner or replica accepted it.
type RouteStats struct {
	Owned         uint64 `json:"owned"`          // stored here: this node is the ring owner
	Forwarded     uint64 `json:"forwarded"`      // accepted by a peer (owner or replica)
	Failovers     uint64 `json:"failovers"`      // of Forwarded: accepted by a replica or on the retry round
	ForwardFailed uint64 `json:"forward_failed"` // failed forward attempts, counted per record and attempt
	KeptLocal     uint64 `json:"kept_local"`     // stored here although another node owns it
}

// routeStats holds an optional RouteStats source; Stats includes it when set.
type routeStats struct {
	fn atomic.Pointer[func() RouteStats]
}

// SetRouteStats attaches the ring-aware handler's counters, which Stats then
// reports under "routing". A nil fn detaches them.
func (r *routeStats) SetRouteStats(fn func() RouteStats) {
	if fn == nil {
		r.fn.Store(nil)
		return
	}
	r.fn.Store(&fn)
}

func (r *routeStats) get() *RouteStats {
	fn := r.fn.Load()
	if fn == nil {
		return nil
	}
	s := (*fn)()
	return &s
}
