package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
)

// StatsD / DogStatsD line protocol:
//
//	<name>:<value>|<type>[|@<sample_rate>][|#tag1:v1,tag2:v2]
//
// Types: c (counter), g (gauge, "+N"/"-N" are deltas), ms/h/d (timer/
// histogram/distribution), s (set). Multiple lines per datagram separated
// by '\n'. Samples are aggregated in-process per flush interval and emitted
// as one datum per series — exactly what the CloudWatch agent does, which
// cuts write amplification by orders of magnitude for hot counters.

// StatsDNamespace is the namespace for StatsD metrics.
const StatsDNamespace = "StatsD"

// StatsDSample is one parsed line.
type StatsDSample struct {
	Name  string
	Value float64
	Type  string
	Rate  float64
	Delta bool // gauge +/- delta
	Set   string
	Tags  []model.Dimension
}

// ErrStatsD marks malformed lines.
var ErrStatsD = errors.New("statsd: malformed line")

// ParseStatsDLine parses a single line.
func ParseStatsDLine(line string) (StatsDSample, error) {
	s := StatsDSample{Rate: 1}
	colon := strings.IndexByte(line, ':')
	if colon <= 0 {
		return s, fmt.Errorf("%w: missing ':' in %q", ErrStatsD, line)
	}
	s.Name = line[:colon]
	parts := strings.Split(line[colon+1:], "|")
	if len(parts) < 2 {
		return s, fmt.Errorf("%w: missing type in %q", ErrStatsD, line)
	}
	s.Type = parts[1]
	switch s.Type {
	case "c", "g", "ms", "h", "d", "s":
	default:
		return s, fmt.Errorf("%w: unknown type %q", ErrStatsD, s.Type)
	}
	raw := parts[0]
	if s.Type == "s" {
		s.Set = raw
	} else {
		if s.Type == "g" && (strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-")) {
			s.Delta = true
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return s, fmt.Errorf("%w: bad value %q", ErrStatsD, raw)
		}
		s.Value = v
	}
	for _, p := range parts[2:] {
		switch {
		case strings.HasPrefix(p, "@"):
			r, err := strconv.ParseFloat(p[1:], 64)
			if err != nil || r <= 0 || r > 1 {
				return s, fmt.Errorf("%w: bad sample rate %q", ErrStatsD, p)
			}
			s.Rate = r
		case strings.HasPrefix(p, "#"):
			for _, t := range strings.Split(p[1:], ",") {
				k, v, ok := strings.Cut(t, ":")
				if !ok {
					k, v = t, "true"
				}
				if k != "" && v != "" && len(s.Tags) < model.MaxDimensions {
					s.Tags = append(s.Tags, model.Dimension{Name: k, Value: v})
				}
			}
		}
	}
	return s, nil
}

type statsdAgg struct {
	name  string
	dims  []model.Dimension
	typ   string
	sum   float64
	count float64
	min   float64
	max   float64
	last  float64
	set   map[string]struct{}
}

// StatsDAggregator accumulates samples between flushes.
type StatsDAggregator struct {
	mu      sync.Mutex
	series  map[string]*statsdAgg
	gauges  map[string]float64 // gauges persist across flushes (statsd semantics)
	maxKeys int
	dropped uint64
}

// NewStatsDAggregator bounds the number of distinct series per interval.
func NewStatsDAggregator(maxKeys int) *StatsDAggregator {
	if maxKeys <= 0 {
		maxKeys = 100000
	}
	return &StatsDAggregator{series: map[string]*statsdAgg{}, gauges: map[string]float64{}, maxKeys: maxKeys}
}

func seriesKey(name, typ string, dims []model.Dimension) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('|')
	b.WriteString(typ)
	for _, d := range dims {
		b.WriteByte('|')
		b.WriteString(d.Name)
		b.WriteByte('=')
		b.WriteString(d.Value)
	}
	return b.String()
}

// Add folds a sample into the current interval.
func (a *StatsDAggregator) Add(s StatsDSample) {
	dims := model.SortDimensions(s.Tags)
	typ := s.Type
	if typ == "h" || typ == "d" {
		typ = "ms"
	}
	key := seriesKey(s.Name, typ, dims)
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.series[key]
	if !ok {
		if len(a.series) >= a.maxKeys {
			a.dropped++
			return
		}
		g = &statsdAgg{name: s.Name, dims: dims, typ: typ, min: math.Inf(1), max: math.Inf(-1)}
		a.series[key] = g
	}
	switch typ {
	case "c":
		g.sum += s.Value / s.Rate
		g.count += 1 / s.Rate
	case "g":
		if s.Delta {
			a.gauges[key] += s.Value
		} else {
			a.gauges[key] = s.Value
		}
		g.last = a.gauges[key]
		g.count++
	case "ms":
		w := 1 / s.Rate
		g.sum += s.Value * w
		g.count += w
		g.min = math.Min(g.min, s.Value)
		g.max = math.Max(g.max, s.Value)
	case "s":
		if g.set == nil {
			g.set = map[string]struct{}{}
		}
		g.set[s.Set] = struct{}{}
	}
}

// Flush emits one datum per series and resets the interval.
func (a *StatsDAggregator) Flush(ts int64) []model.MetricDatum {
	a.mu.Lock()
	cur := a.series
	a.series = make(map[string]*statsdAgg, len(cur))
	a.mu.Unlock()
	keys := make([]string, 0, len(cur))
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]model.MetricDatum, 0, len(cur))
	for _, k := range keys {
		g := cur[k]
		d := model.MetricDatum{Namespace: StatsDNamespace, MetricName: g.name, Dimensions: g.dims, Timestamp: ts}
		switch g.typ {
		case "c":
			d.Value, d.Unit = model.Float(g.sum), "Count"
		case "g":
			d.Value = model.Float(g.last)
		case "ms":
			d.Unit = "Milliseconds"
			d.StatisticValues = &model.StatisticSet{SampleCount: g.count, Sum: g.sum, Minimum: g.min, Maximum: g.max}
		case "s":
			d.Value, d.Unit = model.Float(float64(len(g.set))), "Count"
		}
		out = append(out, d)
	}
	return out
}

// StatsDServer listens on UDP and periodically flushes into the pipeline.
type StatsDServer struct {
	Addr     string
	Pipeline *Pipeline
	Tenant   uint32
	Interval time.Duration
	Log      *slog.Logger

	agg *StatsDAggregator
}

// Run blocks until ctx is cancelled.
func (s *StatsDServer) Run(ctx context.Context) error {
	pc, err := net.ListenPacket("udp", s.Addr)
	if err != nil {
		return fmt.Errorf("statsd listen %s: %w", s.Addr, err)
	}
	if s.Interval <= 0 {
		s.Interval = 10 * time.Second
	}
	s.agg = NewStatsDAggregator(0)
	s.Log.Info("statsd listening", "addr", pc.LocalAddr().String())
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	go s.flushLoop(ctx)
	buf := make([]byte, 65535)
	var bad uint64
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("statsd read: %w", err)
		}
		for _, line := range bytes.Split(buf[:n], []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			smp, err := ParseStatsDLine(string(line))
			if err != nil {
				bad++
				if bad%1000 == 1 {
					s.Log.Debug("statsd parse error", "err", err, "total", bad)
				}
				continue
			}
			s.agg.Add(smp)
		}
	}
}

func (s *StatsDServer) flushLoop(ctx context.Context) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			ds := s.agg.Flush(now.UnixMilli())
			if len(ds) == 0 {
				continue
			}
			if _, err := s.Pipeline.WriteMetrics(ctx, s.Tenant, ds); err != nil {
				s.Log.Warn("statsd flush failed", "err", err, "series", len(ds))
			}
		}
	}
}
