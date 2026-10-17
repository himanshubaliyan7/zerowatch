package commitlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

func rec(i int) Record {
	return Record{Metric: &model.MetricDatum{Namespace: "n", MetricName: "m", Timestamp: int64(i), Value: model.Float(float64(i))}}
}

func TestMemoryBatchingAndOrder(t *testing.T) {
	m := NewMemory(20000)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var got []int64
	var batches []int
	done := make(chan struct{})
	go func() {
		_ = m.Run(ctx, func(_ context.Context, b []Record) error {
			mu.Lock()
			defer mu.Unlock()
			batches = append(batches, len(b))
			for _, r := range b {
				got = append(got, r.Metric.Timestamp)
			}
			return nil
		})
		close(done)
	}()
	recs := make([]Record, 12000)
	for i := range recs {
		recs[i] = rec(i)
	}
	if err := m.Publish(ctx, recs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 12000 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if len(got) != 12000 {
		t.Fatalf("delivered %d", len(got))
	}
	for i, ts := range got {
		if ts != int64(i) {
			t.Fatalf("out of order at %d", i)
		}
	}
	for _, b := range batches {
		if b > DefaultBatchSize {
			t.Fatalf("batch too large %d", b)
		}
	}
	if s := m.Stats(); s.Delivered != 12000 || s.Depth != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestMemoryBackpressure(t *testing.T) {
	m := NewMemory(10)
	if err := m.Publish(context.Background(), make([]Record, 11)); err != ErrBackpressure {
		t.Fatalf("want backpressure got %v", err)
	}
	if err := m.Publish(context.Background(), make([]Record, 10)); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	if err := m.Publish(context.Background(), make([]Record, 1)); err != ErrClosed {
		t.Fatal("want closed")
	}
}
