//go:build integration

package commitlog

// Live Kafka/Redpanda tests. Run with
//
//	ZW_TEST_KAFKA_BROKERS=localhost:19092 go test -tags integration ./internal/commitlog
//
// (deploy/compose/README.md). Each test uses its own topic and group.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
)

func kafkaTestConfig(t *testing.T) KafkaConfig {
	t.Helper()
	b := os.Getenv("ZW_TEST_KAFKA_BROKERS")
	if b == "" {
		t.Skip("ZW_TEST_KAFKA_BROKERS not set")
	}
	id := fmt.Sprintf("zw-it-%s-%d", strings.NewReplacer("/", ".", "=", "-").Replace(strings.ToLower(t.Name())), time.Now().UnixNano())
	return KafkaConfig{Brokers: strings.Split(b, ","), Topic: id, Group: id + "-g",
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}
}

func newKafka(t *testing.T, cfg KafkaConfig) *Kafka {
	t.Helper()
	k, err := NewKafkaConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k
}

func metricRec(i int) Record {
	return Record{Metric: &model.MetricDatum{TenantID: 1, Namespace: "App", MetricName: "Latency",
		Dimensions: []model.Dimension{{Name: "Service", Value: fmt.Sprintf("svc%d", i%3)}},
		Timestamp:  1_700_000_000_000 + int64(i), Value: model.Float(float64(i) + 0.25), Unit: "Milliseconds"}}
}

func logRec(i int) Record {
	return Record{Log: &model.LogEvent{TenantID: 1, LogGroup: "app", LogStream: fmt.Sprintf("s%d", i%2),
		Timestamp: 1_700_000_000_000 + int64(i), Message: fmt.Sprintf("line %d \"quoted\" ünï", i), Level: "INFO",
		Attributes: map[string]string{"i": fmt.Sprint(i)}}}
}

// collector gathers delivered records; wait blocks until n have arrived.
type collector struct {
	mu   sync.Mutex
	recs []Record
	ch   chan struct{}
}

func newCollector() *collector { return &collector{ch: make(chan struct{}, 1)} }

func (c *collector) handle(_ context.Context, b []Record) error {
	c.mu.Lock()
	c.recs = append(c.recs, b...)
	c.mu.Unlock()
	select {
	case c.ch <- struct{}{}:
	default:
	}
	return nil
}

func (c *collector) wait(t *testing.T, n int, d time.Duration) []Record {
	t.Helper()
	deadline := time.After(d)
	for {
		c.mu.Lock()
		got := len(c.recs)
		c.mu.Unlock()
		if got >= n {
			c.mu.Lock()
			defer c.mu.Unlock()
			return append([]Record(nil), c.recs...)
		}
		select {
		case <-c.ch:
		case <-deadline:
			t.Fatalf("timed out: %d of %d records delivered", got, n)
		}
	}
}

// run starts Run in the background and returns a stop function that cancels
// it and waits for it to return.
func run(t *testing.T, k *Kafka, h Handler) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Run(ctx, h) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func TestKafkaRoundTrip(t *testing.T) {
	cfg := kafkaTestConfig(t)
	k := newKafka(t, cfg)
	ctx := context.Background()
	var want []Record
	for i := 0; i < 3000; i++ {
		if i%3 == 0 {
			want = append(want, logRec(i))
		} else {
			want = append(want, metricRec(i))
		}
	}
	for i := 0; i < len(want); i += 500 { // gateway-sized batches
		if err := k.Publish(ctx, want[i:i+500]); err != nil {
			t.Fatal(err)
		}
	}
	c := newCollector()
	stop := run(t, k, c.handle)
	got := c.wait(t, len(want), 60*time.Second)
	stop()
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d (duplicates?)", len(got), len(want))
	}
	// One partition: global order is preserved, and every field round-trips.
	if !reflect.DeepEqual(got, want) {
		for i := range want {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Fatalf("record %d differs:\n got  %+v %+v\n want %+v %+v", i, got[i].Metric, got[i].Log, want[i].Metric, want[i].Log)
			}
		}
	}
	if st := k.Stats(); st.Published != 3000 || st.Delivered != 3000 || st.Dropped != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestKafkaCommitAndResume(t *testing.T) {
	cfg := kafkaTestConfig(t)
	ctx := context.Background()
	var all []Record
	for i := 0; i < 150; i++ {
		all = append(all, metricRec(i))
	}

	a := newKafka(t, cfg)
	if err := a.Publish(ctx, all[:100]); err != nil {
		t.Fatal(err)
	}
	ca := newCollector()
	stop := run(t, a, ca.handle)
	if got := ca.wait(t, 100, 60*time.Second); !reflect.DeepEqual(got, all[:100]) {
		t.Fatalf("first consumer got %d records", len(got))
	}
	stop()
	_ = a.Close()

	// Records published while no consumer runs; a restarted consumer in the
	// same group resumes from the committed offset.
	p := newKafka(t, KafkaConfig{Brokers: cfg.Brokers, Topic: cfg.Topic, Group: cfg.Group + "-producer-only"})
	if err := p.Publish(ctx, all[100:]); err != nil {
		t.Fatal(err)
	}
	b := newKafka(t, cfg)
	cb := newCollector()
	stop = run(t, b, cb.handle)
	got := cb.wait(t, 50, 60*time.Second)
	time.Sleep(time.Second) // anything redelivered would arrive by now
	got = cb.wait(t, 50, time.Second)
	stop()
	if !reflect.DeepEqual(got, all[100:]) {
		t.Fatalf("resumed consumer got %d records starting at ts %d, want the 50 uncommitted ones", len(got), got[0].Metric.Timestamp)
	}
}

// Shutdown that begins while a batch is being handled: a batch the handler
// accepted is still committed (no duplicates on restart); one the handler
// failed because of the shutdown is not (it is redelivered).
func TestKafkaShutdownDuringHandler(t *testing.T) {
	for _, accept := range []bool{true, false} {
		t.Run(fmt.Sprintf("accept=%v", accept), func(t *testing.T) {
			cfg := kafkaTestConfig(t)
			ctx := context.Background()
			recs := []Record{metricRec(1), metricRec(2), metricRec(3)}
			a := newKafka(t, cfg)
			if err := a.Publish(ctx, recs); err != nil {
				t.Fatal(err)
			}
			rctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				_ = a.Run(rctx, func(hctx context.Context, b []Record) error {
					cancel() // SIGTERM arrives mid-batch
					if !accept {
						return hctx.Err()
					}
					return nil
				})
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("Run did not return")
			}
			_ = a.Close()

			b := newKafka(t, cfg)
			if err := b.Publish(ctx, []Record{metricRec(4)}); err != nil {
				t.Fatal(err)
			}
			c := newCollector()
			stop := run(t, b, c.handle)
			want := 1
			if !accept {
				want = 4
			}
			c.wait(t, want, 60*time.Second)
			time.Sleep(time.Second)
			got := c.wait(t, want, time.Second)
			stop()
			if len(got) != want {
				t.Fatalf("restarted consumer got %d records, want %d", len(got), want)
			}
		})
	}
}

// A batch handled but not committed (the process dies first) is redelivered.
func TestKafkaRedeliversUncommitted(t *testing.T) {
	cfg := kafkaTestConfig(t)
	ctx := context.Background()
	var recs []Record
	for i := 0; i < 20; i++ {
		recs = append(recs, logRec(i))
	}
	a := newKafka(t, cfg)
	if err := a.Publish(ctx, recs); err != nil {
		t.Fatal(err)
	}
	inHandler, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	runDone := make(chan struct{})
	go func() {
		_ = a.Run(context.Background(), func(context.Context, []Record) error {
			once.Do(func() { close(inHandler) })
			<-release
			return nil
		})
		close(runDone)
	}()
	select {
	case <-inHandler:
	case <-time.After(60 * time.Second):
		t.Fatal("handler never called")
	}
	closed := make(chan struct{})
	go func() { _ = a.Close(); close(closed) }() // "crash": client gone before the commit
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("Close blocked while a batch was in flight")
	}
	close(release)
	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after Close")
	}

	b := newKafka(t, cfg)
	c := newCollector()
	stop := run(t, b, c.handle)
	got := c.wait(t, 20, 60*time.Second)
	stop()
	if !reflect.DeepEqual(got[:20], recs) {
		t.Fatalf("redelivery: got %d records", len(got))
	}
}

// A failing handler is retried once; a batch failing twice is dropped and
// committed (Memory's semantics), so it is not redelivered.
func TestKafkaHandlerRetryAndDrop(t *testing.T) {
	cfg := kafkaTestConfig(t)
	ctx := context.Background()
	k := newKafka(t, cfg)
	if err := k.Publish(ctx, []Record{metricRec(1), metricRec(2)}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	c := newCollector()
	stop := run(t, k, func(ctx context.Context, b []Record) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return errors.New("transient")
		}
		return c.handle(ctx, b)
	})
	c.wait(t, 2, 60*time.Second)
	stop()
	if st := k.Stats(); st.Delivered != 2 || st.Dropped != 0 {
		t.Fatalf("retry: %+v", st)
	}
	_ = k.Close()

	// Poison batch: always fails.
	k2 := newKafka(t, cfg)
	if err := k2.Publish(ctx, []Record{metricRec(3)}); err != nil {
		t.Fatal(err)
	}
	failed := make(chan struct{}, 4)
	stop = run(t, k2, func(context.Context, []Record) error { failed <- struct{}{}; return errors.New("poison") })
	for i := 0; i < 2; i++ {
		select {
		case <-failed:
		case <-time.After(60 * time.Second):
			t.Fatal("poison batch not attempted twice")
		}
	}
	time.Sleep(500 * time.Millisecond)
	stop()
	if st := k2.Stats(); st.Dropped != 1 || st.Delivered != 0 {
		t.Fatalf("poison: %+v", st)
	}
	_ = k2.Close()

	k3 := newKafka(t, cfg)
	c3 := newCollector()
	if err := k3.Publish(ctx, []Record{metricRec(4)}); err != nil {
		t.Fatal(err)
	}
	stop = run(t, k3, c3.handle)
	got := c3.wait(t, 1, 60*time.Second)
	time.Sleep(time.Second)
	got = c3.wait(t, 1, time.Second)
	stop()
	if len(got) != 1 || got[0].Metric.Timestamp != metricRec(4).Metric.Timestamp {
		t.Fatalf("after drop the group must resume past the poison batch, got %d records", len(got))
	}
}

func TestKafkaBackpressureAndClose(t *testing.T) {
	cfg := kafkaTestConfig(t)
	cfg.MaxBufferedRecords = 10
	k := newKafka(t, cfg)
	ctx := context.Background()
	var recs []Record
	for i := 0; i < 2000; i++ {
		recs = append(recs, metricRec(i))
	}
	if err := k.Publish(ctx, recs); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("want ErrBackpressure from a full produce buffer, got %v", err)
	}
	st := k.Stats()
	if st.Dropped == 0 || st.Published+st.Dropped != 2000 {
		t.Fatalf("backpressure accounting %+v", st)
	}
	// Once the buffer drains, publishing works again.
	if err := k.Publish(ctx, recs[:5]); err != nil {
		t.Fatalf("after backpressure: %v", err)
	}

	// Close: Run returns, Publish reports ErrClosed.
	runDone := make(chan error, 1)
	go func() { runDone <- k.Run(context.Background(), func(context.Context, []Record) error { return nil }) }()
	time.Sleep(2 * time.Second)
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run after Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Close")
	}
	if err := k.Publish(ctx, recs[:1]); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close: want ErrClosed, got %v", err)
	}
	if err := k.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
}
