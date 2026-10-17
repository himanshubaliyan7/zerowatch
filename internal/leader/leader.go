// Package leader runs singleton jobs (the alarm engine, scheduled reports) on
// exactly one query replica, using a lease row in the shared metadata DB.
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync/atomic"
	"time"
)

// Leases is the lease store (implemented by metadata.SQLStore).
type Leases interface {
	AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
}

// Elector campaigns for one named lease. Every TTL/3 it renews (or tries to
// take) the lease; while it holds it, the lead function runs.
type Elector struct {
	Leases Leases
	Name   string // lease name, e.g. "alarms"
	Holder string // unique per process; see NewHolder
	TTL    time.Duration
	Log    *slog.Logger

	leading atomic.Bool
}

// NewHolder returns a holder id: the node name plus a random suffix, so two
// replicas misconfigured with the same node name still can't both lead.
func NewHolder(node string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return node + "/" + hex.EncodeToString(b)
}

// Leading reports whether this process currently holds the lease.
func (e *Elector) Leading() bool { return e.leading.Load() }

// Run campaigns until ctx ends. Each time the lease is won, lead runs with a
// context that is cancelled when the lease is lost, or when renewals keep
// failing long enough that it may have lapsed. Run waits for lead to return
// before campaigning again, and releases the lease on shutdown.
func (e *Elector) Run(ctx context.Context, lead func(ctx context.Context)) {
	renew := e.TTL / 3
	var (
		cancel context.CancelFunc
		done   chan struct{}
		lastOK time.Time
	)
	stop := func(why string) {
		if cancel == nil {
			return
		}
		cancel()
		<-done
		cancel = nil
		e.leading.Store(false)
		e.Log.Warn("leadership ended", "lease", e.Name, "holder", e.Holder, "reason", why)
	}
	step := func() {
		ok, err := e.Leases.AcquireLease(ctx, e.Name, e.Holder, e.TTL)
		now := time.Now()
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			e.Log.Warn("lease renew failed", "lease", e.Name, "err", err)
			// Step down before the last lease written could expire, leaving
			// one renew interval as margin for clock skew.
			if cancel != nil && now.Sub(lastOK) >= e.TTL-renew {
				stop("renewals failing")
			}
		case ok:
			lastOK = now
			if cancel == nil {
				var lctx context.Context
				lctx, cancel = context.WithCancel(ctx)
				done = make(chan struct{})
				e.leading.Store(true)
				e.Log.Info("became leader", "lease", e.Name, "holder", e.Holder)
				go func() {
					defer close(done)
					lead(lctx)
				}()
			}
		default:
			stop("lease held by another replica")
		}
	}

	t := time.NewTicker(renew)
	defer t.Stop()
	step()
	for {
		select {
		case <-ctx.Done():
			wasLeading := cancel != nil
			stop("shutdown")
			if wasLeading {
				rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
				if err := e.Leases.ReleaseLease(rctx, e.Name, e.Holder); err != nil {
					e.Log.Warn("lease release failed", "lease", e.Name, "err", err)
				}
				rcancel()
			}
			return
		case <-t.C:
			step()
		}
	}
}
