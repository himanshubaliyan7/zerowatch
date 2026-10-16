package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ShardClient executes a request against a single storage node.
type ShardClient[Req, Resp any] interface {
	Do(ctx context.Context, node string, req Req) (Resp, error)
}

// ShardClientFunc adapts a function to ShardClient.
type ShardClientFunc[Req, Resp any] func(ctx context.Context, node string, req Req) (Resp, error)

// Do implements ShardClient.
func (f ShardClientFunc[Req, Resp]) Do(ctx context.Context, node string, req Req) (Resp, error) {
	return f(ctx, node, req)
}

// PartialError reports per-node failures when AllowPartial is set.
type PartialError struct{ Failed map[string]error }

func (e *PartialError) Error() string {
	return fmt.Sprintf("cluster: %d shard(s) failed", len(e.Failed))
}

// Coordinator fans a request out to many nodes and merges results.
type Coordinator[Req, Resp any] struct {
	Client       ShardClient[Req, Resp]
	Timeout      time.Duration // per-shard timeout; 0 => 10s
	MaxParallel  int           // 0 => unlimited
	AllowPartial bool          // return merged partial results + *PartialError
}

// ScatterGather calls Client.Do on every node concurrently and merges successful
// responses with merge. Without AllowPartial, the first error cancels the rest.
func (c *Coordinator[Req, Resp]) ScatterGather(ctx context.Context, nodes []string, req Req, merge func([]Resp) Resp) (Resp, error) {
	var zero Resp
	if len(nodes) == 0 {
		return zero, errors.New("cluster: no nodes to query")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sem chan struct{}
	if c.MaxParallel > 0 {
		sem = make(chan struct{}, c.MaxParallel)
	}
	type res struct {
		node string
		r    Resp
		err  error
	}
	out := make(chan res, len(nodes))
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					out <- res{node: n, err: ctx.Err()}
					return
				}
			}
			sctx, scancel := context.WithTimeout(ctx, timeout)
			defer scancel()
			r, err := c.Client.Do(sctx, n, req)
			out <- res{n, r, err}
		}(n)
	}
	go func() { wg.Wait(); close(out) }()
	var ok []Resp
	failed := map[string]error{}
	for r := range out {
		if r.err != nil {
			failed[r.node] = r.err
			if !c.AllowPartial {
				cancel()
			}
			continue
		}
		ok = append(ok, r.r)
	}
	if len(failed) > 0 && !c.AllowPartial {
		for n, e := range failed {
			return zero, fmt.Errorf("cluster: shard %s: %w", n, e)
		}
	}
	merged := merge(ok)
	if len(failed) > 0 {
		return merged, &PartialError{Failed: failed}
	}
	return merged, nil
}
