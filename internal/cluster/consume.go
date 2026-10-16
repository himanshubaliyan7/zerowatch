package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zerowatch/zerowatch/internal/commitlog"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/storage"
)

// DefaultConsumeRetryDelay is the pause before ConsumeRouter's second routing
// round, long enough for gossip to mark a crashed peer suspect or dead.
const DefaultConsumeRetryDelay = 500 * time.Millisecond

// ConsumeRouter is the ring-aware commit-log handler of a data-owning node.
// With Kafka a storage node consumes whichever partitions the group gave it,
// so the records it reads belong anywhere on the ring. For each batch it:
//
//  1. looks up each record's owner and replica (metrics by series ID, logs by
//     LogStreamKey, the same keys Engine.WriteMetrics/WriteLogs use);
//  2. stores the records this node owns directly in its local engine;
//  3. sends the rest to their owners, one /internal/v1/write per peer, and
//     re-sends a failed peer's records to their replica;
//  4. waits RetryDelay and routes what is still undelivered once more over
//     the current ring (a dead peer has usually left it by then);
//  5. keeps whatever no peer accepted in its local engine rather than drop it.
//
// The handler returns nil only when every record was accepted by a peer or
// stored locally, so the commit log commits offsets only then. Forwards go to
// the internal write endpoint, which writes the receiving shard's engine and
// never the commit log, so a forwarded record is never re-published.
//
// Like Engine, each record is stored on one node (the replica only on
// failover): reads merge every shard, so a second copy would double count.
type ConsumeRouter struct {
	e     *Engine
	local storage.Engine
	log   *slog.Logger
	// RetryDelay is the pause before the second routing round
	// (DefaultConsumeRetryDelay when zero; negative skips the pause).
	RetryDelay time.Duration
	// OnLogs, when set, sees every consumed log event (live tail), wherever
	// it is stored.
	OnLogs func([]model.LogEvent)

	owned, forwarded, failovers, forwardFailed, keptLocal atomic.Uint64
}

// NewConsumeRouter builds the handler for an Engine whose node owns a shard.
func NewConsumeRouter(e *Engine, log *slog.Logger) (*ConsumeRouter, error) {
	if e == nil || e.local == nil {
		return nil, errors.New("cluster: consume router needs a data-owning node")
	}
	if log == nil {
		log = slog.Default()
	}
	return &ConsumeRouter{e: e, local: e.local, log: log}, nil
}

// Stats returns the routing counters (commitlog.Kafka.SetRouteStats takes this).
func (c *ConsumeRouter) Stats() commitlog.RouteStats {
	return commitlog.RouteStats{Owned: c.owned.Load(), Forwarded: c.forwarded.Load(), Failovers: c.failovers.Load(),
		ForwardFailed: c.forwardFailed.Load(), KeptLocal: c.keptLocal.Load()}
}

// Handle is a commitlog.Handler.
func (c *ConsumeRouter) Handle(ctx context.Context, recs []commitlog.Record) error {
	var ms []model.MetricDatum
	var ls []model.LogEvent
	for _, r := range recs {
		if r.Metric != nil {
			ms = append(ms, *r.Metric)
		}
		if r.Log != nil {
			ls = append(ls, *r.Log)
		}
	}
	if err := c.Write(ctx, ms, ls); err != nil {
		return err
	}
	if len(ls) > 0 && c.OnLogs != nil {
		c.OnLogs(ls)
	}
	return nil
}

var _ commitlog.Handler = (*ConsumeRouter)(nil).Handle

// Write places metrics and logs on their ring owners as described on
// ConsumeRouter. It fails only when the local engine refuses what must be
// stored here, or when ctx ends before every record was placed (the batch is
// then left uncommitted and redelivered; records a peer already accepted are
// duplicated by that redelivery, which at-least-once allows).
func (c *ConsumeRouter) Write(ctx context.Context, ms []model.MetricDatum, ls []model.LogEvent) error {
	n := len(ms) + len(ls)
	if n == 0 {
		return nil
	}
	keyOf := func(i int) uint64 {
		if i < len(ms) {
			return ms[i].ID()
		}
		return LogStreamKey(&ls[i-len(ms)])
	}
	build := func(idx []int) writeReq {
		var req writeReq
		for _, i := range idx {
			if i < len(ms) {
				req.Metrics = append(req.Metrics, ms[i])
			} else {
				req.Logs = append(req.Logs, ls[i-len(ms)])
			}
		}
		return req
	}
	self := c.e.mem.Self().Name

	var own, kept []int // owned here; kept here after the forwards failed
	pending := make([]int, n)
	for i := range pending {
		pending[i] = i
	}
	var lastErr error
	for round := 0; round < 2 && len(pending) > 0; round++ {
		if round == 1 {
			if err := c.pause(ctx); err != nil {
				return err
			}
		}
		ring := c.e.mem.Ring()
		primary := map[string][]int{}
		alt := map[int]string{}
		var noRing bool
		for _, i := range pending {
			owners, err := ring.GetN(keyOf(i), 2)
			if err != nil {
				noRing = true
				break
			}
			if owners[0] == self {
				own = append(own, i)
				continue
			}
			primary[owners[0]] = append(primary[owners[0]], i)
			if len(owners) > 1 {
				alt[i] = owners[1]
			}
		}
		if noRing {
			// This node is on the ring itself, so an empty ring is only a
			// transient membership state: keep everything here.
			lastErr = ErrNoStorage
			break
		}
		failed, err := c.sendAll(ctx, primary, round > 0, build)
		if err != nil {
			lastErr = err
		}
		retry := map[string][]int{}
		pending = pending[:0]
		for _, i := range failed {
			switch a := alt[i]; a {
			case "":
				pending = append(pending, i)
			case self:
				// The replica is this node: store it here as the failover copy.
				kept = append(kept, i)
			default:
				retry[a] = append(retry[a], i)
			}
		}
		still, err := c.sendAll(ctx, retry, true, build)
		if err != nil {
			lastErr = err
		}
		pending = append(pending, still...)
		if ctx.Err() != nil {
			return fmt.Errorf("cluster: consume interrupted with %d records unplaced: %w", len(pending), ctx.Err())
		}
	}
	if len(pending) > 0 {
		kept = append(kept, pending...)
		c.log.Warn("cluster: no owner or replica accepted consumed records; kept on this node", "records", len(pending), "err", lastErr)
	}
	if len(own)+len(kept) == 0 {
		return nil
	}
	if _, err := (shard{local: c.local}).write(ctx, build(append(own, kept...))); err != nil {
		return err
	}
	c.owned.Add(uint64(len(own)))
	c.keptLocal.Add(uint64(len(kept)))
	return nil
}

func (c *ConsumeRouter) pause(ctx context.Context) error {
	d := c.RetryDelay
	if d == 0 {
		d = DefaultConsumeRetryDelay
	}
	if d < 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sendAll sends each peer its records concurrently and returns the indexes
// of the groups that failed. failover marks deliveries counted as failovers.
func (c *ConsumeRouter) sendAll(ctx context.Context, groups map[string][]int, failover bool, build func([]int) writeReq) ([]int, error) {
	var (
		mu       sync.Mutex
		failed   []int
		firstErr error
		wg       sync.WaitGroup
	)
	for node, idx := range groups {
		wg.Add(1)
		go func(node string, idx []int) {
			defer wg.Done()
			var resp empty
			if err := c.e.client.call(ctx, node, pathWrite, build(idx), &resp); err != nil {
				c.forwardFailed.Add(uint64(len(idx)))
				mu.Lock()
				failed = append(failed, idx...)
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			c.forwarded.Add(uint64(len(idx)))
			if failover {
				c.failovers.Add(uint64(len(idx)))
			}
		}(node, idx)
	}
	wg.Wait()
	return failed, firstErr
}
