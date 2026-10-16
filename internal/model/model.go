// Package model defines the canonical telemetry data model shared by every
// ZeroWatch subsystem (ingest, commit log, storage, query, alarms).
//
// The shapes mirror the CloudWatch wire model (MetricDatum / StatisticSet /
// Dimension / InputLogEvent) so the CloudWatch compatibility layer is a thin
// translation, while also carrying OTLP-style attributes.
package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zeebo/xxh3"
)

// Validation limits taken from the CloudWatch PutMetricData / PutLogEvents contracts.
const (
	MaxDimensions      = 30
	MaxDimensionLength = 255
	MaxPastSkew        = 14 * 24 * time.Hour
	MaxFutureSkew      = 2 * time.Hour
	MaxLogBatchBytes   = 1 << 20
	MaxLogBatchEvents  = 10000
)

// Dimension is a strict key/value pair attached to a metric.
type Dimension struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// StatisticSet is a client-side pre-aggregated bucket (CloudWatch StatisticValues).
type StatisticSet struct {
	SampleCount float64 `json:"SampleCount"`
	Sum         float64 `json:"Sum"`
	Minimum     float64 `json:"Minimum"`
	Maximum     float64 `json:"Maximum"`
}

// MetricDatum is a single ingested metric observation.
type MetricDatum struct {
	TenantID          uint32        `json:"tenantId"`
	Namespace         string        `json:"namespace"`
	MetricName        string        `json:"metricName"`
	Dimensions        []Dimension   `json:"dimensions,omitempty"`
	Timestamp         int64         `json:"timestamp"` // unix milliseconds
	Value             *float64      `json:"value,omitempty"`
	StatisticValues   *StatisticSet `json:"statisticValues,omitempty"`
	Unit              string        `json:"unit,omitempty"`
	StorageResolution uint32        `json:"storageResolution,omitempty"` // 1 = high-res, 60 = standard
}

// LogEvent is a single log line belonging to a (group, stream) ledger.
type LogEvent struct {
	TenantID   uint32            `json:"tenantId"`
	LogGroup   string            `json:"logGroup"`
	LogStream  string            `json:"logStream"`
	Timestamp  int64             `json:"timestamp"` // unix milliseconds
	Message    string            `json:"message"`
	Level      string            `json:"level,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Float returns a pointer to v (helper for optional values).
func Float(v float64) *float64 { return &v }

// ErrValidation is wrapped by all datum validation failures so callers can map
// them to HTTP 400.
var ErrValidation = errors.New("validation error")

func validationf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}

// SortDimensions returns dimensions sorted by name (then value) — the canonical order.
func SortDimensions(d []Dimension) []Dimension {
	out := make([]Dimension, len(d))
	copy(out, d)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Value < out[j].Value
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SeriesID computes the canonical 64-bit series fingerprint:
//
//	SeriesID = XXH3_64(TenantID || Namespace || MetricName || SortedDimensions)
//
// Fields are separated with 0xFF bytes (never valid UTF-8) so that
// ("ab","c") and ("a","bc") cannot collide by concatenation.
func SeriesID(tenant uint32, namespace, name string, dims []Dimension) uint64 {
	var b strings.Builder
	b.Grow(64)
	fmt.Fprintf(&b, "%d", tenant)
	b.WriteByte(0xff)
	b.WriteString(namespace)
	b.WriteByte(0xff)
	b.WriteString(name)
	for _, d := range SortDimensions(dims) {
		b.WriteByte(0xff)
		b.WriteString(d.Name)
		b.WriteByte(0xfe)
		b.WriteString(d.Value)
	}
	return xxh3.HashString(b.String())
}

// ID returns the fingerprint of this datum's series.
func (m *MetricDatum) ID() uint64 {
	return SeriesID(m.TenantID, m.Namespace, m.MetricName, m.Dimensions)
}

// DimMap returns dimensions as a map.
func (m *MetricDatum) DimMap() map[string]string {
	out := make(map[string]string, len(m.Dimensions))
	for _, d := range m.Dimensions {
		out[d.Name] = d.Value
	}
	return out
}

// Validate enforces the CloudWatch wire invariants relative to now.
func (m *MetricDatum) Validate(now time.Time) error {
	if m.Namespace == "" || m.MetricName == "" {
		return validationf("namespace and metricName are required")
	}
	if len(m.Namespace) > MaxDimensionLength || len(m.MetricName) > MaxDimensionLength {
		return validationf("namespace/metricName exceed %d chars", MaxDimensionLength)
	}
	if len(m.Dimensions) > MaxDimensions {
		return validationf("too many dimensions: %d > %d", len(m.Dimensions), MaxDimensions)
	}
	seen := make(map[string]struct{}, len(m.Dimensions))
	for _, d := range m.Dimensions {
		if d.Name == "" || d.Value == "" {
			return validationf("dimension name and value must be non-empty")
		}
		if utf8.RuneCountInString(d.Name) > MaxDimensionLength || utf8.RuneCountInString(d.Value) > MaxDimensionLength {
			return validationf("dimension %q exceeds %d chars", d.Name, MaxDimensionLength)
		}
		if _, dup := seen[d.Name]; dup {
			return validationf("duplicate dimension %q", d.Name)
		}
		seen[d.Name] = struct{}{}
	}
	if (m.Value == nil) == (m.StatisticValues == nil) {
		return validationf("exactly one of value or statisticValues is required")
	}
	if s := m.StatisticValues; s != nil {
		if s.SampleCount <= 0 || s.Minimum > s.Maximum {
			return validationf("invalid statisticValues")
		}
	}
	if m.StorageResolution != 0 && m.StorageResolution != 1 && m.StorageResolution != 60 {
		return validationf("storageResolution must be 1 or 60")
	}
	ts := time.UnixMilli(m.Timestamp)
	if ts.Before(now.Add(-MaxPastSkew)) || ts.After(now.Add(MaxFutureSkew)) {
		return validationf("timestamp outside [-14d, +2h] window")
	}
	return nil
}

// Validate checks a log event.
func (e *LogEvent) Validate(now time.Time) error {
	if e.LogGroup == "" || e.LogStream == "" {
		return validationf("logGroup and logStream are required")
	}
	ts := time.UnixMilli(e.Timestamp)
	if ts.Before(now.Add(-MaxPastSkew)) || ts.After(now.Add(MaxFutureSkew)) {
		return validationf("log timestamp outside [-14d, +2h] window")
	}
	return nil
}

// Point is a decoded (timestamp, value) sample.
type Point struct {
	T int64   `json:"t"` // unix ms
	V float64 `json:"v"`
}

// SeriesMeta describes a stored series.
type SeriesMeta struct {
	ID         uint64            `json:"id,string"`
	TenantID   uint32            `json:"tenantId"`
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metricName"`
	Dimensions map[string]string `json:"dimensions"`
	Unit       string            `json:"unit,omitempty"`
}
