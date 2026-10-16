package commitlog

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// DefaultKafkaGroup is the consumer group storage writers join.
const DefaultKafkaGroup = "zerowatch-storage"

// KafkaConfig configures NewKafkaConfig. Zero values take the defaults.
type KafkaConfig struct {
	Brokers []string
	Topic   string
	Group   string // consumer group; DefaultKafkaGroup when empty
	// MaxBufferedRecords bounds records awaiting broker acks; Publish returns
	// ErrBackpressure instead of blocking once it is reached (default 50000).
	MaxBufferedRecords int
	// FlushInterval and BatchSize override the consumer's micro-batching
	// (DefaultFlushInterval, DefaultBatchSize).
	FlushInterval time.Duration
	BatchSize     int
	Log           *slog.Logger
	// ProduceOnly never joins the consumer group (ingest/query/supervisor
	// nodes in a split deployment publish, storage nodes consume); Run then
	// returns ErrProduceOnly.
	ProduceOnly bool
}

// ErrProduceOnly is returned by Run on a client created with ProduceOnly.
var ErrProduceOnly = errors.New("commitlog: kafka client is produce-only")

// Kafka is a Producer/Consumer backed by Kafka or Redpanda (franz-go client).
// Records are JSON-encoded; metrics are keyed by series ID so a series always
// lands on the same partition (ordering per series), logs by group/stream.
//
// Delivery is at-least-once: offsets are committed only after the handler
// accepted a batch, rebalances wait until the in-flight batch is committed,
// and a batch left unhandled at shutdown is redelivered to the next consumer.
type Kafka struct {
	cl        *kgo.Client
	topic     string
	log       *slog.Logger
	interval  time.Duration
	batchSize int
	closed    atomic.Bool
	// produceOnly clients never joined the consumer group; Run refuses.
	produceOnly bool
	routeStats

	published, delivered, dropped atomic.Uint64
}

// NewKafka connects a client that both produces and consumes (consumer group
// DefaultKafkaGroup).
func NewKafka(brokers []string, topic string, log *slog.Logger) (*Kafka, error) {
	return NewKafkaConfig(KafkaConfig{Brokers: brokers, Topic: topic, Log: log})
}

// NewKafkaConfig connects a client from cfg.
func NewKafkaConfig(cfg KafkaConfig) (*Kafka, error) {
	if cfg.Group == "" {
		cfg.Group = DefaultKafkaGroup
	}
	if cfg.MaxBufferedRecords <= 0 {
		cfg.MaxBufferedRecords = 50_000
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.DefaultProduceTopic(cfg.Topic),
		kgo.AllowAutoTopicCreation(),
		kgo.ProducerLinger(5 * time.Millisecond),
		kgo.ProducerBatchCompression(kgo.ZstdCompression(), kgo.Lz4Compression(), kgo.NoCompression()),
		kgo.MaxBufferedRecords(cfg.MaxBufferedRecords),
	}
	if !cfg.ProduceOnly {
		opts = append(opts,
			kgo.ConsumeTopics(cfg.Topic),
			kgo.ConsumerGroup(cfg.Group),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
			kgo.DisableAutoCommit(),
			kgo.BlockRebalanceOnPoll(),
		)
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("commitlog: kafka client: %w", err)
	}
	return &Kafka{cl: cl, topic: cfg.Topic, log: cfg.Log, interval: cfg.FlushInterval, batchSize: cfg.BatchSize, produceOnly: cfg.ProduceOnly}, nil
}

func key(r Record) []byte {
	if r.Metric != nil {
		return binary.BigEndian.AppendUint64(nil, r.Metric.ID())
	}
	if r.Log != nil {
		return []byte(r.Log.LogGroup + "/" + r.Log.LogStream)
	}
	return nil
}

// Publish produces and waits for the broker acks, so the gateway can return
// 200 only after the records are durable. When the client's produce buffer is
// full it returns ErrBackpressure rather than blocking (records queued before
// the full buffer was hit are still delivered: Kafka publishes are not
// all-or-none like Memory's). After Close it returns ErrClosed.
func (k *Kafka) Publish(ctx context.Context, recs []Record) error {
	if k.closed.Load() {
		return ErrClosed
	}
	krs := make([]*kgo.Record, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		krs = append(krs, &kgo.Record{Key: key(r), Value: b})
	}
	// The promise runs exactly once per record (synchronously when the
	// buffer is full); done is sized so it never blocks.
	done := make(chan error, len(krs))
	for _, r := range krs {
		k.cl.TryProduce(ctx, r, func(_ *kgo.Record, err error) { done <- err })
	}
	var first error
	var full bool
	for range krs {
		err := <-done
		switch {
		case err == nil:
			k.published.Add(1)
		case errors.Is(err, kgo.ErrMaxBuffered):
			full = true
			k.dropped.Add(1)
		case first == nil:
			first = err
		}
	}
	switch {
	case full:
		return ErrBackpressure
	case first == nil:
		return nil
	case errors.Is(first, kgo.ErrClientClosed):
		return ErrClosed
	}
	return first
}

// Run polls fetches, accumulating up to BatchSize records or FlushInterval,
// then commits offsets after the handler succeeds. A failing batch is retried
// once (as Memory does) and then dropped and committed, so one poison batch
// cannot stall the partition forever; a batch whose handler was cut short by
// shutdown is not committed. On ctx cancellation the pending batch is flushed
// with a short independent deadline; Run returns when ctx is done or the
// client is closed.
func (k *Kafka) Run(ctx context.Context, h Handler) error {
	if k.produceOnly {
		return ErrProduceOnly
	}
	batch := make([]Record, 0, k.batchSize)
	flush := func(fctx context.Context) {
		if len(batch) > 0 {
			err := h(fctx, batch)
			if err != nil && fctx.Err() == nil {
				err = h(fctx, batch)
			}
			commit := true
			switch {
			case err == nil:
				k.delivered.Add(uint64(len(batch)))
			case fctx.Err() != nil:
				// Interrupted by shutdown, not a bad batch: leave it
				// uncommitted so the next consumer gets it again.
				commit = false
				k.log.Warn("commitlog handler interrupted; batch left for redelivery", "records", len(batch), "err", err)
			default:
				k.dropped.Add(uint64(len(batch)))
				k.log.Error("commitlog handler failed twice; batch dropped", "records", len(batch), "err", err)
			}
			if commit {
				// The batch is handled: commit even if shutdown began meanwhile.
				cctx, cancel := context.WithTimeout(context.WithoutCancel(fctx), 5*time.Second)
				if err := k.cl.CommitUncommittedOffsets(cctx); err != nil {
					k.log.Warn("kafka commit failed", "err", err)
				}
				cancel()
			}
			batch = batch[:0]
		}
		k.cl.AllowRebalance()
	}
	defer k.cl.AllowRebalance()
	deadline := time.Now().Add(k.interval)
	for ctx.Err() == nil {
		pctx, cancel := context.WithDeadline(ctx, deadline)
		fs := k.cl.PollRecords(pctx, k.batchSize-len(batch))
		cancel()
		if fs.IsClientClosed() {
			return nil
		}
		fs.EachError(func(t string, p int32, err error) {
			if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				k.log.Warn("kafka fetch error", "topic", t, "partition", p, "err", err)
			}
		})
		fs.EachRecord(func(r *kgo.Record) {
			var rec Record
			if err := json.Unmarshal(r.Value, &rec); err != nil {
				k.dropped.Add(1)
				k.log.Warn("kafka: undecodable record skipped", "partition", r.Partition, "offset", r.Offset, "err", err)
				return
			}
			batch = append(batch, rec)
		})
		if ctx.Err() != nil {
			break
		}
		if len(batch) >= k.batchSize || !time.Now().Before(deadline) {
			flush(ctx)
			deadline = time.Now().Add(k.interval)
		} else if len(batch) == 0 {
			k.cl.AllowRebalance()
		}
	}
	dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	flush(dctx)
	return nil
}

// Stats returns counters (Depth and Capacity are not tracked for Kafka).
func (k *Kafka) Stats() Stats {
	return Stats{Published: k.published.Load(), Delivered: k.delivered.Load(), Dropped: k.dropped.Load(), Routing: k.routeStats.get()}
}

// Close flushes pending produces, leaves the consumer group and closes the
// client. Publish returns ErrClosed afterwards and Run returns.
func (k *Kafka) Close() error {
	if k.closed.Swap(true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = k.cl.Flush(ctx)
	k.cl.AllowRebalance()
	k.cl.Close()
	return nil
}
