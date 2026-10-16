package codec

import (
	"math"
	"math/rand"
	"testing"
)

func roundTrip(t *testing.T, ts []int64, vs []float64) *Chunk {
	t.Helper()
	c := NewChunk()
	for i := range ts {
		c.Append(ts[i], vs[i])
	}
	i := 0
	err := c.Iterate(func(gt int64, gv float64) bool {
		if gt != ts[i] {
			t.Fatalf("sample %d: ts got %d want %d", i, gt, ts[i])
		}
		if math.Float64bits(gv) != math.Float64bits(vs[i]) {
			t.Fatalf("sample %d: v got %v want %v", i, gv, vs[i])
		}
		i++
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if i != len(ts) {
		t.Fatalf("decoded %d samples want %d", i, len(ts))
	}
	return c
}

func TestRegularSeriesCompressesWell(t *testing.T) {
	n := 1000
	ts := make([]int64, n)
	vs := make([]float64, n)
	base := int64(1_700_000_000_000)
	for i := 0; i < n; i++ {
		ts[i] = base + int64(i)*60_000
		vs[i] = 42.0
	}
	c := roundTrip(t, ts, vs)
	// Constant value + regular interval => ~2 bits/sample after the header.
	if bps := float64(c.Size()*8) / float64(n); bps > 3 {
		t.Fatalf("expected <= 3 bits/sample, got %.2f", bps)
	}
}

func TestJitteryRandomSeries(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	n := 5000
	ts := make([]int64, n)
	vs := make([]float64, n)
	cur := int64(1_700_000_000_000)
	v := 100.0
	for i := 0; i < n; i++ {
		cur += 1000 + int64(r.Intn(400)-200) // ±200ms jitter
		if r.Intn(50) == 0 {
			cur += int64(r.Intn(10_000_000)) // occasional big gap -> 64-bit bucket
		}
		ts[i] = cur
		v += r.NormFloat64()
		vs[i] = v
	}
	roundTrip(t, ts, vs)
}

func TestSpecialFloats(t *testing.T) {
	ts := []int64{0, 1, 2, 3, 4, 5, 6, 7}
	vs := []float64{0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), math.MaxFloat64, math.SmallestNonzeroFloat64, 1e-300, -12.5}
	roundTrip(t, ts, vs)
	nan := NewChunk()
	nan.Append(1, math.NaN())
	nan.Append(2, 3)
	_ = nan.Iterate(func(tt int64, v float64) bool {
		if tt == 1 && !math.IsNaN(v) {
			t.Fatal("NaN not preserved")
		}
		return true
	})
}

func TestOutOfOrderAndNegativeTimestamps(t *testing.T) {
	ts := []int64{100, 50, 200, -1000, 0, math.MaxInt64 / 4}
	vs := []float64{1, 2, 3, 4, 5, 6}
	roundTrip(t, ts, vs)
}

func TestTimestampBuckets(t *testing.T) {
	// exercise every delta-of-delta bucket boundary
	deltas := []int64{60, 60, 60 + 64, 60 - 63, 60 + 256, 60 - 255, 60 + 2048, 60 - 2047, 60 + 2049, 60 - 3000}
	ts := []int64{1000}
	for _, d := range deltas {
		ts = append(ts, ts[len(ts)-1]+d)
	}
	vs := make([]float64, len(ts))
	roundTrip(t, ts, vs)
}

func TestTruncatedStream(t *testing.T) {
	c := NewChunk()
	for i := 0; i < 10; i++ {
		c.Append(int64(i*1000), float64(i)*1.5)
	}
	b := c.Bytes()
	if err := DecodeChunk(b[:len(b)/2], 10, func(int64, float64) bool { return true }); err == nil {
		t.Fatal("expected error decoding truncated chunk")
	}
}

func TestBitWriterReader(t *testing.T) {
	var w BitWriter
	w.WriteBits(0b101, 3)
	w.WriteBits(0xDEADBEEF, 32)
	w.WriteBit(true)
	r := NewBitReader(w.Bytes())
	if v, _ := r.ReadBits(3); v != 0b101 {
		t.Fatalf("got %b", v)
	}
	if v, _ := r.ReadBits(32); v != 0xDEADBEEF {
		t.Fatalf("got %x", v)
	}
	if b, _ := r.ReadBit(); !b {
		t.Fatal("bit")
	}
	if w.BitLen() != 36 {
		t.Fatalf("bitlen %d", w.BitLen())
	}
}

func BenchmarkChunkAppend(b *testing.B) {
	c := NewChunk()
	for i := 0; i < b.N; i++ {
		if c.Len() >= 1024 {
			c = NewChunk()
		}
		c.Append(int64(i)*1000, float64(i%100))
	}
}
