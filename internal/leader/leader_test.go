package leader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memLeases is an in-memory Leases with injectable failures.
type memLeases struct {
	mu      sync.Mutex
	holder  string
	expires time.Time
	fail    atomic.Bool
}

func (m *memLeases) AcquireLease(_ context.Context, _, holder string, ttl time.Duration) (bool, error) {
	if m.fail.Load() {
		return false, errors.New("db down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.holder != "" && m.holder != holder && now.Before(m.expires) {
		return false, nil
	}
	m.holder, m.expires = holder, now.Add(ttl)
	return true, nil
}

func (m *memLeases) ReleaseLease(_ context.Context, _, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holder == holder {
		m.holder = ""
	}
	return nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func elector(l Leases, node string) *Elector {
	return &Elector{Leases: l, Name: "alarms", Holder: NewHolder(node), TTL: 60 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// runner counts lead invocations and how many are running right now.
type runner struct{ running, terms atomic.Int32 }

func (r *runner) lead(ctx context.Context) {
	r.terms.Add(1)
	r.running.Add(1)
	<-ctx.Done()
	r.running.Add(-1)
}

func TestSingleLeaderAndFailover(t *testing.T) {
	l := &memLeases{}
	a, b := elector(l, "a"), elector(l, "b")
	var ra, rb runner
	actx, acancel := context.WithCancel(context.Background())
	bctx, bcancel := context.WithCancel(context.Background())
	defer bcancel()
	aDone := make(chan struct{})
	go func() { a.Run(actx, ra.lead); close(aDone) }()
	eventually(t, "a leads", a.Leading)
	go b.Run(bctx, rb.lead)

	time.Sleep(200 * time.Millisecond) // several renew rounds
	if b.Leading() || rb.terms.Load() != 0 || ra.running.Load() != 1 {
		t.Fatalf("want only a leading: a=%v b=%v rbTerms=%d", a.Leading(), b.Leading(), rb.terms.Load())
	}

	acancel() // graceful shutdown releases the lease
	<-aDone
	if ra.running.Load() != 0 || a.Leading() {
		t.Fatal("a's lead func must have returned")
	}
	eventually(t, "b takes over", b.Leading)
	if rb.running.Load() != 1 {
		t.Fatal("b's lead func should be running")
	}
}

func TestStepsDownWhenRenewalsFail(t *testing.T) {
	l := &memLeases{}
	a := elector(l, "a")
	var ra runner
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, ra.lead)
	eventually(t, "a leads", a.Leading)

	l.fail.Store(true)
	eventually(t, "a steps down", func() bool { return !a.Leading() && ra.running.Load() == 0 })

	l.fail.Store(false)
	eventually(t, "a leads again", a.Leading)
	if ra.terms.Load() != 2 {
		t.Fatalf("terms = %d, want 2", ra.terms.Load())
	}
}

func TestStepsDownWhenLeaseStolen(t *testing.T) {
	l := &memLeases{}
	a := elector(l, "a")
	var ra runner
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, ra.lead)
	eventually(t, "a leads", a.Leading)

	l.mu.Lock()
	l.holder, l.expires = "intruder", time.Now().Add(time.Hour)
	l.mu.Unlock()
	eventually(t, "a steps down", func() bool { return !a.Leading() && ra.running.Load() == 0 })
}
