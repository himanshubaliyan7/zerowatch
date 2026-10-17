// Package tier moves data that ages out of the memory engine into an
// S3-compatible bucket (AWS S3 or MinIO) and serves it back on queries, and
// applies per-tenant retention locally and in the bucket. It implements
// storage.Tier; see segment.go for the object layout.
//
// Data path: Memory.Compact hands evicted chunks, rollup minutes and log
// events to Archive (queued in memory and still queryable). Flush, run every
// FlushEvery or when the queue grows past MaxQueueBytes, splits the queue by
// tenant, kind and hour, writes one immutable segment per series shard and
// rewrites that hour's manifest. Reads merge the queue, the staged groups and
// the uploaded segments; each piece of data is in exactly one place, so
// results equal an all-hot engine.
package tier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zeebo/xxh3"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

// Config configures a Tier.
type Config struct {
	// Store is the bucket; nil applies retention to local data only.
	Store  *S3
	Prefix string // object key prefix (use one per storage node)
	// Retention is the per-tenant policy (RawHot, Raw, Rollup1m, Logs).
	Retention Retention
	// RollupHot and LogsHot are how long minute rollups and log events stay
	// in memory with a bucket (defaults 7 d and 24 h).
	RollupHot time.Duration
	LogsHot   time.Duration
	// Shards splits an hour's series (or log groups) into this many
	// segments (default 4).
	Shards int
	// FlushEvery (default 1 h) and MaxQueueBytes (default 64 MiB) trigger
	// uploads; JanitorEvery (default 1 h) runs bucket retention.
	FlushEvery    time.Duration
	MaxQueueBytes int64
	JanitorEvery  time.Duration
	// CacheBytes bounds the LRU of manifests and blocks (default 256 MiB).
	CacheBytes int
	// MaxLogScanBytes caps the archived log bytes one search reads (default
	// 64 MiB); past it the result is partial.
	MaxLogScanBytes int64
	Logger          *slog.Logger
	Now             func() time.Time
}

// Stats describe the tier.
type Stats struct {
	Bucket          string `json:"bucket,omitempty"`
	QueuedBytes     int64  `json:"queuedBytes"`
	StagedGroups    int    `json:"stagedGroups"`
	Hours           int    `json:"hours"`
	SegmentsWritten int64  `json:"segmentsWritten"`
	BytesWritten    int64  `json:"bytesWritten"`
	ObjectsDeleted  int64  `json:"objectsDeleted"`
	CacheBytes      int    `json:"cacheBytes"`
	CacheHits       int64  `json:"cacheHits"`
	CacheMisses     int64  `json:"cacheMisses"`
	LastError       string `json:"lastError,omitempty"`
}

type hourState struct{ nseg int } // committed segments; -1 = read the manifest

// group is the decoded data of one hour, waiting for upload.
type group struct {
	key   hourKey
	metas map[uint64]model.SeriesMeta
	raw   map[uint64][]sample
	roll  map[uint64]map[int64]storage.Agg
	logs  []model.LogEvent
}

func newGroup(k hourKey) *group {
	return &group{key: k, metas: map[uint64]model.SeriesMeta{}, raw: map[uint64][]sample{}, roll: map[uint64]map[int64]storage.Agg{}}
}

// Tier implements storage.Tier.
type Tier struct {
	cfg   Config
	cache *lru
	log   *slog.Logger

	mu            sync.Mutex
	queue         []*storage.Evicted
	queueBytes    int64
	materializing []*storage.Evicted
	staged        map[hourKey][]*group
	hours         map[hourKey]*hourState
	stats         Stats

	flushMu sync.Mutex
	kick    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

var _ storage.Tier = (*Tier)(nil)

// New returns a Tier; call Load (with a bucket) and Start.
func New(cfg Config) *Tier {
	if cfg.RollupHot <= 0 {
		cfg.RollupHot = 7 * 24 * time.Hour
	}
	if cfg.LogsHot <= 0 {
		cfg.LogsHot = 24 * time.Hour
	}
	if cfg.Shards <= 0 {
		cfg.Shards = 4
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = time.Hour
	}
	if cfg.MaxQueueBytes <= 0 {
		cfg.MaxQueueBytes = 64 << 20
	}
	if cfg.JanitorEvery <= 0 {
		cfg.JanitorEvery = time.Hour
	}
	if cfg.CacheBytes <= 0 {
		cfg.CacheBytes = 256 << 20
	}
	if cfg.MaxLogScanBytes <= 0 {
		cfg.MaxLogScanBytes = 64 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	t := &Tier{cfg: cfg, cache: newLRU(cfg.CacheBytes), log: cfg.Logger, staged: map[hourKey][]*group{}, hours: map[hourKey]*hourState{},
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	if cfg.Store != nil {
		t.stats.Bucket = cfg.Store.Bucket()
	}
	return t
}

func (t *Tier) listPrefix() string {
	if t.cfg.Prefix == "" {
		return ""
	}
	return t.cfg.Prefix + "/"
}

// Load indexes the hours already in the bucket (one LIST of the prefix).
func (t *Tier) Load(ctx context.Context) error {
	if t.cfg.Store == nil {
		return nil
	}
	objs, err := t.cfg.Store.List(ctx, t.listPrefix())
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, o := range objs {
		if k, ok := parseKey(t.cfg.Prefix, o.Key); ok && t.hours[k] == nil {
			t.hours[k] = &hourState{nseg: -1}
		}
	}
	return nil
}

// Start runs the flush and retention loops until Close.
func (t *Tier) Start() {
	go func() {
		defer close(t.done)
		ft, jt := time.NewTicker(t.cfg.FlushEvery), time.NewTicker(t.cfg.JanitorEvery)
		defer ft.Stop()
		defer jt.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-ft.C:
				t.background(func(ctx context.Context) error { return t.Flush(ctx) })
			case <-t.kick:
				t.background(func(ctx context.Context) error { return t.Flush(ctx) })
			case <-jt.C:
				t.background(func(ctx context.Context) error { _, err := t.RunRetention(ctx); return err })
			}
		}
	}()
}

func (t *Tier) background(fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := fn(ctx); err != nil {
		t.log.Warn("tier", "err", err)
	}
}

// Close stops the loops and uploads what is queued.
func (t *Tier) Close(ctx context.Context) error {
	t.once.Do(func() { close(t.stop) })
	select {
	case <-t.done:
	default:
		// Start was never called.
	}
	return t.Flush(ctx)
}

// Cutoffs implements storage.Tier.
func (t *Tier) Cutoffs(tenant uint32, nowMs int64) storage.Cutoffs {
	p := t.cfg.Retention.For(tenant)
	raw, roll, logs := p.Raw, p.Rollup1m, p.Logs
	if t.cfg.Store != nil {
		raw, roll, logs = min(raw, p.RawHot), min(roll, t.cfg.RollupHot), min(logs, t.cfg.LogsHot)
	}
	return storage.Cutoffs{Raw: nowMs - raw.Milliseconds(), Rollup: nowMs - roll.Milliseconds(), Logs: nowMs - logs.Milliseconds()}
}

// Archive implements storage.Tier. Without a bucket the data is dropped.
func (t *Tier) Archive(e *storage.Evicted) {
	if t.cfg.Store == nil || e == nil {
		return
	}
	var n int64
	for _, s := range e.Series {
		for _, c := range s.Chunks {
			n += int64(c.Size())
		}
		n += int64(len(s.Rollups)) * 48
	}
	for i := range e.Logs {
		n += int64(len(e.Logs[i].Message)) + 96
	}
	t.mu.Lock()
	t.queue = append(t.queue, e)
	t.queueBytes += n
	full := t.queueBytes > t.cfg.MaxQueueBytes
	t.mu.Unlock()
	if full {
		select {
		case t.kick <- struct{}{}:
		default:
		}
	}
}

// materialize decodes queued evictions into hourly groups, dropping data
// already past retention.
func (t *Tier) materialize(batch []*storage.Evicted, nowMs int64) map[hourKey]*group {
	out := map[hourKey]*group{}
	get := func(k hourKey) *group {
		g := out[k]
		if g == nil {
			g = newGroup(k)
			out[k] = g
		}
		return g
	}
	for _, e := range batch {
		for _, s := range e.Series {
			p := t.cfg.Retention.For(s.Meta.TenantID)
			rawCut, rollCut := nowMs-p.Raw.Milliseconds(), nowMs-p.Rollup1m.Milliseconds()
			for _, c := range s.Chunks {
				_ = c.Iterate(func(ts int64, v float64) bool {
					if ts >= rawCut {
						g := get(hourKey{s.Meta.TenantID, KindRaw, hourOf(ts)})
						g.metas[s.Meta.ID] = s.Meta
						g.raw[s.Meta.ID] = append(g.raw[s.Meta.ID], sample{ts, v})
					}
					return true
				})
			}
			for minute, a := range s.Rollups {
				if minute < rollCut {
					continue
				}
				g := get(hourKey{s.Meta.TenantID, KindRollup, hourOf(minute)})
				g.metas[s.Meta.ID] = s.Meta
				r := g.roll[s.Meta.ID]
				if r == nil {
					r = map[int64]storage.Agg{}
					g.roll[s.Meta.ID] = r
				}
				cur := r[minute]
				cur.Merge(a)
				r[minute] = cur
			}
		}
		for _, ev := range e.Logs {
			if ev.Timestamp < nowMs-t.cfg.Retention.For(ev.TenantID).Logs.Milliseconds() {
				continue
			}
			g := get(hourKey{ev.TenantID, KindLogs, hourOf(ev.Timestamp)})
			g.logs = append(g.logs, ev)
		}
	}
	for _, g := range out {
		for _, ss := range g.raw {
			sort.SliceStable(ss, func(i, j int) bool { return ss[i].t < ss[j].t })
		}
	}
	return out
}

// Flush uploads everything queued. Each hour is committed on its own, so a
// failure leaves only the failed hours staged for the next attempt.
func (t *Tier) Flush(ctx context.Context) error {
	if t.cfg.Store == nil {
		return nil
	}
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	now := t.cfg.Now().UnixMilli()
	t.mu.Lock()
	batch := t.queue
	t.queue, t.queueBytes, t.materializing = nil, 0, batch
	t.mu.Unlock()
	groups := t.materialize(batch, now)
	t.mu.Lock()
	for k, g := range groups {
		t.staged[k] = append(t.staged[k], g)
	}
	t.materializing = nil
	snap := make(map[hourKey][]*group, len(t.staged))
	for k, gs := range t.staged {
		snap[k] = append([]*group(nil), gs...)
	}
	t.mu.Unlock()
	keys := make([]hourKey, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].hour < keys[j].hour })
	var errs []error
	for _, k := range keys {
		if err := t.upload(ctx, k, snap[k]); err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	t.mu.Lock()
	if err != nil {
		t.stats.LastError = err.Error()
	}
	t.mu.Unlock()
	return err
}

func mergeGroups(k hourKey, gs []*group) *group {
	if len(gs) == 1 {
		return gs[0]
	}
	m := newGroup(k)
	for _, g := range gs {
		for id, meta := range g.metas {
			m.metas[id] = meta
		}
		for id, ss := range g.raw {
			m.raw[id] = append(m.raw[id], ss...)
		}
		for id, r := range g.roll {
			mr := m.roll[id]
			if mr == nil {
				mr = map[int64]storage.Agg{}
				m.roll[id] = mr
			}
			for minute, a := range r {
				cur := mr[minute]
				cur.Merge(a)
				mr[minute] = cur
			}
		}
		m.logs = append(m.logs, g.logs...)
	}
	for _, ss := range m.raw {
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].t < ss[j].t })
	}
	return m
}

func (t *Tier) manifestKey(k hourKey) string { return k.dir(t.cfg.Prefix) + manifestName }

func (t *Tier) upload(ctx context.Context, k hourKey, gs []*group) error {
	m, err := t.fetchManifest(ctx, k)
	if err != nil {
		return err
	}
	m = m.clone()
	g := mergeGroups(k, gs)
	type blk struct {
		info    BlockInfo
		payload []byte
	}
	shards := make([][]blk, t.cfg.Shards)
	shardOf := func(id uint64) int { return int(id % uint64(t.cfg.Shards)) }
	switch k.kind {
	case KindRaw:
		for id, ss := range g.raw {
			if len(ss) == 0 {
				continue
			}
			shards[shardOf(id)] = append(shards[shardOf(id)], blk{BlockInfo{ID: id, MinT: ss[0].t, MaxT: ss[len(ss)-1].t, N: len(ss)}, encodeRaw(ss)})
		}
	case KindRollup:
		for id, r := range g.roll {
			if len(r) == 0 {
				continue
			}
			b, n := encodeRollup(r)
			lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
			for minute := range r {
				lo, hi = min(lo, minute), max(hi, minute)
			}
			shards[shardOf(id)] = append(shards[shardOf(id)], blk{BlockInfo{ID: id, MinT: lo, MaxT: hi, N: n}, b})
		}
	case KindLogs:
		byGroup := map[string][]model.LogEvent{}
		for _, e := range g.logs {
			byGroup[e.LogGroup] = append(byGroup[e.LogGroup], e)
		}
		for name, es := range byGroup {
			sort.SliceStable(es, func(i, j int) bool { return es[i].Timestamp < es[j].Timestamp })
			b, err := encodeLogs(es)
			if err != nil {
				return err
			}
			id := xxh3.HashString(name)
			shards[shardOf(id)] = append(shards[shardOf(id)], blk{BlockInfo{ID: id, Group: name, MinT: es[0].Timestamp, MaxT: es[len(es)-1].Timestamp, N: len(es)}, b})
		}
	}
	var wrote, wroteBytes int64
	for shard, bs := range shards {
		if len(bs) == 0 {
			continue
		}
		sort.Slice(bs, func(i, j int) bool { return bs[i].info.ID < bs[j].info.ID })
		w := newSegmentWriter()
		lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
		for _, b := range bs {
			w.add(b.info, b.payload)
			lo, hi = min(lo, b.info.MinT), max(hi, b.info.MaxT)
		}
		body, blocks, err := w.finish()
		if err != nil {
			return err
		}
		seq := len(m.Segments)
		key := k.dir(t.cfg.Prefix) + fmt.Sprintf("%02d-%06d.zwseg", shard, seq)
		if err := t.cfg.Store.Put(ctx, key, body, "application/octet-stream"); err != nil {
			return err
		}
		m.Segments = append(m.Segments, SegmentInfo{Key: key, Shard: shard, Seq: seq, Bytes: int64(len(body)), MinT: lo, MaxT: hi, Blocks: blocks})
		wrote++
		wroteBytes += int64(len(body))
	}
	for id, meta := range g.metas {
		if m.Series == nil {
			m.Series = map[string]model.SeriesMeta{}
		}
		m.Series[strconv.FormatUint(id, 10)] = meta
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := t.cfg.Store.Put(ctx, t.manifestKey(k), mb, "application/json"); err != nil {
		return err
	}
	t.mu.Lock()
	hs := t.hours[k]
	if hs == nil {
		hs = &hourState{}
		t.hours[k] = hs
	}
	hs.nseg = len(m.Segments)
	left := t.staged[k][:0]
	for _, x := range t.staged[k] {
		uploaded := false
		for _, y := range gs {
			uploaded = uploaded || x == y
		}
		if !uploaded {
			left = append(left, x)
		}
	}
	if len(left) == 0 {
		delete(t.staged, k)
	} else {
		t.staged[k] = left
	}
	t.stats.SegmentsWritten += wrote
	t.stats.BytesWritten += wroteBytes + int64(len(mb))
	t.mu.Unlock()
	t.cache.put("m:"+t.manifestKey(k), m, m.size())
	return nil
}

// fetchManifest reads an hour's manifest from the bucket (an empty one when
// it does not exist).
func (t *Tier) fetchManifest(ctx context.Context, k hourKey) (*Manifest, error) {
	b, err := t.cfg.Store.Get(ctx, t.manifestKey(k))
	if errors.Is(err, ErrNotFound) {
		return &Manifest{Version: 1, Tenant: k.tenant, Kind: k.kind, Hour: k.hour}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("tier: manifest %s: %w", t.manifestKey(k), err)
	}
	return &m, nil
}

// manifest returns the cached manifest, refetching when it has fewer than
// nseg committed segments.
func (t *Tier) manifest(ctx context.Context, k hourKey, nseg int) (*Manifest, error) {
	ck := "m:" + t.manifestKey(k)
	if v, ok := t.cache.get(ck); ok {
		if m := v.(*Manifest); nseg < 0 || len(m.Segments) >= nseg {
			return m, nil
		}
	}
	m, err := t.fetchManifest(ctx, k)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	fresh := t.hours[k] == nil || len(m.Segments) >= t.hours[k].nseg
	t.mu.Unlock()
	if fresh {
		t.cache.put(ck, m, m.size())
	}
	return m, nil
}

func (t *Tier) block(ctx context.Context, key string, b BlockInfo) ([]byte, error) {
	ck := "b:" + key + "@" + strconv.FormatInt(b.Off, 10)
	if v, ok := t.cache.get(ck); ok {
		return v.([]byte), nil
	}
	data, err := t.cfg.Store.GetRange(ctx, key, b.Off, b.Len)
	if err != nil {
		return nil, err
	}
	t.cache.put(ck, data, len(data)+len(ck)+64)
	return data, nil
}

// view is a consistent snapshot of where a tenant's data of one kind lives.
type view struct {
	queue  []*storage.Evicted
	groups []*group
	hours  map[hourKey]int // committed segment count (-1 = all)
}

func overlaps(hour, startMs, endMs int64) bool { return hour+hourMs > startMs && hour < endMs }

func (t *Tier) view(tenant uint32, kind string, startMs, endMs int64) view {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := view{hours: map[hourKey]int{}}
	v.queue = append(append(v.queue, t.queue...), t.materializing...)
	for k, gs := range t.staged {
		if k.tenant == tenant && k.kind == kind && overlaps(k.hour, startMs, endMs) {
			v.groups = append(v.groups, gs...)
		}
	}
	for k, hs := range t.hours {
		if k.tenant == tenant && k.kind == kind && overlaps(k.hour, startMs, endMs) {
			v.hours[k] = hs.nseg
		}
	}
	return v
}

// matches applies the engine's exact dimension semantics ("*" = any value).
func matches(meta model.SeriesMeta, tenant uint32, ms query.MetricStat) bool {
	if meta.TenantID != tenant || meta.Namespace != ms.Namespace || meta.MetricName != ms.MetricName || len(meta.Dimensions) != len(ms.Dimensions) {
		return false
	}
	for k, v := range ms.Dimensions {
		hv, ok := meta.Dimensions[k]
		if !ok || (v != "*" && hv != v) {
			return false
		}
	}
	return true
}

// coldSeries walks the segments of the hours in v and calls fn with each
// block of a matching series.
func (t *Tier) coldSegments(ctx context.Context, v view, tenant uint32, ms query.MetricStat, startMs, endMs int64, fn func(b BlockInfo, data []byte) (bool, error)) error {
	keys := make([]hourKey, 0, len(v.hours))
	for k := range v.hours {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].hour < keys[j].hour })
	for _, k := range keys {
		m, err := t.manifest(ctx, k, v.hours[k])
		if err != nil {
			return err
		}
		ids := map[uint64]bool{}
		for _, meta := range m.Series {
			if matches(meta, tenant, ms) {
				ids[meta.ID] = true
			}
		}
		if len(ids) == 0 {
			continue
		}
		segs := m.Segments
		if n := v.hours[k]; n >= 0 && n < len(segs) {
			segs = segs[:n]
		}
		for _, seg := range segs {
			if seg.MaxT < startMs || seg.MinT >= endMs {
				continue
			}
			for _, b := range seg.Blocks {
				if !ids[b.ID] || b.MaxT < startMs || b.MinT >= endMs {
					continue
				}
				data, err := t.block(ctx, seg.Key, b)
				if err != nil {
					return err
				}
				if more, err := fn(b, data); err != nil || !more {
					return err
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// ColdRaw implements storage.Tier.
func (t *Tier) ColdRaw(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, fn func(ts int64, v float64) bool) error {
	v := t.view(tenant, KindRaw, startMs, endMs)
	more := true
	emit := func(ts int64, val float64) bool {
		if ts >= startMs && ts < endMs && !fn(ts, val) {
			more = false
		}
		return more
	}
	for _, e := range v.queue {
		for _, s := range e.Series {
			if !matches(s.Meta, tenant, ms) {
				continue
			}
			for _, c := range s.Chunks {
				if c.MaxT() >= startMs && c.MinT() < endMs && more {
					_ = c.Iterate(emit)
				}
			}
		}
	}
	for _, g := range v.groups {
		for id, ss := range g.raw {
			if !matches(g.metas[id], tenant, ms) {
				continue
			}
			for _, s := range ss {
				if !more || !emit(s.t, s.v) {
					break
				}
			}
		}
	}
	if !more {
		return nil
	}
	return t.coldSegments(ctx, v, tenant, ms, startMs, endMs, func(b BlockInfo, data []byte) (bool, error) {
		if err := decodeRaw(data, b.N, emit); err != nil {
			return false, err
		}
		return more, nil
	})
}

// ColdRollups implements storage.Tier.
func (t *Tier) ColdRollups(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, fn func(minute int64, a storage.Agg)) error {
	v := t.view(tenant, KindRollup, startMs, endMs)
	emit := func(minute int64, a storage.Agg) {
		if minute >= startMs && minute < endMs {
			fn(minute, a)
		}
	}
	for _, e := range v.queue {
		for _, s := range e.Series {
			if matches(s.Meta, tenant, ms) {
				for minute, a := range s.Rollups {
					emit(minute, a)
				}
			}
		}
	}
	for _, g := range v.groups {
		for id, r := range g.roll {
			if matches(g.metas[id], tenant, ms) {
				for minute, a := range r {
					emit(minute, a)
				}
			}
		}
	}
	return t.coldSegments(ctx, v, tenant, ms, startMs, endMs, func(b BlockInfo, data []byte) (bool, error) {
		return true, decodeRollup(data, b.N, emit)
	})
}

// ColdLogs implements storage.Tier: archived events, newest hour first,
// reading at most MaxLogScanBytes of segment blocks.
func (t *Tier) ColdLogs(ctx context.Context, q storage.LogQuery, terms []string, limit int) (storage.LogResult, error) {
	res := storage.LogResult{Events: []model.LogEvent{}}
	startMs, endMs := q.StartMs, q.EndMs
	if endMs == 0 {
		endMs = math.MaxInt64
	}
	v := t.view(q.TenantID, KindLogs, startMs, endMs)
	byHour := map[int64][]model.LogEvent{}
	hourSet := map[int64]bool{}
	consider := func(e *model.LogEvent) {
		res.Scanned++
		if storage.MatchLog(&q, terms, e) {
			h := hourOf(e.Timestamp)
			byHour[h] = append(byHour[h], *e)
			hourSet[h] = true
		}
	}
	for _, e := range v.queue {
		for i := range e.Logs {
			consider(&e.Logs[i])
		}
	}
	for _, g := range v.groups {
		for i := range g.logs {
			consider(&g.logs[i])
		}
	}
	for k := range v.hours {
		hourSet[k.hour] = true
	}
	hours := make([]int64, 0, len(hourSet))
	for h := range hourSet {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] > hours[j] })
	var read int64
	for _, h := range hours {
		evs := byHour[h]
		k := hourKey{q.TenantID, KindLogs, h}
		stop := false
		if n, ok := v.hours[k]; ok {
			m, err := t.manifest(ctx, k, n)
			if err != nil {
				return res, err
			}
			segs := m.Segments
			if n >= 0 && n < len(segs) {
				segs = segs[:n]
			}
		segLoop:
			for _, seg := range segs {
				for _, b := range seg.Blocks {
					if (q.LogGroup != "" && b.Group != q.LogGroup) || b.MaxT < startMs || b.MinT >= endMs {
						continue
					}
					if read+b.Len > t.cfg.MaxLogScanBytes {
						res.Partial = fmt.Sprintf("archived log search stopped after reading %d KiB (cap %d KiB); narrow the time range or pick a log group", read>>10, t.cfg.MaxLogScanBytes>>10)
						stop = true
						break segLoop
					}
					data, err := t.block(ctx, seg.Key, b)
					if err != nil {
						return res, err
					}
					read += b.Len
					es, err := decodeLogs(data)
					if err != nil {
						return res, err
					}
					for i := range es {
						res.Scanned++
						if storage.MatchLog(&q, terms, &es[i]) {
							evs = append(evs, es[i])
						}
					}
				}
			}
		}
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].Timestamp > evs[j].Timestamp })
		for _, e := range evs {
			if len(res.Events) >= limit {
				res.Truncated = true
				return res, nil
			}
			res.Events = append(res.Events, e)
		}
		if stop {
			break
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// RunRetention deletes the bucket's hourly prefixes past each tenant's
// retention and drops staged data past it. It returns the objects deleted.
// Local data is trimmed by the memory engine's Compact through Cutoffs.
func (t *Tier) RunRetention(ctx context.Context) (int, error) {
	if t.cfg.Store == nil {
		return 0, nil
	}
	now := t.cfg.Now().UnixMilli()
	expired := func(k hourKey) bool {
		p := t.cfg.Retention.For(k.tenant)
		age := p.Raw
		switch k.kind {
		case KindRollup:
			age = p.Rollup1m
		case KindLogs:
			age = p.Logs
		}
		return k.hour+hourMs <= now-age.Milliseconds()
	}
	t.mu.Lock()
	var gone []hourKey
	for k := range t.hours {
		if expired(k) {
			gone = append(gone, k)
			delete(t.hours, k) // readers stop looking before objects go
		}
	}
	for k := range t.staged {
		if expired(k) {
			delete(t.staged, k)
		}
	}
	t.mu.Unlock()
	deleted := 0
	var errs []error
	for _, k := range gone {
		dir := k.dir(t.cfg.Prefix)
		objs, err := t.cfg.Store.List(ctx, dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, o := range objs {
			if err := t.cfg.Store.Delete(ctx, o.Key); err != nil {
				errs = append(errs, err)
				continue
			}
			deleted++
		}
		t.cache.removePrefix("m:" + dir)
		t.cache.removePrefix("b:" + dir)
	}
	err := errors.Join(errs...)
	t.mu.Lock()
	t.stats.ObjectsDeleted += int64(deleted)
	if err != nil {
		t.stats.LastError = err.Error()
		// Retry the failed hours next run.
		for _, k := range gone {
			if t.hours[k] == nil {
				t.hours[k] = &hourState{nseg: -1}
			}
		}
	}
	t.mu.Unlock()
	return deleted, err
}

// Stats reports the tier's state.
func (t *Tier) Stats() Stats {
	t.mu.Lock()
	st := t.stats
	st.QueuedBytes = t.queueBytes
	for _, gs := range t.staged {
		st.StagedGroups += len(gs)
	}
	st.Hours = len(t.hours)
	t.mu.Unlock()
	st.CacheBytes, st.CacheHits, st.CacheMisses = t.cache.stats()
	return st
}
