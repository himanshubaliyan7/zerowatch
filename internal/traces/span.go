// Package traces holds distributed-tracing spans received over OTLP (or
// produced by the synthetic generator) in a bounded, per-tenant in-memory
// store, and derives RED metrics from SERVER spans.
//
// The JSON shapes here are a fixed contract with the UI (web/src/lib/api.ts:
// TraceSummary, Span, TraceDetail): times are unix milliseconds (fractional),
// kinds and statuses lowercase, ids lowercase hex.
package traces

import (
	"fmt"
	"strings"

	"github.com/zerowatch/zerowatch/internal/model"
)

// Span kinds (lowercase, as the UI expects).
const (
	KindInternal = "internal"
	KindServer   = "server"
	KindClient   = "client"
	KindProducer = "producer"
	KindConsumer = "consumer"
)

// Span statuses.
const (
	StatusUnset = "unset"
	StatusOK    = "ok"
	StatusError = "error"
)

// SyntheticAttr marks spans made by the generator. RED derivation skips them
// because the generator already writes its own RED metrics.
const SyntheticAttr = "zerowatch.synthetic"

// Per-span bounds, so one exporter cannot blow up store memory with huge
// attribute maps.
const (
	MaxAttributes  = 64
	MaxAttrValue   = 1024
	MaxNameLength  = 512
	MaxStatusMsg   = 1024
	traceIDHexLen  = 32
	spanIDHexLen   = 16
	defaultService = "unknown_service"
)

// Span is one unit of work.
type Span struct {
	TraceID       string            `json:"traceId"`
	SpanID        string            `json:"spanId"`
	ParentSpanID  string            `json:"parentSpanId,omitempty"`
	Service       string            `json:"service"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Start         float64           `json:"start"`
	DurationMs    float64           `json:"durationMs"`
	Status        string            `json:"status"`
	StatusMessage string            `json:"statusMessage,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

// End is the span end time in unix ms.
func (s *Span) End() float64 { return s.Start + s.DurationMs }

// IsError reports whether the span ended in error.
func (s *Span) IsError() bool { return s.Status == StatusError }

// Normalize lowercases ids, fills defaults and truncates oversized fields;
// Validate must still be called afterwards.
func (s *Span) Normalize() {
	s.TraceID = strings.ToLower(s.TraceID)
	s.SpanID = strings.ToLower(s.SpanID)
	s.ParentSpanID = strings.ToLower(s.ParentSpanID)
	if isZeroHex(s.ParentSpanID) {
		s.ParentSpanID = ""
	}
	if s.Service == "" {
		s.Service = defaultService
	}
	switch s.Kind {
	case KindServer, KindClient, KindProducer, KindConsumer:
	default:
		s.Kind = KindInternal
	}
	switch s.Status {
	case StatusOK, StatusError:
	default:
		s.Status = StatusUnset
	}
	if s.DurationMs < 0 {
		s.DurationMs = 0
	}
	s.Name = truncate(s.Name, MaxNameLength)
	s.Service = truncate(s.Service, model.MaxDimensionLength)
	s.StatusMessage = truncate(s.StatusMessage, MaxStatusMsg)
	for k, v := range s.Attributes {
		if len(v) > MaxAttrValue {
			s.Attributes[k] = truncate(v, MaxAttrValue)
		}
	}
	if len(s.Attributes) > MaxAttributes {
		n := 0
		for k := range s.Attributes {
			if n++; n > MaxAttributes {
				delete(s.Attributes, k)
			}
		}
	}
}

// Validate checks ids and timing.
func (s *Span) Validate() error {
	if !isHex(s.TraceID, traceIDHexLen) || isZeroHex(s.TraceID) {
		return fmt.Errorf("%w: traceId must be %d lowercase hex chars and non-zero", model.ErrValidation, traceIDHexLen)
	}
	if !isHex(s.SpanID, spanIDHexLen) || isZeroHex(s.SpanID) {
		return fmt.Errorf("%w: spanId must be %d lowercase hex chars and non-zero", model.ErrValidation, spanIDHexLen)
	}
	if s.ParentSpanID != "" && !isHex(s.ParentSpanID, spanIDHexLen) {
		return fmt.Errorf("%w: parentSpanId must be %d hex chars", model.ErrValidation, spanIDHexLen)
	}
	if s.Start <= 0 {
		return fmt.Errorf("%w: span %s has no start time", model.ErrValidation, s.SpanID)
	}
	if s.Name == "" {
		return fmt.Errorf("%w: span %s has no name", model.ErrValidation, s.SpanID)
	}
	return nil
}

// ValidTraceID reports whether id is a well-formed lowercase trace id.
func ValidTraceID(id string) bool { return isHex(id, traceIDHexLen) && !isZeroHex(id) }

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isZeroHex(s string) bool {
	if s == "" {
		return false
	}
	return strings.Trim(s, "0") == ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Drop a rune cut in half so JSON output stays valid UTF-8.
	return strings.ToValidUTF8(s[:n], "")
}
