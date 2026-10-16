package cluster

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestRingDistributionAndStability(t *testing.T) {
	r := NewRing(0)
	if _, err := r.Get(1); err != ErrEmptyRing {
		t.Fatal("expected ErrEmptyRing")
	}
	for i := 0; i < 5; i++ {
		r.Add(fmt.Sprintf("node-%d", i))
	}
	r.Add("node-0") // idempotent
	const keys = 50000
	counts := map[string]int{}
	before := make([]string, keys)
	for k := 0; k < keys; k++ {
		n, _ := r.Get(uint64(k))
		before[k] = n
		counts[n]++
	}
	exp := float64(keys) / 5
	for n, c := range counts {
		if math.Abs(float64(c)-exp)/exp > 0.25 {
			t.Fatalf("node %s imbalanced: %d (exp %.0f)", n, c, exp)
		}
	}
	r.Add("node-5")
	moved := 0
	for k := 0; k < keys; k++ {
		n, _ := r.Get(uint64(k))
		if n != before[k] {
			if n != "node-5" {
				t.Fatalf("key %d moved between existing nodes", k)
			}
			moved++
		}
	}
	if frac := float64(moved) / keys; frac < 0.08 || frac > 0.28 {
		t.Fatalf("moved fraction %.3f, expected ~1/6", frac)
	}
	r.Remove("node-5")
	for k := 0; k < keys; k++ {
		if n, _ := r.Get(uint64(k)); n != before[k] {
			t.Fatal("remove should restore original placement")
		}
	}
}

func TestGetN(t *testing.T) {
	r := NewRing(16)
	r.Add("a")
	r.Add("b")
	r.Add("c")
	ns, err := r.GetN(42, 5)
	if err != nil || len(ns) != 3 {
		t.Fatalf("got %v %v", ns, err)
	}
	seen := map[string]bool{}
	for _, n := range ns {
		if seen[n] {
			t.Fatal("duplicate replica")
		}
		seen[n] = true
	}
	first, _ := r.Get(42)
	if ns[0] != first {
		t.Fatal("primary replica must equal Get")
	}
	if got := r.Nodes(); len(got) != 3 || got[0] != "a" {
		t.Fatalf("nodes %v", got)
	}
}

func TestScatterGather(t *testing.T) {
	c := &Coordinator[int, int]{Client: ShardClientFunc[int, int](func(ctx context.Context, node string, req int) (int, error) {
		if node == "bad" {
			return 0, errors.New("boom")
		}
		return req * 2, nil
	}), MaxParallel: 2}
	sum := func(xs []int) int {
		s := 0
		for _, x := range xs {
			s += x
		}
		return s
	}
	got, err := c.ScatterGather(context.Background(), []string{"a", "b", "c"}, 5, sum)
	if err != nil || got != 30 {
		t.Fatalf("got %d %v", got, err)
	}
	if _, err := c.ScatterGather(context.Background(), []string{"a", "bad"}, 5, sum); err == nil {
		t.Fatal("expected error")
	}
	c.AllowPartial = true
	got, err = c.ScatterGather(context.Background(), []string{"a", "bad"}, 5, sum)
	var pe *PartialError
	if !errors.As(err, &pe) || got != 10 || len(pe.Failed) != 1 {
		t.Fatalf("partial: %d %v", got, err)
	}
}
