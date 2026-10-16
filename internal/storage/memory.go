package storage

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zerowatch/zerowatch/internal/codec"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/query"
)

const (
	chunkMaxSamples = 240 // seal head chunk after 240 samples (4h @ 1m, 4min @ 1s)
	minuteMs        = 60_000
)

// MemoryConfig tunes the embedded engine.
type MemoryConfig struct {
	RawRetention    time.Duration
	RollupRetention time.Duration
	MaxSeries       int
	MaxLogEvents    int
	// Tier, when set, replaces RawRetention/RollupRetention with per-tenant
	// cutoffs, receives the data leaving memory, and serves it back on
	// queries (internal/tier). Nil keeps the behaviour above unchanged.
	Tier Tier
}

// Tier is an optional cold tier and retention policy behind the memory
// engine. Methods must be safe for concurrent use; Archive must not block
// (it runs under the engine's locks). Cold reads cover exactly the data
// handed to Archive, so hot and cold never overlap.
type Tier interface {
	// Cutoffs returns, for a tenant at nowMs, the unix-ms times before which
	// raw chunks (by their newest sample), rollup minutes and log events
	// leave memory. 0 keeps everything.
	Cutoffs(tenant uint32, nowMs int64) Cutoffs
	// Archive takes ownership of data leaving memory.
	Archive(e *Evicted)
	// ColdRollups calls fn for archived minute summaries of the series
	// matching ms (exact dimension semantics) with minute in [startMs, endMs).
	ColdRollups(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, fn func(minute int64, a Agg)) error
	// ColdRaw calls fn for archived raw samples in [startMs, endMs) until fn
	// returns false.
	ColdRaw(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, fn func(t int64, v float64) bool) error
	// ColdLogs searches archived events (newest first, at most limit).
	ColdLogs(ctx context.Context, q LogQuery, terms []string, limit int) (LogResult, error)
}

// Cutoffs are the per-tenant eviction times returned by Tier.Cutoffs.
type Cutoffs struct{ Raw, Rollup, Logs int64 }

// Evicted is data leaving memory. Chunks are no longer written to.
type Evicted struct {
	Series []EvictedSeries
	Logs   []model.LogEvent
}

// EvictedSeries is one series' evicted chunks and rollup minutes.
type EvictedSeries struct {
	Meta    model.SeriesMeta
	Chunks  []*codec.Chunk
	Rollups map[int64]Agg
}

type series struct {
	meta   model.SeriesMeta
	mu     sync.Mutex
	chunks []*codec.Chunk // last element is the open head chunk
	rollup map[int64]*Agg // minute start -> summary (the 1m tier)
	last   int64
}

type nameKey struct {
	tenant     uint32
	ns, metric string
}

// Memory is the embedded StorageEngine.
type Memory struct {
	cfg MemoryConfig

	mu     sync.RWMutex
	series map[uint64]*series
	byName map[nameKey][]*series

	logMu   sync.RWMutex
	logs    []model.LogEvent // ring
	logHead int
	logN    int

	// evictMu (only with a Tier) makes eviction atomic for metric queries,
	// which read hot and cold data under its read lock.
	evictMu sync.RWMutex

	samples atomic.Int64
	now     func() time.Time
	stop    chan struct{}
	once    sync.Once
}

// NewMemory creates the engine and starts its retention janitor.
func NewMemory(cfg MemoryConfig) *Memory {
	if cfg.RawRetention <= 0 {
		cfg.RawRetention = 6 * time.Hour
	}
	if cfg.RollupRetention <= 0 {
		cfg.RollupRetention = 15 * 24 * time.Hour
	}
	if cfg.MaxSeries <= 0 {
		cfg.MaxSeries = 200_000
	}
	if cfg.MaxLogEvents <= 0 {
		cfg.MaxLogEvents = 200_000
	}
	m := &Memory{
		cfg: cfg, series: map[uint64]*series{}, byName: map[nameKey][]*series{},
		logs: make([]model.LogEvent, cfg.MaxLogEvents), now: time.Now, stop: make(chan struct{}),
	}
	go m.janitor()
	return m
}

// WriteMetrics appends points; StatisticValues go straight into the rollup tier.
func (m *Memory) WriteMetrics(_ context.Context, ms []model.MetricDatum) error {
	var firstErr error
	for i := range ms {
		d := &ms[i]
		s, err := m.getOrCreate(d)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		minute := query.AlignTimestamp(d.Timestamp, minuteMs)
		s.mu.Lock()
		if d.StatisticValues != nil {
			sv := d.StatisticValues
			a := Agg{Count: sv.SampleCount, Sum: sv.Sum, Min: sv.Minimum, Max: sv.Maximum}
			s.addRollup(minute, a)
			if sv.SampleCount > 0 {
				s.appendRaw(d.Timestamp, sv.Sum/sv.SampleCount)
			}
		} else if d.Value != nil {
			s.appendRaw(d.Timestamp, *d.Value)
			a := Agg{}
			a.Add(*d.Value)
			s.addRollup(minute, a)
		}
		if d.Timestamp > s.last {
			s.last = d.Timestamp
		}
		s.mu.Unlock()
		m.samples.Add(1)
	}
	return firstErr
}

func (s *series) appendRaw(t int64, v float64) {
	if len(s.chunks) == 0 || s.chunks[len(s.chunks)-1].Len() >= chunkMaxSamples {
		s.chunks = append(s.chunks, codec.NewChunk())
	}
	s.chunks[len(s.chunks)-1].Append(t, v)
}

func (s *series) addRollup(minute int64, a Agg) {
	r := s.rollup[minute]
	if r == nil {
		r = &Agg{}
		s.rollup[minute] = r
	}
	r.Merge(a)
}

func (m *Memory) getOrCreate(d *model.MetricDatum) (*series, error) {
	id := d.ID()
	m.mu.RLock()
	s := m.series[id]
	m.mu.RUnlock()
	if s != nil {
		return s, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s = m.series[id]; s != nil {
		return s, nil
	}
	if len(m.series) >= m.cfg.MaxSeries {
		return nil, ErrCardinality
	}
	s = &series{
		meta:   model.SeriesMeta{ID: id, TenantID: d.TenantID, Namespace: d.Namespace, MetricName: d.MetricName, Dimensions: d.DimMap(), Unit: d.Unit},
		rollup: map[int64]*Agg{},
	}
	m.series[id] = s
	k := nameKey{d.TenantID, d.Namespace, d.MetricName}
	m.byName[k] = append(m.byName[k], s)
	return s, nil
}

// FetchStat aggregates matching series per period. Rollup-able stats with
// minute-multiple periods are served from the 1m tier; everything else
// (percentiles, sub-minute periods) scans raw Gorilla chunks.
func (m *Memory) FetchStat(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, periodSec int) ([]model.Point, error) {
	st, err := ParseStat(ms.Stat)
	if err != nil {
		return nil, err
	}
	if ms.Dimensions == nil {
		ms.Dimensions = map[string]string{}
	}
	tier := m.cfg.Tier
	if tier != nil {
		m.evictMu.RLock()
		defer m.evictMu.RUnlock()
	}
	m.mu.RLock()
	cands := m.byName[nameKey{tenant, ms.Namespace, ms.MetricName}]
	matched := make([]*series, 0, len(cands))
	for _, s := range cands {
		if matchDims(s.meta.Dimensions, ms.Dimensions) {
			matched = append(matched, s)
		}
	}
	m.mu.RUnlock()
	if len(matched) == 0 && tier == nil {
		return nil, nil
	}
	pMs := int64(periodSec) * 1000
	useRollup := st.Rollupable() && pMs%minuteMs == 0
	if useRollup {
		buckets := map[int64]*Agg{}
		for _, s := range matched {
			s.mu.Lock()
			for minute, a := range s.rollup {
				if minute < query.AlignTimestamp(startMs, minuteMs) || minute >= endMs {
					continue
				}
				b := query.AlignTimestamp(minute, pMs)
				if buckets[b] == nil {
					buckets[b] = &Agg{}
				}
				buckets[b].Merge(*a)
			}
			s.mu.Unlock()
		}
		if tier != nil {
			err := tier.ColdRollups(ctx, tenant, ms, query.AlignTimestamp(startMs, minuteMs), endMs, func(minute int64, a Agg) {
				b := query.AlignTimestamp(minute, pMs)
				if buckets[b] == nil {
					buckets[b] = &Agg{}
				}
				buckets[b].Merge(a)
			})
			if err != nil {
				return nil, err
			}
		}
		out := make([]model.Point, 0, len(buckets))
		for t, a := range buckets {
			out = append(out, model.Point{T: t, V: a.Value(st)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
		return out, ctx.Err()
	}
	vals, _ := collectRaw(matched, startMs, endMs, pMs, 0)
	if tier != nil {
		err := tier.ColdRaw(ctx, tenant, ms, startMs, endMs, func(t int64, v float64) bool {
			b := query.AlignTimestamp(t, pMs)
			vals[b] = append(vals[b], v)
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]model.Point, 0, len(vals))
	for t, v := range vals {
		out = append(out, model.Point{T: t, V: st.Reduce(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, ctx.Err()
}

// FetchRaw returns the raw samples of the series matching ms (same exact
// dimension semantics as FetchStat), bucketed by period start, so a cluster
// coordinator can compute exact percentiles across shards. It stops after
// limit values in total (limit <= 0: no limit) and reports truncated.
// ms.Stat is ignored.
func (m *Memory) FetchRaw(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, periodSec, limit int) (map[int64][]float64, bool, error) {
	if periodSec <= 0 {
		return nil, false, fmt.Errorf("%w: period must be positive", ErrBadStat)
	}
	if ms.Dimensions == nil {
		ms.Dimensions = map[string]string{}
	}
	tier := m.cfg.Tier
	if tier != nil {
		m.evictMu.RLock()
		defer m.evictMu.RUnlock()
	}
	m.mu.RLock()
	cands := m.byName[nameKey{tenant, ms.Namespace, ms.MetricName}]
	matched := make([]*series, 0, len(cands))
	for _, s := range cands {
		if matchDims(s.meta.Dimensions, ms.Dimensions) {
			matched = append(matched, s)
		}
	}
	m.mu.RUnlock()
	pMs := int64(periodSec) * 1000
	vals, truncated := collectRaw(matched, startMs, endMs, pMs, limit)
	if tier != nil && !truncated {
		n := 0
		for _, v := range vals {
			n += len(v)
		}
		err := tier.ColdRaw(ctx, tenant, ms, startMs, endMs, func(t int64, v float64) bool {
			if limit > 0 && n >= limit {
				truncated = true
				return false
			}
			b := query.AlignTimestamp(t, pMs)
			vals[b] = append(vals[b], v)
			n++
			return true
		})
		if err != nil {
			return nil, false, err
		}
	}
	return vals, truncated, ctx.Err()
}

// collectRaw scans the raw Gorilla chunks of the given series for samples in
// [startMs, endMs), bucketed by period start. limit <= 0 means no limit.
func collectRaw(matched []*series, startMs, endMs, pMs int64, limit int) (map[int64][]float64, bool) {
	vals := map[int64][]float64{}
	n, truncated := 0, false
	for _, s := range matched {
		s.mu.Lock()
		for _, c := range s.chunks {
			if c.MaxT() < startMs || c.MinT() >= endMs {
				continue // chunk-level pruning by time bounds
			}
			_ = c.Iterate(func(t int64, v float64) bool {
				if t >= startMs && t < endMs {
					if limit > 0 && n >= limit {
						truncated = true
						return false
					}
					b := query.AlignTimestamp(t, pMs)
					vals[b] = append(vals[b], v)
					n++
				}
				return true
			})
		}
		s.mu.Unlock()
		if truncated {
			break
		}
	}
	return vals, truncated
}

// ListSeries returns metadata for series matching the filter.
func (m *Memory) ListSeries(_ context.Context, tenant uint32, f SeriesFilter) ([]model.SeriesMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []model.SeriesMeta{}
	for _, s := range m.series {
		if s.meta.TenantID != tenant {
			continue
		}
		if (f.Namespace != "" && s.meta.Namespace != f.Namespace) || (f.MetricName != "" && s.meta.MetricName != f.MetricName) {
			continue
		}
		if !subsetDims(s.meta.Dimensions, f.Dimensions) {
			continue
		}
		out = append(out, s.meta)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.MetricName != b.MetricName {
			return a.MetricName < b.MetricName
		}
		return a.ID < b.ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// WriteLogs appends to the ring (oldest events are overwritten).
func (m *Memory) WriteLogs(_ context.Context, ls []model.LogEvent) error {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	capN := len(m.logs)
	var over []model.LogEvent
	for _, e := range ls {
		idx := (m.logHead + m.logN) % capN
		if m.logN == capN {
			if m.cfg.Tier != nil {
				over = append(over, m.logs[m.logHead])
			}
			m.logs[m.logHead] = e
			m.logHead = (m.logHead + 1) % capN
		} else {
			m.logs[idx] = e
			m.logN++
		}
	}
	if len(over) > 0 {
		m.cfg.Tier.Archive(&Evicted{Logs: over})
	}
	return nil
}

// SearchLogs scans newest-to-oldest (ring order ≈ arrival order).
func (m *Memory) SearchLogs(ctx context.Context, q LogQuery) (LogResult, error) {
	limit := q.Limit
	if limit <= 0 || limit > 10_000 {
		limit = 500
	}
	terms := Terms(q.Query)
	res, err := m.searchHotLogs(ctx, q, terms, limit)
	if tier := m.cfg.Tier; tier != nil && err == nil && !res.Truncated {
		cold, cerr := tier.ColdLogs(ctx, q, terms, limit-len(res.Events))
		res.Events = append(res.Events, cold.Events...)
		res.Scanned += cold.Scanned
		res.Truncated = cold.Truncated
		res.Partial = cold.Partial
		if cerr != nil {
			res.Partial = "cold tier: " + cerr.Error()
		}
	}
	return res, err
}

func (m *Memory) searchHotLogs(ctx context.Context, q LogQuery, terms []string, limit int) (LogResult, error) {
	m.logMu.RLock()
	defer m.logMu.RUnlock()
	res := LogResult{Events: []model.LogEvent{}}
	capN := len(m.logs)
	for i := m.logN - 1; i >= 0; i-- {
		e := &m.logs[(m.logHead+i)%capN]
		res.Scanned++
		if MatchLog(&q, terms, e) {
			if len(res.Events) >= limit {
				res.Truncated = true
				break
			}
			res.Events = append(res.Events, *e)
		}
		if res.Scanned%4096 == 0 && ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	return res, nil
}

// ListLogGroups summarises groups for a tenant.
func (m *Memory) ListLogGroups(_ context.Context, tenant uint32) ([]LogGroupInfo, error) {
	m.logMu.RLock()
	defer m.logMu.RUnlock()
	groups := map[string]*LogGroupInfo{}
	streams := map[string]map[string]bool{}
	capN := len(m.logs)
	for i := 0; i < m.logN; i++ {
		e := &m.logs[(m.logHead+i)%capN]
		if e.TenantID != tenant {
			continue
		}
		g := groups[e.LogGroup]
		if g == nil {
			g = &LogGroupInfo{Name: e.LogGroup}
			groups[e.LogGroup] = g
			streams[e.LogGroup] = map[string]bool{}
		}
		g.Events++
		if e.Timestamp > g.LastEvent {
			g.LastEvent = e.Timestamp
		}
		streams[e.LogGroup][e.LogStream] = true
	}
	out := make([]LogGroupInfo, 0, len(groups))
	for name, g := range groups {
		for s := range streams[name] {
			g.Streams = append(g.Streams, s)
		}
		sort.Strings(g.Streams)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Stats reports engine internals including the achieved compression ratio.
func (m *Memory) Stats() Stats {
	st := Stats{Backend: "memory"}
	m.mu.RLock()
	st.Series = len(m.series)
	var pts int64
	for _, s := range m.series {
		s.mu.Lock()
		for _, c := range s.chunks {
			st.ChunkBytes += int64(c.Size())
			pts += int64(c.Len())
		}
		st.RollupBucket += len(s.rollup)
		s.mu.Unlock()
	}
	m.mu.RUnlock()
	st.Samples = m.samples.Load()
	if pts > 0 {
		st.BytesPerPt = math.Round(float64(st.ChunkBytes)/float64(pts)*100) / 100
	}
	m.logMu.RLock()
	st.LogEvents = m.logN
	m.logMu.RUnlock()
	return st
}

// Compact enforces retention (exported for tests; the janitor calls it every minute).
func (m *Memory) Compact() {
	if m.cfg.Tier != nil {
		m.compactTiered()
		return
	}
	now := m.now().UnixMilli()
	rawCut := now - m.cfg.RawRetention.Milliseconds()
	rollCut := now - m.cfg.RollupRetention.Milliseconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.series {
		s.mu.Lock()
		kept := s.chunks[:0]
		for _, c := range s.chunks {
			if c.MaxT() >= rawCut {
				kept = append(kept, c)
			}
		}
		s.chunks = kept
		for minute := range s.rollup {
			if minute < rollCut {
				delete(s.rollup, minute)
			}
		}
		empty := len(s.chunks) == 0 && len(s.rollup) == 0
		s.mu.Unlock()
		if empty {
			delete(m.series, id)
			k := nameKey{s.meta.TenantID, s.meta.Namespace, s.meta.MetricName}
			list := m.byName[k]
			for i, x := range list {
				if x == s {
					m.byName[k] = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(m.byName[k]) == 0 {
				delete(m.byName, k)
			}
		}
	}
}

// compactTiered applies the Tier's per-tenant cutoffs and hands everything
// it removes to Tier.Archive, while metric queries are held off (evictMu).
func (m *Memory) compactTiered() {
	tier := m.cfg.Tier
	now := m.now().UnixMilli()
	cuts := map[uint32]Cutoffs{}
	cutFor := func(tenant uint32) Cutoffs {
		c, ok := cuts[tenant]
		if !ok {
			c = tier.Cutoffs(tenant, now)
			cuts[tenant] = c
		}
		return c
	}
	ev := &Evicted{}
	m.evictMu.Lock()
	defer m.evictMu.Unlock()
	m.mu.Lock()
	for id, s := range m.series {
		c := cutFor(s.meta.TenantID)
		s.mu.Lock()
		es := EvictedSeries{Meta: s.meta}
		kept := s.chunks[:0]
		for _, ch := range s.chunks {
			if ch.MaxT() >= c.Raw {
				kept = append(kept, ch)
			} else {
				es.Chunks = append(es.Chunks, ch)
			}
		}
		for i := len(kept); i < len(s.chunks); i++ {
			s.chunks[i] = nil
		}
		s.chunks = kept
		for minute, a := range s.rollup {
			if minute < c.Rollup {
				if es.Rollups == nil {
					es.Rollups = map[int64]Agg{}
				}
				es.Rollups[minute] = *a
				delete(s.rollup, minute)
			}
		}
		empty := len(s.chunks) == 0 && len(s.rollup) == 0
		s.mu.Unlock()
		if len(es.Chunks) > 0 || len(es.Rollups) > 0 {
			ev.Series = append(ev.Series, es)
		}
		if empty {
			delete(m.series, id)
			k := nameKey{s.meta.TenantID, s.meta.Namespace, s.meta.MetricName}
			list := m.byName[k]
			for i, x := range list {
				if x == s {
					m.byName[k] = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(m.byName[k]) == 0 {
				delete(m.byName, k)
			}
		}
	}
	m.mu.Unlock()
	// Logs past their tenant's cutoff leave the ring; the survivors are
	// packed in arrival order behind the head.
	m.logMu.Lock()
	capN := len(m.logs)
	kept := 0
	for i := 0; i < m.logN; i++ {
		e := m.logs[(m.logHead+i)%capN]
		if e.Timestamp < cutFor(e.TenantID).Logs {
			ev.Logs = append(ev.Logs, e)
			continue
		}
		m.logs[(m.logHead+kept)%capN] = e
		kept++
	}
	for i := kept; i < m.logN; i++ {
		m.logs[(m.logHead+i)%capN] = model.LogEvent{}
	}
	m.logN = kept
	m.logMu.Unlock()
	if len(ev.Series) > 0 || len(ev.Logs) > 0 {
		tier.Archive(ev)
	}
}

func (m *Memory) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.Compact()
		}
	}
}

// Close stops background work.
func (m *Memory) Close() error {
	m.once.Do(func() { close(m.stop) })
	return nil
}
