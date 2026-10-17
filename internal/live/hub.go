// Package live is the in-process fan-out hub behind Server-Sent Events
// (live tail, alarm transitions, supervisor events).
//
// Design: every subscriber owns a bounded channel. Publish never blocks — if a
// subscriber is too slow its event is dropped and its Dropped counter is
// incremented, so one stalled browser tab can never back-pressure ingest.
package live

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

// Event kinds.
const (
	KindLog        = "log"
	KindAlarm      = "alarm"
	KindSupervisor = "supervisor"
	KindMetric     = "metric"
)

// Event is one message delivered to subscribers.
type Event struct {
	Kind     string `json:"kind"`
	TenantID uint32 `json:"-"`
	Time     int64  `json:"time"`
	// Log is set for KindLog (used for server-side filtering).
	Log  *model.LogEvent `json:"log,omitempty"`
	Data any             `json:"data,omitempty"`
}

// Filter decides whether a subscriber wants an event. nil means everything.
type Filter func(*Event) bool

// Subscription is a handle returned by Subscribe.
type Subscription struct {
	C       <-chan Event
	ch      chan Event
	tenant  uint32
	filter  Filter
	dropped atomic.Uint64
	hub     *Hub
	once    sync.Once
}

// Dropped returns how many events this subscriber missed due to slowness.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close unsubscribes; safe to call multiple times.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, s)
		s.hub.mu.Unlock()
		close(s.ch)
	})
}

// Hub fans events out to subscribers.
type Hub struct {
	mu        sync.RWMutex
	subs      map[*Subscription]struct{}
	buf       int
	maxSubs   int
	published atomic.Uint64
	dropped   atomic.Uint64
}

// NewHub creates a hub. buffer is the per-subscriber channel size; maxSubs
// bounds concurrent subscribers (0 = 1000).
func NewHub(buffer, maxSubs int) *Hub {
	if buffer <= 0 {
		buffer = 256
	}
	if maxSubs <= 0 {
		maxSubs = 1000
	}
	return &Hub{subs: map[*Subscription]struct{}{}, buf: buffer, maxSubs: maxSubs}
}

// Subscribe registers a subscriber for a tenant. ok=false when the hub is full.
func (h *Hub) Subscribe(tenant uint32, f Filter) (*Subscription, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.maxSubs {
		return nil, false
	}
	ch := make(chan Event, h.buf)
	s := &Subscription{C: ch, ch: ch, tenant: tenant, filter: f, hub: h}
	h.subs[s] = struct{}{}
	return s, true
}

// Publish delivers e to every matching subscriber without blocking.
func (h *Hub) Publish(e Event) {
	if e.Time == 0 {
		e.Time = time.Now().UnixMilli()
	}
	h.published.Add(1)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.tenant != e.TenantID {
			continue
		}
		if s.filter != nil && !s.filter(&e) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.dropped.Add(1)
			h.dropped.Add(1)
		}
	}
}

// PublishLogs is a convenience for batches of log events.
func (h *Hub) PublishLogs(ls []model.LogEvent) {
	if h.Subscribers() == 0 {
		return
	}
	for i := range ls {
		l := ls[i]
		h.Publish(Event{Kind: KindLog, TenantID: l.TenantID, Time: l.Timestamp, Log: &l})
	}
}

// Subscribers returns the current subscriber count.
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Stats for /api/v1/stats.
type Stats struct {
	Subscribers int    `json:"subscribers"`
	Published   uint64 `json:"published"`
	Dropped     uint64 `json:"dropped"`
}

// Stats returns counters.
func (h *Hub) Stats() Stats {
	return Stats{Subscribers: h.Subscribers(), Published: h.published.Load(), Dropped: h.dropped.Load()}
}
