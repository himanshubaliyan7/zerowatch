// Package storage defines the StorageEngine abstraction and its two
// implementations: an embedded in-memory TSDB (Gorilla chunks + 1-minute
// rollups + log ring) and a ClickHouse engine over the HTTP interface.
package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
)

// Errors.
var (
	ErrCardinality = errors.New("storage: series cardinality limit reached")
	ErrBadStat     = errors.New("storage: unsupported statistic")
)

// SeriesFilter narrows ListSeries.
type SeriesFilter struct {
	Namespace  string            `json:"namespace,omitempty"`
	MetricName string            `json:"metricName,omitempty"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Limit      int               `json:"limit,omitempty"`
}

// LogQuery is a log search request.
type LogQuery struct {
	TenantID  uint32 `json:"-"`
	LogGroup  string `json:"logGroup,omitempty"`
	LogStream string `json:"logStream,omitempty"`
	Level     string `json:"level,omitempty"`
	// Query: whitespace separated terms, all must match (case-insensitive
	// substring); key=value terms match attributes; -term excludes.
	Query   string `json:"query,omitempty"`
	StartMs int64  `json:"start,omitempty"`
	EndMs   int64  `json:"end,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// LogResult is returned by SearchLogs (newest first).
type LogResult struct {
	Events    []model.LogEvent `json:"events"`
	Scanned   int              `json:"scanned"`
	Truncated bool             `json:"truncated"`
	// Partial says why some events may be missing (cluster: shards that did
	// not answer); empty when the search covered all data.
	Partial string `json:"partial,omitempty"`
}

// PartialError comes with a list answer (ListSeries, ListLogGroups) built
// from only some of the data: the cluster engine returns it, together with
// the surviving shards' entries, when shards do not answer. Callers that can
// show a partial list should use the data and say so (see IsPartial).
type PartialError struct{ Msg string }

func (e *PartialError) Error() string { return e.Msg }

// IsPartial reports whether err is a *PartialError, i.e. the accompanying
// data is usable but incomplete.
func IsPartial(err error) bool {
	var pe *PartialError
	return errors.As(err, &pe)
}

// LogGroupInfo summarises a log group.
type LogGroupInfo struct {
	Name      string   `json:"name"`
	Streams   []string `json:"streams"`
	Events    int      `json:"events"`
	LastEvent int64    `json:"lastEvent"`
}

// Stats describe engine internals.
type Stats struct {
	Backend      string  `json:"backend"`
	Series       int     `json:"series"`
	Samples      int64   `json:"samples"`
	ChunkBytes   int64   `json:"chunkBytes"`
	BytesPerPt   float64 `json:"bytesPerPoint"`
	RollupBucket int     `json:"rollupBuckets"`
	LogEvents    int     `json:"logEvents"`
}

// Engine is the storage abstraction used by ingest, query and alarms.
type Engine interface {
	query.Fetcher
	WriteMetrics(ctx context.Context, ms []model.MetricDatum) error
	WriteLogs(ctx context.Context, ls []model.LogEvent) error
	ListSeries(ctx context.Context, tenant uint32, f SeriesFilter) ([]model.SeriesMeta, error)
	ListLogGroups(ctx context.Context, tenant uint32) ([]LogGroupInfo, error)
	SearchLogs(ctx context.Context, q LogQuery) (LogResult, error)
	Stats() Stats
	Close() error
}

// Stat is a parsed statistic.
type Stat struct {
	Kind string  // avg|sum|min|max|count|pct
	Pct  float64 // for pct, 0..100
}

// ParseStat accepts CloudWatch names (Average, Sum, Minimum, Maximum,
// SampleCount, pNN / pNN.N).
func ParseStat(s string) (Stat, error) {
	switch s {
	case "Average", "avg", "":
		return Stat{Kind: "avg"}, nil
	case "Sum", "sum":
		return Stat{Kind: "sum"}, nil
	case "Minimum", "min":
		return Stat{Kind: "min"}, nil
	case "Maximum", "max":
		return Stat{Kind: "max"}, nil
	case "SampleCount", "count":
		return Stat{Kind: "count"}, nil
	}
	if strings.HasPrefix(s, "p") {
		f, err := strconv.ParseFloat(s[1:], 64)
		if err == nil && f >= 0 && f <= 100 {
			return Stat{Kind: "pct", Pct: f}, nil
		}
	}
	return Stat{}, fmt.Errorf("%w: %q", ErrBadStat, s)
}

// Rollupable reports whether the stat can be answered from (count,sum,min,max).
func (s Stat) Rollupable() bool { return s.Kind != "pct" }

// Reduce computes the stat over raw values (vals is reordered for percentiles).
func (s Stat) Reduce(vals []float64) float64 {
	if len(vals) == 0 {
		return math.NaN()
	}
	switch s.Kind {
	case "count":
		return float64(len(vals))
	case "pct":
		sort.Float64s(vals)
		// nearest-rank on the (n-1) scale with linear interpolation
		pos := s.Pct / 100 * float64(len(vals)-1)
		lo := int(math.Floor(pos))
		hi := int(math.Ceil(pos))
		return vals[lo] + (vals[hi]-vals[lo])*(pos-float64(lo))
	}
	a := Agg{}
	for _, v := range vals {
		a.Add(v)
	}
	return a.Value(s)
}

// Agg is a mergeable (count,sum,min,max) summary — the rollup unit.
type Agg struct {
	Count, Sum, Min, Max float64
}

// Add folds one sample.
func (a *Agg) Add(v float64) {
	a.Merge(Agg{Count: 1, Sum: v, Min: v, Max: v})
}

// Merge folds another summary.
func (a *Agg) Merge(b Agg) {
	if b.Count == 0 {
		return
	}
	if a.Count == 0 {
		*a = b
		return
	}
	a.Count += b.Count
	a.Sum += b.Sum
	a.Min = math.Min(a.Min, b.Min)
	a.Max = math.Max(a.Max, b.Max)
}

// Value returns the stat from the summary.
func (a Agg) Value(s Stat) float64 {
	if a.Count == 0 {
		return math.NaN()
	}
	switch s.Kind {
	case "sum":
		return a.Sum
	case "min":
		return a.Min
	case "max":
		return a.Max
	case "count":
		return a.Count
	}
	return a.Sum / a.Count
}

// matchDims implements CloudWatch exact-match semantics, extended with "*"
// wildcard values (which aggregate across all values of that dimension).
func matchDims(have, want map[string]string) bool {
	if len(have) != len(want) {
		return false
	}
	for k, v := range want {
		hv, ok := have[k]
		if !ok || (v != "*" && hv != v) {
			return false
		}
	}
	return true
}

// subsetDims is the looser filter used for discovery (ListSeries).
func subsetDims(have, want map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || (v != "*" && v != "" && hv != v) {
			return false
		}
	}
	return true
}

// matchLog applies a LogQuery to one event (shared with tests and live tail).
func MatchLog(q *LogQuery, terms []string, e *model.LogEvent) bool {
	if q.TenantID != 0 && e.TenantID != q.TenantID {
		return false
	}
	if q.LogGroup != "" && e.LogGroup != q.LogGroup {
		return false
	}
	if q.LogStream != "" && e.LogStream != q.LogStream {
		return false
	}
	if q.Level != "" && !strings.EqualFold(q.Level, e.Level) {
		return false
	}
	if q.StartMs != 0 && e.Timestamp < q.StartMs {
		return false
	}
	if q.EndMs != 0 && e.Timestamp >= q.EndMs {
		return false
	}
	if len(terms) == 0 {
		return true
	}
	msg := strings.ToLower(e.Message)
	for _, t := range terms {
		neg := strings.HasPrefix(t, "-") && len(t) > 1
		if neg {
			t = t[1:]
		}
		var hit bool
		if k, v, ok := strings.Cut(t, "="); ok && k != "" {
			av, present := e.Attributes[k]
			if !present && k == "level" {
				av, present = e.Level, true
			}
			hit = present && strings.EqualFold(av, v)
		} else {
			hit = strings.Contains(msg, t)
		}
		if hit == neg {
			return false
		}
	}
	return true
}

// Terms tokenizes a log query (lower-cased). Whitespace separates terms;
// double quotes group text containing spaces and are dropped, so
// route="GET /health" and "connection reset" are single terms. An unclosed
// quote runs to the end. web/src/lib/live.ts filterTerms mirrors this.
func Terms(q string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range strings.ToLower(q) {
		switch {
		case r == '"':
			quoted = !quoted
		case !quoted && unicode.IsSpace(r):
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}
