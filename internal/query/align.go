package query

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/zerowatch/zerowatch/internal/model"
)

// Grid is the unified time grid T = [t0, t0+Δt, ..., tN] all series are
// aligned onto before vector operations.
type Grid struct {
	Start    int64 // unix ms, aligned
	PeriodMs int64
	N        int
}

// NewGrid builds a grid covering [start, end) with t0 = floor(start/period)*period.
func NewGrid(startMs, endMs int64, periodSec int) (Grid, error) {
	if periodSec <= 0 {
		return Grid{}, fmt.Errorf("period must be > 0")
	}
	if endMs <= startMs {
		return Grid{}, fmt.Errorf("end must be after start")
	}
	p := int64(periodSec) * 1000
	t0 := AlignTimestamp(startMs, p)
	n := int((endMs - t0 + p - 1) / p)
	const maxPoints = 100_800 // CloudWatch GetMetricData per-series point cap
	if n > maxPoints {
		return Grid{}, fmt.Errorf("query would return %d points per series (max %d); increase period", n, maxPoints)
	}
	return Grid{Start: t0, PeriodMs: p, N: n}, nil
}

// AlignTimestamp floors t to the period boundary: floor(t/P)*P (handles negatives).
func AlignTimestamp(t, periodMs int64) int64 {
	q := t / periodMs
	if t%periodMs != 0 && t < 0 {
		q--
	}
	return q * periodMs
}

// Timestamps returns the grid's timestamps.
func (g Grid) Timestamps() []int64 {
	out := make([]int64, g.N)
	for i := range out {
		out[i] = g.Start + int64(i)*g.PeriodMs
	}
	return out
}

// Index returns the bucket index for t, or -1 if outside.
func (g Grid) Index(t int64) int {
	i := int((AlignTimestamp(t, g.PeriodMs) - g.Start) / g.PeriodMs)
	if i < 0 || i >= g.N {
		return -1
	}
	return i
}

// Align maps points onto the grid. Multiple points falling into one bucket are
// averaged (storage usually pre-aggregates per period, so this mostly handles
// clock jitter). Missing buckets are NaN.
func Align(points []model.Point, g Grid) []float64 {
	out := make([]float64, g.N)
	cnt := make([]int, g.N)
	for i := range out {
		out[i] = math.NaN()
	}
	for _, p := range points {
		i := g.Index(p.T)
		if i < 0 || math.IsNaN(p.V) {
			continue
		}
		if cnt[i] == 0 {
			out[i] = p.V
		} else {
			out[i] += p.V
		}
		cnt[i]++
	}
	for i, c := range cnt {
		if c > 1 {
			out[i] /= float64(c)
		}
	}
	return out
}

// FillPolicy controls synthesis of missing aligned points.
type FillPolicy struct {
	Kind  string // NONE | VALUE | REPEAT | LINEAR
	Value float64
}

// ParseFillPolicy accepts NONE, REPEAT, LINEAR or a number (e.g. "0").
func ParseFillPolicy(s string) (FillPolicy, error) {
	switch u := strings.ToUpper(strings.TrimSpace(s)); u {
	case "", "NONE":
		return FillPolicy{Kind: "NONE"}, nil
	case "REPEAT", "LINEAR":
		return FillPolicy{Kind: u}, nil
	default:
		f, err := strconv.ParseFloat(u, 64)
		if err != nil {
			return FillPolicy{}, fmt.Errorf("invalid fill policy %q", s)
		}
		return FillPolicy{Kind: "VALUE", Value: f}, nil
	}
}

// Fill returns a copy of v with NaNs replaced per policy.
//   - VALUE:  constant
//   - REPEAT: carry the last observed value forward (leading gaps take the
//     first observed value, matching CloudWatch's FILL(m, REPEAT))
//   - LINEAR: linear interpolation between neighbours; edges repeat nearest
func Fill(v []float64, p FillPolicy) []float64 {
	out := append([]float64(nil), v...)
	switch p.Kind {
	case "VALUE":
		for i := range out {
			if math.IsNaN(out[i]) {
				out[i] = p.Value
			}
		}
	case "REPEAT":
		last := math.NaN()
		for i := range out {
			if math.IsNaN(out[i]) {
				out[i] = last
			} else {
				last = out[i]
			}
		}
		// back-fill leading gap
		first := -1
		for i := range out {
			if !math.IsNaN(out[i]) {
				first = i
				break
			}
		}
		for i := 0; i < first; i++ {
			out[i] = out[first]
		}
	case "LINEAR":
		prev := -1
		for i := range out {
			if math.IsNaN(out[i]) {
				continue
			}
			if prev >= 0 && i-prev > 1 {
				step := (out[i] - out[prev]) / float64(i-prev)
				for k := prev + 1; k < i; k++ {
					out[k] = out[prev] + step*float64(k-prev)
				}
			} else if prev < 0 {
				for k := 0; k < i; k++ {
					out[k] = out[i]
				}
			}
			prev = i
		}
		if prev >= 0 {
			for k := prev + 1; k < len(out); k++ {
				out[k] = out[prev]
			}
		}
	}
	return out
}
