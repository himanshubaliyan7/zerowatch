package tier

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zerowatch/zerowatch/internal/codec"
	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/storage"
)

// Object layout (one directory per tenant, kind and UTC hour):
//
//	<prefix>/<tenant>/<kind>/<yyyy>/<mm>/<dd>/<hh>/<shard>-<seq>.zwseg
//	<prefix>/<tenant>/<kind>/<yyyy>/<mm>/<dd>/<hh>/manifest.json
//
// kind is raw (Gorilla samples), 1m (minute rollups) or logs. The kind sits
// above the date so each kind's retention deletes whole hourly prefixes.
// Every block in a segment holds one series (or one log group) and only data
// of that hour; the hour's manifest indexes the blocks (id, time bounds,
// offset, length) so a read fetches exactly the blocks it needs with Range
// GETs. Segments are immutable; the manifest is rewritten when a segment is
// added. A segment also ends with its own block index (JSON), a 4-byte
// big-endian length and the magic "ZWS1", so it can be read without the
// manifest.

// Kinds.
const (
	KindRaw    = "raw"
	KindRollup = "1m"
	KindLogs   = "logs"
)

const (
	hourMs       = int64(time.Hour / time.Millisecond)
	segMagic     = "ZWSEG1\n"
	segTailMagic = "ZWS1"
	manifestName = "manifest.json"
)

// hourKey names one hourly directory.
type hourKey struct {
	tenant uint32
	kind   string
	hour   int64 // unix ms of the hour start
}

func hourOf(t int64) int64 {
	h := t / hourMs * hourMs
	if t < 0 && t%hourMs != 0 {
		h -= hourMs
	}
	return h
}

func (k hourKey) dir(prefix string) string {
	t := time.UnixMilli(k.hour).UTC()
	d := fmt.Sprintf("%d/%s/%04d/%02d/%02d/%02d/", k.tenant, k.kind, t.Year(), int(t.Month()), t.Day(), t.Hour())
	if prefix != "" {
		return prefix + "/" + d
	}
	return d
}

// parseKey turns an object key back into its hourKey.
func parseKey(prefix, key string) (hourKey, bool) {
	if prefix != "" {
		if !strings.HasPrefix(key, prefix+"/") {
			return hourKey{}, false
		}
		key = key[len(prefix)+1:]
	}
	p := strings.Split(key, "/")
	if len(p) != 7 {
		return hourKey{}, false
	}
	tenant, err := strconv.ParseUint(p[0], 10, 32)
	if err != nil || (p[1] != KindRaw && p[1] != KindRollup && p[1] != KindLogs) {
		return hourKey{}, false
	}
	t, err := time.Parse("2006/01/02/15", strings.Join(p[2:6], "/"))
	if err != nil {
		return hourKey{}, false
	}
	return hourKey{tenant: uint32(tenant), kind: p[1], hour: t.UnixMilli()}, true
}

// BlockInfo locates one block inside a segment.
type BlockInfo struct {
	ID    uint64 `json:"id,string"`
	Group string `json:"group,omitempty"` // log group (logs)
	MinT  int64  `json:"minT"`
	MaxT  int64  `json:"maxT"`
	N     int    `json:"n"`
	Off   int64  `json:"off"`
	Len   int64  `json:"len"`
}

// SegmentInfo is one segment in a manifest.
type SegmentInfo struct {
	Key    string      `json:"key"`
	Shard  int         `json:"shard"`
	Seq    int         `json:"seq"`
	Bytes  int64       `json:"bytes"`
	MinT   int64       `json:"minT"`
	MaxT   int64       `json:"maxT"`
	Blocks []BlockInfo `json:"blocks"`
}

// Manifest indexes the segments of one hour.
type Manifest struct {
	Version  int                         `json:"version"`
	Tenant   uint32                      `json:"tenant"`
	Kind     string                      `json:"kind"`
	Hour     int64                       `json:"hour"`
	Series   map[string]model.SeriesMeta `json:"series,omitempty"` // id -> meta (raw, 1m)
	Segments []SegmentInfo               `json:"segments"`
}

func (m *Manifest) size() int {
	n := 256 + len(m.Series)*160
	for _, s := range m.Segments {
		n += 128 + len(s.Blocks)*96
	}
	return n
}

// clone copies the parts a writer changes.
func (m *Manifest) clone() *Manifest {
	c := *m
	c.Series = make(map[string]model.SeriesMeta, len(m.Series))
	for k, v := range m.Series {
		c.Series[k] = v
	}
	c.Segments = append([]SegmentInfo(nil), m.Segments...)
	return &c
}

// segmentWriter builds one segment.
type segmentWriter struct {
	buf    bytes.Buffer
	blocks []BlockInfo
}

func newSegmentWriter() *segmentWriter {
	w := &segmentWriter{}
	w.buf.WriteString(segMagic)
	return w
}

func (w *segmentWriter) add(b BlockInfo, payload []byte) {
	b.Off, b.Len = int64(w.buf.Len()), int64(len(payload))
	w.buf.Write(payload)
	w.blocks = append(w.blocks, b)
}

func (w *segmentWriter) finish() ([]byte, []BlockInfo, error) {
	idx, err := json.Marshal(w.blocks)
	if err != nil {
		return nil, nil, err
	}
	w.buf.Write(idx)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(idx)))
	w.buf.Write(n[:])
	w.buf.WriteString(segTailMagic)
	return w.buf.Bytes(), w.blocks, nil
}

// readSegmentIndex returns the block index stored at the end of a segment.
func readSegmentIndex(seg []byte) ([]BlockInfo, error) {
	if len(seg) < len(segMagic)+8 || string(seg[:len(segMagic)]) != segMagic || string(seg[len(seg)-4:]) != segTailMagic {
		return nil, errors.New("tier: not a segment")
	}
	n := int(binary.BigEndian.Uint32(seg[len(seg)-8:]))
	if n > len(seg)-8-len(segMagic) {
		return nil, errors.New("tier: corrupt segment index")
	}
	var out []BlockInfo
	if err := json.Unmarshal(seg[len(seg)-8-n:len(seg)-8], &out); err != nil {
		return nil, fmt.Errorf("tier: segment index: %w", err)
	}
	return out, nil
}

type sample struct {
	t int64
	v float64
}

// encodeRaw writes samples (sorted by time) as one Gorilla chunk.
func encodeRaw(ss []sample) []byte {
	c := codec.NewChunk()
	for _, s := range ss {
		c.Append(s.t, s.v)
	}
	return append([]byte(nil), c.Bytes()...)
}

func decodeRaw(b []byte, n int, fn func(t int64, v float64) bool) error {
	return codec.DecodeChunk(b, n, fn)
}

// encodeRollup writes minute summaries as four Gorilla streams (count, sum,
// min, max), each prefixed by its uvarint length; Gorilla is lossless, so
// the summaries round-trip bit for bit.
func encodeRollup(r map[int64]storage.Agg) ([]byte, int) {
	mins := make([]int64, 0, len(r))
	for m := range r {
		mins = append(mins, m)
	}
	sort.Slice(mins, func(i, j int) bool { return mins[i] < mins[j] })
	var out []byte
	for f := 0; f < 4; f++ {
		c := codec.NewChunk()
		for _, m := range mins {
			a := r[m]
			c.Append(m, [4]float64{a.Count, a.Sum, a.Min, a.Max}[f])
		}
		out = binary.AppendUvarint(out, uint64(c.Size()))
		out = append(out, c.Bytes()...)
	}
	return out, len(mins)
}

func decodeRollup(b []byte, n int, fn func(minute int64, a storage.Agg)) error {
	mins := make([]int64, n)
	vals := make([][4]float64, n)
	for f := 0; f < 4; f++ {
		l, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < l {
			return errors.New("tier: corrupt rollup block")
		}
		i := 0
		err := codec.DecodeChunk(b[k:k+int(l)], n, func(t int64, v float64) bool {
			mins[i] = t
			vals[i][f] = v
			i++
			return true
		})
		if err != nil {
			return err
		}
		b = b[k+int(l):]
	}
	for i, m := range mins {
		fn(m, storage.Agg{Count: vals[i][0], Sum: vals[i][1], Min: vals[i][2], Max: vals[i][3]})
	}
	return nil
}

// encodeLogs gzips the events as a JSON array.
func encodeLogs(es []model.LogEvent) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(es); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeLogs(b []byte) ([]model.LogEvent, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, 512<<20))
	if err != nil {
		return nil, err
	}
	var es []model.LogEvent
	if err := json.Unmarshal(raw, &es); err != nil {
		return nil, err
	}
	return es, nil
}
