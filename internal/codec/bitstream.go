// Package codec implements the Facebook Gorilla time-series compression
// scheme: delta-of-delta timestamps and XOR floating point values.
//
// Reference: Pelkonen et al., "Gorilla: A Fast, Scalable, In-Memory Time
// Series Database", VLDB 2015.
package codec

import "errors"

// ErrEOF is returned when a reader runs out of bits.
var ErrEOF = errors.New("codec: unexpected end of stream")

// BitWriter appends individual bits / bit-fields MSB-first into a byte slice.
type BitWriter struct {
	buf   []byte
	nbits uint8 // number of free bits in the last byte (0 = need a new byte)
}

// WriteBit appends a single bit.
func (w *BitWriter) WriteBit(bit bool) {
	if w.nbits == 0 {
		w.buf = append(w.buf, 0)
		w.nbits = 8
	}
	if bit {
		w.buf[len(w.buf)-1] |= 1 << (w.nbits - 1)
	}
	w.nbits--
}

// WriteBits appends the low n bits of v, most significant first (n <= 64).
func (w *BitWriter) WriteBits(v uint64, n int) {
	v <<= 64 - uint(n)
	for n >= 8 && w.nbits == 0 {
		// fast path: byte aligned
		w.buf = append(w.buf, byte(v>>56))
		v <<= 8
		n -= 8
	}
	for n > 0 {
		w.WriteBit(v>>63 == 1)
		v <<= 1
		n--
	}
}

// Bytes returns the encoded bytes (the final byte may be partially filled).
func (w *BitWriter) Bytes() []byte { return w.buf }

// BitLen returns the number of bits written.
func (w *BitWriter) BitLen() int { return len(w.buf)*8 - int(w.nbits) }

// BitReader reads bits MSB-first.
type BitReader struct {
	buf []byte
	pos int // bit position
}

// NewBitReader wraps b.
func NewBitReader(b []byte) *BitReader { return &BitReader{buf: b} }

// ReadBit reads one bit.
func (r *BitReader) ReadBit() (bool, error) {
	if r.pos >= len(r.buf)*8 {
		return false, ErrEOF
	}
	b := r.buf[r.pos>>3]&(1<<(7-uint(r.pos&7))) != 0
	r.pos++
	return b, nil
}

// ReadBits reads n bits into the low bits of the result.
func (r *BitReader) ReadBits(n int) (uint64, error) {
	if r.pos+n > len(r.buf)*8 {
		return 0, ErrEOF
	}
	var v uint64
	for n > 0 {
		if r.pos&7 == 0 && n >= 8 {
			v = v<<8 | uint64(r.buf[r.pos>>3])
			r.pos += 8
			n -= 8
			continue
		}
		bit := r.buf[r.pos>>3]&(1<<(7-uint(r.pos&7))) != 0
		v <<= 1
		if bit {
			v |= 1
		}
		r.pos++
		n--
	}
	return v, nil
}
