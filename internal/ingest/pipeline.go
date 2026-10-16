// Package ingest implements every write-path protocol ZeroWatch speaks:
//
//   - OTLP/HTTP JSON (/v1/metrics, /v1/logs)
//   - Prometheus remote write (snappy + protobuf, hand-decoded with protowire)
//   - StatsD / DogStatsD over UDP
//   - CloudWatch query/JSON protocol (PutMetricData, PutLogEvents, GetMetricData)
//   - Embedded Metric Format extraction from any ingested log line
//
// All protocols converge on Pipeline, which validates, normalises and
// publishes to the commit log. Nothing here touches storage directly, so the
// ingest role scales independently of storage (blueprint: "ingest is
// stateless; the log is the source of truth").
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/zerowatch/zerowatch/internal/commitlog"
	"github.com/zerowatch/zerowatch/internal/emf"
	"github.com/zerowatch/zerowatch/internal/model"
)

// Pipeline validates and publishes telemetry.
type Pipeline struct {
	Producer commitlog.Producer
	Log      *slog.Logger
	Now      func() time.Time
	// ExtractEMF turns EMF-formatted log lines into metrics (default true via NewPipeline).
	ExtractEMF bool

	metricsAccepted atomic.Uint64
	metricsRejected atomic.Uint64
	logsAccepted    atomic.Uint64
	logsRejected    atomic.Uint64
	emfExtracted    atomic.Uint64
}

// NewPipeline returns a pipeline with EMF extraction enabled.
func NewPipeline(p commitlog.Producer, log *slog.Logger) *Pipeline {
	return &Pipeline{Producer: p, Log: log, Now: time.Now, ExtractEMF: true}
}

// Result summarises a write.
type Result struct {
	Accepted int      `json:"accepted"`
	Rejected int      `json:"rejected"`
	Errors   []string `json:"errors,omitempty"`
}

const maxReportedErrors = 10

func (r *Result) reject(i int, err error) {
	r.Rejected++
	if len(r.Errors) < maxReportedErrors {
		r.Errors = append(r.Errors, fmt.Sprintf("item %d: %v", i, err))
	}
}

// ErrAllRejected is returned when a non-empty batch had zero valid items.
var ErrAllRejected = fmt.Errorf("%w: every item in the batch was rejected", model.ErrValidation)

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// WriteMetrics validates each datum (partial acceptance, like CloudWatch's
// per-datum validation) and publishes the valid ones atomically.
func (p *Pipeline) WriteMetrics(ctx context.Context, tenant uint32, ms []model.MetricDatum) (Result, error) {
	var res Result
	now := p.now()
	recs := make([]commitlog.Record, 0, len(ms))
	for i := range ms {
		m := ms[i]
		m.TenantID = tenant
		if m.Timestamp == 0 {
			m.Timestamp = now.UnixMilli()
		}
		m.Dimensions = model.SortDimensions(m.Dimensions)
		if err := m.Validate(now); err != nil {
			res.reject(i, err)
			continue
		}
		recs = append(recs, commitlog.Record{Metric: &m})
	}
	p.metricsRejected.Add(uint64(res.Rejected))
	if len(recs) == 0 {
		if len(ms) > 0 {
			return res, ErrAllRejected
		}
		return res, nil
	}
	if err := p.Producer.Publish(ctx, recs); err != nil {
		return res, err
	}
	res.Accepted = len(recs)
	p.metricsAccepted.Add(uint64(len(recs)))
	return res, nil
}

// WriteLogs validates and publishes log events; EMF lines additionally yield metrics.
func (p *Pipeline) WriteLogs(ctx context.Context, tenant uint32, ls []model.LogEvent) (Result, error) {
	var res Result
	if len(ls) > model.MaxLogBatchEvents {
		return res, fmt.Errorf("%w: batch has %d events (max %d)", model.ErrValidation, len(ls), model.MaxLogBatchEvents)
	}
	now := p.now()
	recs := make([]commitlog.Record, 0, len(ls))
	var size int
	for i := range ls {
		l := ls[i]
		l.TenantID = tenant
		if l.Timestamp == 0 {
			l.Timestamp = now.UnixMilli()
		}
		size += len(l.Message) + 26 // CloudWatch accounting: message + 26 bytes overhead
		if err := l.Validate(now); err != nil {
			res.reject(i, err)
			continue
		}
		if l.Level == "" {
			l.Level = InferLevel(l.Message)
		}
		recs = append(recs, commitlog.Record{Log: &l})
		if p.ExtractEMF && emf.IsCandidate([]byte(l.Message)) {
			mets, err := emf.Extract(tenant, []byte(l.Message), l.Timestamp)
			if err == nil {
				for j := range mets {
					m := mets[j]
					if m.Validate(now) == nil {
						recs = append(recs, commitlog.Record{Metric: &m})
						p.emfExtracted.Add(1)
					}
				}
			} else if !errors.Is(err, emf.ErrNotEMF) {
				p.Log.Debug("emf extraction failed", "err", err)
			}
		}
	}
	if size > model.MaxLogBatchBytes {
		return Result{}, fmt.Errorf("%w: batch is %d bytes (max %d)", model.ErrValidation, size, model.MaxLogBatchBytes)
	}
	p.logsRejected.Add(uint64(res.Rejected))
	if len(recs) == 0 {
		if len(ls) > 0 {
			return res, ErrAllRejected
		}
		return res, nil
	}
	if err := p.Producer.Publish(ctx, recs); err != nil {
		return res, err
	}
	res.Accepted = len(ls) - res.Rejected
	p.logsAccepted.Add(uint64(res.Accepted))
	return res, nil
}

// Stats are pipeline counters.
type Stats struct {
	MetricsAccepted uint64 `json:"metricsAccepted"`
	MetricsRejected uint64 `json:"metricsRejected"`
	LogsAccepted    uint64 `json:"logsAccepted"`
	LogsRejected    uint64 `json:"logsRejected"`
	EMFExtracted    uint64 `json:"emfExtracted"`
}

// Stats returns counters.
func (p *Pipeline) Stats() Stats {
	return Stats{
		MetricsAccepted: p.metricsAccepted.Load(), MetricsRejected: p.metricsRejected.Load(),
		LogsAccepted: p.logsAccepted.Load(), LogsRejected: p.logsRejected.Load(),
		EMFExtracted: p.emfExtracted.Load(),
	}
}

// InferLevel guesses a severity from free text (cheap, prefix-biased scan).
func InferLevel(msg string) string {
	n := len(msg)
	if n > 200 {
		n = 200
	}
	head := msg[:n]
	for _, c := range []struct{ needle, level string }{
		{"FATAL", "fatal"}, {"PANIC", "fatal"}, {"ERROR", "error"}, {"error", "error"},
		{"WARN", "warn"}, {"warn", "warn"}, {"DEBUG", "debug"}, {"debug", "debug"},
	} {
		if containsWord(head, c.needle) {
			return c.level
		}
	}
	return "info"
}

func containsWord(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i:i+len(w)] != w {
			continue
		}
		before := i == 0 || !isAlpha(s[i-1])
		after := i+len(w) == len(s) || !isAlpha(s[i+len(w)])
		if before && (after || w == "WARN" || w == "warn") {
			return true
		}
	}
	return false
}

func isAlpha(b byte) bool { return (b|0x20) >= 'a' && (b|0x20) <= 'z' }
