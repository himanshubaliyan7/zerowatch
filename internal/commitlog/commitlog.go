// Package commitlog decouples ingestion from storage. Ingest gateways Publish
// validated records; a consumer drains them in micro-batches (every 100ms or
// 5000 records, whichever first) into the storage engine. The in-memory ring
// buffer is the embedded default; Kafka/Redpanda provides durability and
// fan-out in clustered deployments.
package commitlog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
)

// Defaults from the blueprint's micro-batching contract.
const (
	DefaultFlushInterval = 100 * time.Millisecond
	DefaultBatchSize     = 5000
)

// ErrBackpressure is returned when the buffer is full; gateways map it to 429/503.
var ErrBackpressure = errors.New("commitlog: buffer full")

// ErrClosed is returned after Close.
var ErrClosed = errors.New("commitlog: closed")

// Record is one ingested item. Exactly one of Metric or Log is set.
type Record struct {
	Metric *model.MetricDatum `json:"m,omitempty"`
	Log    *model.LogEvent    `json:"l,omitempty"`
}

// Producer publishes records.
type Producer interface {
	Publish(ctx context.Context, recs []Record) error
	Close() error
}

// Handler processes a micro-batch. Errors are logged by the consumer and the
// batch is retried once before being dropped (at-least-once within process).
type Handler func(ctx context.Context, batch []Record) error

// Consumer drains records into a handler until ctx is cancelled.
type Consumer interface {
	Run(ctx context.Context, h Handler) error
}

// Stats are exported for /metrics and the supervisor.
type Stats struct {
	Published uint64 `json:"published"`
	Delivered uint64 `json:"delivered"`
	Dropped   uint64 `json:"dropped"`
	Depth     int    `json:"depth"`
	Capacity  int    `json:"capacity"`
	// Routing is set when a ring-aware consumer is attached (SetRouteStats).
	Routing *RouteStats `json:"routing,omitempty"`
}

// Memory is a bounded ring buffer implementing Producer and Consumer.
type Memory struct {
	mu        sync.Mutex
	notEmpty  *sync.Cond
	buf       []Record
	head, n   int
	closed    bool
	interval  time.Duration
	batchSize int
	routeStats

	published, delivered, dropped atomic.Uint64
}

// NewMemory creates a ring buffer with the given capacity.
func NewMemory(capacity int) *Memory {
	if capacity <= 0 {
		capacity = 1 << 18
	}
	m := &Memory{buf: make([]Record, capacity), interval: DefaultFlushInterval, batchSize: DefaultBatchSize}
	m.notEmpty = sync.NewCond(&m.mu)
	return m
}

// Publish appends all records or none (returns ErrBackpressure).
func (m *Memory) Publish(_ context.Context, recs []Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.n+len(recs) > len(m.buf) {
		m.dropped.Add(uint64(len(recs)))
		return ErrBackpressure
	}
	for _, r := range recs {
		m.buf[(m.head+m.n)%len(m.buf)] = r
		m.n++
	}
	m.published.Add(uint64(len(recs)))
	m.notEmpty.Signal()
	return nil
}

// drain removes up to max records.
func (m *Memory) drain(max int) []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.n
	if k > max {
		k = max
	}
	out := make([]Record, k)
	for i := 0; i < k; i++ {
		idx := (m.head + i) % len(m.buf)
		out[i] = m.buf[idx]
		m.buf[idx] = Record{}
	}
	m.head = (m.head + k) % len(m.buf)
	m.n -= k
	return out
}

// Run flushes every interval, or immediately when batchSize is reached.
func (m *Memory) Run(ctx context.Context, h Handler) error {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	full := make(chan struct{}, 1)
	// watcher wakes the loop early when a full batch is available
	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for !m.closed && ctx.Err() == nil {
			if m.n >= m.batchSize {
				select {
				case full <- struct{}{}:
				default:
				}
			}
			m.notEmpty.Wait()
		}
	}()
	go func() { <-ctx.Done(); m.mu.Lock(); m.notEmpty.Broadcast(); m.mu.Unlock() }()
	flush := func(fctx context.Context) {
		for {
			batch := m.drain(m.batchSize)
			if len(batch) == 0 {
				return
			}
			if err := h(fctx, batch); err != nil {
				if err2 := h(fctx, batch); err2 != nil {
					m.dropped.Add(uint64(len(batch)))
					continue
				}
			}
			m.delivered.Add(uint64(len(batch)))
			if len(batch) < m.batchSize {
				return
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			// final drain with a short independent deadline
			dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			flush(dctx)
			cancel()
			return nil
		case <-t.C:
			flush(ctx)
		case <-full:
			flush(ctx)
		}
	}
}

// Close rejects further publishes.
func (m *Memory) Close() error {
	m.mu.Lock()
	m.closed = true
	m.notEmpty.Broadcast()
	m.mu.Unlock()
	return nil
}

// Stats returns counters.
func (m *Memory) Stats() Stats {
	m.mu.Lock()
	d := m.n
	m.mu.Unlock()
	return Stats{Published: m.published.Load(), Delivered: m.delivered.Load(), Dropped: m.dropped.Load(), Depth: d, Capacity: len(m.buf), Routing: m.routeStats.get()}
}
