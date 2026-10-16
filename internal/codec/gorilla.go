package codec

import (
	"math"
	"math/bits"
)

// ---------------------------------------------------------------------------
// Timestamps: delta-of-delta
//
//	D = (t_i - t_{i-1}) - (t_{i-1} - t_{i-2})
//
// Encoding buckets (paper §4.1.1, widened to 64-bit for millisecond clocks):
//
//	D == 0                  -> '0'
//	D in [-63, 64]          -> '10'   + 7 bits
//	D in [-255, 256]        -> '110'  + 9 bits
//	D in [-2047, 2048]      -> '1110' + 12 bits
//	otherwise               -> '1111' + 64 bits
//
// The first timestamp is written raw (64 bits); the first delta is encoded as
// a delta-of-delta against an implicit previous delta of 0.
// ---------------------------------------------------------------------------

// TimestampEncoder compresses a monotone-ish stream of int64 timestamps.
type TimestampEncoder struct {
	w      *BitWriter
	n      int
	prev   int64
	prevDt int64
}

// NewTimestampEncoder creates an encoder writing to w.
func NewTimestampEncoder(w *BitWriter) *TimestampEncoder { return &TimestampEncoder{w: w} }

// Encode appends t.
func (e *TimestampEncoder) Encode(t int64) {
	if e.n == 0 {
		e.w.WriteBits(uint64(t), 64)
		e.prev = t
		e.n++
		return
	}
	dt := t - e.prev
	dod := dt - e.prevDt
	switch {
	case dod == 0:
		e.w.WriteBit(false)
	case dod >= -63 && dod <= 64:
		e.w.WriteBits(0b10, 2)
		e.w.WriteBits(uint64(dod+63), 7)
	case dod >= -255 && dod <= 256:
		e.w.WriteBits(0b110, 3)
		e.w.WriteBits(uint64(dod+255), 9)
	case dod >= -2047 && dod <= 2048:
		e.w.WriteBits(0b1110, 4)
		e.w.WriteBits(uint64(dod+2047), 12)
	default:
		e.w.WriteBits(0b1111, 4)
		e.w.WriteBits(uint64(dod), 64)
	}
	e.prev, e.prevDt = t, dt
	e.n++
}

// TimestampDecoder is the inverse of TimestampEncoder.
type TimestampDecoder struct {
	r      *BitReader
	n      int
	prev   int64
	prevDt int64
}

// NewTimestampDecoder reads from r.
func NewTimestampDecoder(r *BitReader) *TimestampDecoder { return &TimestampDecoder{r: r} }

// Decode returns the next timestamp.
func (d *TimestampDecoder) Decode() (int64, error) {
	if d.n == 0 {
		v, err := d.r.ReadBits(64)
		if err != nil {
			return 0, err
		}
		d.prev = int64(v)
		d.n++
		return d.prev, nil
	}
	// count leading 1 bits of the control prefix (max 4)
	ctrl := 0
	for ctrl < 4 {
		b, err := d.r.ReadBit()
		if err != nil {
			return 0, err
		}
		if !b {
			break
		}
		ctrl++
	}
	var dod int64
	switch ctrl {
	case 0:
		dod = 0
	case 1:
		v, err := d.r.ReadBits(7)
		if err != nil {
			return 0, err
		}
		dod = int64(v) - 63
	case 2:
		v, err := d.r.ReadBits(9)
		if err != nil {
			return 0, err
		}
		dod = int64(v) - 255
	case 3:
		v, err := d.r.ReadBits(12)
		if err != nil {
			return 0, err
		}
		dod = int64(v) - 2047
	case 4:
		v, err := d.r.ReadBits(64)
		if err != nil {
			return 0, err
		}
		dod = int64(v)
	}
	dt := d.prevDt + dod
	d.prev += dt
	d.prevDt = dt
	d.n++
	return d.prev, nil
}

// ---------------------------------------------------------------------------
// Values: XOR float compression (paper §4.1.2)
//
//	first value          -> 64 raw bits
//	xor == 0             -> '0'
//	xor fits previous    -> '10' + meaningful bits (reuse leading/trailing window)
//	otherwise            -> '11' + 5 bits leading zeros + 6 bits length + bits
//
// A 6-bit length of 0 encodes 64 meaningful bits.
// ---------------------------------------------------------------------------

// ValueEncoder compresses float64 values.
type ValueEncoder struct {
	w        *BitWriter
	n        int
	prev     uint64
	leading  int
	trailing int
}

// NewValueEncoder creates a value encoder writing to w.
func NewValueEncoder(w *BitWriter) *ValueEncoder { return &ValueEncoder{w: w, leading: -1} }

// Encode appends v.
func (e *ValueEncoder) Encode(v float64) {
	cur := math.Float64bits(v)
	if e.n == 0 {
		e.w.WriteBits(cur, 64)
		e.prev = cur
		e.n++
		return
	}
	x := cur ^ e.prev
	e.prev = cur
	e.n++
	if x == 0 {
		e.w.WriteBit(false)
		return
	}
	e.w.WriteBit(true)
	lead := bits.LeadingZeros64(x)
	trail := bits.TrailingZeros64(x)
	if lead > 31 { // must fit in 5 bits
		lead = 31
	}
	if e.leading >= 0 && lead >= e.leading && trail >= e.trailing {
		e.w.WriteBit(false)
		sig := 64 - e.leading - e.trailing
		e.w.WriteBits(x>>uint(e.trailing), sig)
		return
	}
	e.leading, e.trailing = lead, trail
	sig := 64 - lead - trail
	e.w.WriteBit(true)
	e.w.WriteBits(uint64(lead), 5)
	e.w.WriteBits(uint64(sig&63), 6) // 64 -> 0
	e.w.WriteBits(x>>uint(trail), sig)
}

// ValueDecoder is the inverse of ValueEncoder.
type ValueDecoder struct {
	r        *BitReader
	n        int
	prev     uint64
	leading  int
	trailing int
}

// NewValueDecoder reads from r.
func NewValueDecoder(r *BitReader) *ValueDecoder { return &ValueDecoder{r: r} }

// Decode returns the next value.
func (d *ValueDecoder) Decode() (float64, error) {
	if d.n == 0 {
		v, err := d.r.ReadBits(64)
		if err != nil {
			return 0, err
		}
		d.prev = v
		d.n++
		return math.Float64frombits(v), nil
	}
	d.n++
	b, err := d.r.ReadBit()
	if err != nil {
		return 0, err
	}
	if !b {
		return math.Float64frombits(d.prev), nil
	}
	b, err = d.r.ReadBit()
	if err != nil {
		return 0, err
	}
	if b {
		lead, err := d.r.ReadBits(5)
		if err != nil {
			return 0, err
		}
		sig, err := d.r.ReadBits(6)
		if err != nil {
			return 0, err
		}
		if sig == 0 {
			sig = 64
		}
		d.leading = int(lead)
		d.trailing = 64 - int(lead) - int(sig)
	}
	sig := 64 - d.leading - d.trailing
	v, err := d.r.ReadBits(sig)
	if err != nil {
		return 0, err
	}
	d.prev ^= v << uint(d.trailing)
	return math.Float64frombits(d.prev), nil
}

// ---------------------------------------------------------------------------
// Chunk: interleaved (t, v) pairs in a single bit stream, as stored by the
// embedded TSDB. Interleaving keeps decode single-pass and cache friendly.
// ---------------------------------------------------------------------------

// Chunk is an append-only compressed block of samples.
type Chunk struct {
	w     BitWriter
	te    *TimestampEncoder
	ve    *ValueEncoder
	count int
	minT  int64
	maxT  int64
}

// NewChunk returns an empty chunk.
func NewChunk() *Chunk {
	c := &Chunk{}
	c.te = NewTimestampEncoder(&c.w)
	c.ve = NewValueEncoder(&c.w)
	return c
}

// Append adds a sample. Timestamps should be non-decreasing for best ratio
// (out-of-order values still round-trip correctly).
func (c *Chunk) Append(t int64, v float64) {
	if c.count == 0 || t < c.minT {
		c.minT = t
	}
	if c.count == 0 || t > c.maxT {
		c.maxT = t
	}
	c.te.Encode(t)
	c.ve.Encode(v)
	c.count++
}

// Len returns the number of samples.
func (c *Chunk) Len() int { return c.count }

// MinT / MaxT return the time bounds.
func (c *Chunk) MinT() int64 { return c.minT }
func (c *Chunk) MaxT() int64 { return c.maxT }

// Bytes returns the compressed payload.
func (c *Chunk) Bytes() []byte { return c.w.Bytes() }

// Size returns the compressed size in bytes.
func (c *Chunk) Size() int { return len(c.w.Bytes()) }

// Iterate decodes every sample, calling fn until it returns false.
func (c *Chunk) Iterate(fn func(t int64, v float64) bool) error {
	return DecodeChunk(c.w.Bytes(), c.count, fn)
}

// DecodeChunk decodes count interleaved samples from b.
func DecodeChunk(b []byte, count int, fn func(t int64, v float64) bool) error {
	r := NewBitReader(b)
	td := NewTimestampDecoder(r)
	vd := NewValueDecoder(r)
	for i := 0; i < count; i++ {
		t, err := td.Decode()
		if err != nil {
			return err
		}
		v, err := vd.Decode()
		if err != nil {
			return err
		}
		if !fn(t, v) {
			return nil
		}
	}
	return nil
}
