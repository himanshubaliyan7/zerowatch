package anomaly

// STL: Seasonal-Trend decomposition using Loess (Cleveland, Cleveland,
// McRae & Terpenning, 1990, J. Official Statistics 6(1):3-73).
//
// y = T + S + R. The inner loop alternates
//
//  1. detrend:            y − T
//  2. cycle-subseries:    loess (n_s) of each phase's subseries, extended
//     one period at both ends → C (length n+2·n_p)
//  3. low-pass:           MA(n_p) ∘ MA(n_p) ∘ MA(3) of C, then loess (n_l) → L
//  4. seasonal:           S = C − L   (removes low-frequency leakage)
//  5. deseasonalise:      y − S
//  6. trend:              loess (n_t) → T
//
// The optional outer loop recomputes bisquare robustness weights from
// R = y − T − S (B(|R|/6·median|R|)) and feeds them into steps 2 and 6, so
// outliers stop dragging the trend and seasonal components. The loess
// routines follow the reference Fortran (stless/stlest), including the
// "jump" optimisation: loess is evaluated every jump-th point and linearly
// interpolated in between.
//
// Missing values (NaN) get weight zero in every loess fit; where a window has
// no usable point the fit is linearly interpolated from its neighbours.

import (
	"errors"
	"math"
	"sort"
)

// STLParams configure STL. Zero values select the paper's defaults.
type STLParams struct {
	Period int // n_p, points per seasonal cycle (>= 2)

	// Loess window lengths (odd, >= 3). Seasonal 0 => 7; Trend 0 => smallest
	// odd >= 1.5·n_p/(1−1.5/n_s); LowPass 0 => smallest odd >= n_p.
	Seasonal, Trend, LowPass int

	// Use a locally constant (degree 0) fit instead of locally linear.
	SeasonalConstant, TrendConstant, LowPassConstant bool

	// Evaluate loess every jump-th point and interpolate; 0 => ceil(len/10).
	SeasonalJump, TrendJump, LowPassJump int

	// Robust enables the outer loop with bisquare weights. Inner 0 => 2 when
	// robust, 5 otherwise; Outer 0 => 15 when robust. Outer > 0 implies robust.
	Robust       bool
	Inner, Outer int
}

// DefaultSTLParams returns the non-robust defaults for a period. Set Robust
// when the series may contain outliers (DetectSTL callers usually should).
func DefaultSTLParams(period int) STLParams { return STLParams{Period: period} }

// stlConfig is STLParams with every default resolved.
type stlConfig struct {
	np, ns, nt, nl      int
	sdeg, tdeg, ldeg    int
	sjump, tjump, ljump int
	inner, outer        int
}

func oddAtLeast(x float64) int {
	v := int(math.Ceil(x))
	if v%2 == 0 {
		v++
	}
	return v
}

func (p STLParams) resolve() (stlConfig, error) {
	if p.Period < 2 {
		return stlConfig{}, errors.New("anomaly: STL period must be >= 2")
	}
	for _, w := range []int{p.Seasonal, p.Trend, p.LowPass} {
		if w != 0 && (w < 3 || w%2 == 0) {
			return stlConfig{}, errors.New("anomaly: STL window lengths must be odd and >= 3")
		}
	}
	for _, v := range []int{p.SeasonalJump, p.TrendJump, p.LowPassJump, p.Inner, p.Outer} {
		if v < 0 {
			return stlConfig{}, errors.New("anomaly: STL jumps and iteration counts must be >= 0")
		}
	}
	c := stlConfig{np: p.Period, ns: p.Seasonal, nt: p.Trend, nl: p.LowPass, inner: p.Inner, outer: p.Outer}
	if c.ns == 0 {
		c.ns = 7
	}
	if c.nt == 0 {
		c.nt = oddAtLeast(1.5 * float64(c.np) / (1 - 1.5/float64(c.ns)))
		if c.nt < 3 {
			c.nt = 3
		}
	}
	if c.nl == 0 {
		c.nl = oddAtLeast(float64(c.np))
	}
	deg := func(constant bool) int {
		if constant {
			return 0
		}
		return 1
	}
	c.sdeg, c.tdeg, c.ldeg = deg(p.SeasonalConstant), deg(p.TrendConstant), deg(p.LowPassConstant)
	jump := func(j, w int) int {
		if j == 0 {
			return (w + 9) / 10
		}
		return j
	}
	c.sjump, c.tjump, c.ljump = jump(p.SeasonalJump, c.ns), jump(p.TrendJump, c.nt), jump(p.LowPassJump, c.nl)
	robust := p.Robust || p.Outer > 0
	if robust && c.outer == 0 {
		c.outer = 15
	}
	if c.inner == 0 {
		c.inner = 5
		if robust {
			c.inner = 2
		}
	}
	return c, nil
}

// STL decomposes y into trend + seasonal + residual. y needs at least two
// full periods of finite values; NaN marks a missing point, whose trend and
// seasonal are still estimated and whose residual is NaN. For every finite
// y[i], trend[i]+seasonal[i]+resid[i] == y[i] up to float rounding.
func STL(y []float64, p STLParams) (trend, seasonal, resid []float64, err error) {
	trend, seasonal, resid, _, err = stl(y, p)
	return
}

// stl is STL that also returns the resolved configuration.
func stl(y []float64, p STLParams) (trend, seasonal, resid []float64, c stlConfig, err error) {
	c, err = p.resolve()
	if err != nil {
		return
	}
	n := len(y)
	if n < 2*c.np {
		err = ErrInsufficientData
		return
	}
	// x is y with missing points zeroed; mask carries their zero weight.
	x := make([]float64, n)
	mask := make([]float64, n)
	finiteN := 0
	for i, v := range y {
		switch {
		case math.IsInf(v, 0):
			err = errors.New("anomaly: STL input contains Inf")
			return
		case math.IsNaN(v):
		default:
			x[i], mask[i] = v, 1
			finiteN++
		}
	}
	if finiteN < 2*c.np {
		err = ErrInsufficientData
		return
	}

	s := newSTLState(n, c)
	rw := make([]float64, n)
	copy(rw, mask)
	trend, seasonal = make([]float64, n), make([]float64, n)
	resid = make([]float64, n)
	// The bisquare cutoff 6·median|R| collapses on noise-free input (the
	// median residual is rounding error), which would zero the weight of
	// nearly every point; floor it at 0.6% of the series' own spread.
	cutFloor := 6e-3 * robustScale(y)
	for k := 0; ; k++ {
		s.inner(x, rw, trend, seasonal)
		if k >= c.outer {
			break
		}
		for i := range x {
			resid[i] = x[i] - trend[i] - seasonal[i]
		}
		robustnessWeights(resid, mask, rw, cutFloor)
	}
	for i := range y {
		resid[i] = y[i] - trend[i] - seasonal[i] // NaN stays NaN
	}
	return trend, seasonal, resid, c, nil
}

// stlState holds the scratch buffers of one decomposition.
type stlState struct {
	c                 stlConfig
	n                 int
	w, cyc, ma1, ma2  []float64
	sub, subw, subOut []float64
	work, low, unitW  []float64
}

func newSTLState(n int, c stlConfig) *stlState {
	k := n/c.np + 1 // longest cycle-subseries
	m := n + 2*c.np
	unit := make([]float64, m)
	for i := range unit {
		unit[i] = 1
	}
	return &stlState{
		c: c, n: n,
		w: make([]float64, n), cyc: make([]float64, m),
		ma1: make([]float64, m), ma2: make([]float64, m),
		sub: make([]float64, k), subw: make([]float64, k), subOut: make([]float64, k+2),
		work: make([]float64, m), low: make([]float64, n), unitW: unit,
	}
}

// inner runs c.inner passes of the STL inner loop, updating trend and
// seasonal in place (trend is the starting estimate, zero on first entry).
func (s *stlState) inner(x, rw, trend, seasonal []float64) {
	c, n, np := s.c, s.n, s.c.np
	for it := 0; it < c.inner; it++ {
		for i := range x {
			s.w[i] = x[i] - trend[i]
		}
		s.cycleSubseries(rw)
		// low-pass filter of C: n+2np -> n+np+1 -> n+2 -> n, then loess.
		movingAverage(s.cyc[:n+2*np], np, s.ma1)
		movingAverage(s.ma1[:n+np+1], np, s.ma2)
		movingAverage(s.ma2[:n+2], 3, s.ma1)
		loessSmooth(s.ma1[:n], s.unitW[:n], c.nl, c.ldeg, c.ljump, s.low, s.work)
		for i := 0; i < n; i++ {
			seasonal[i] = s.cyc[np+i] - s.low[i]
			s.w[i] = x[i] - seasonal[i]
		}
		loessSmooth(s.w, rw, c.nt, c.tdeg, c.tjump, trend, s.work)
	}
}

// cycleSubseries smooths each phase's subseries of s.w with loess (n_s) and
// extrapolates one cycle at each end, writing C into s.cyc (length n+2np).
func (s *stlState) cycleSubseries(rw []float64) {
	c, n, np := s.c, s.n, s.c.np
	for j := 0; j < np; j++ {
		k := (n-j-1)/np + 1
		sub, subw, out := s.sub[:k], s.subw[:k], s.subOut[:k+2]
		for m := 0; m < k; m++ {
			sub[m], subw[m] = s.w[m*np+j], rw[m*np+j]
		}
		loessSmooth(sub, subw, c.ns, c.sdeg, c.sjump, out[1:k+1], s.work)
		// extrapolate to positions -1 and k of the subseries
		nright := min(c.ns, k) - 1
		if v, ok := loessEst(sub, subw, c.ns, c.sdeg, -1, 0, nright, s.work); ok {
			out[0] = v
		} else {
			out[0] = out[1]
		}
		nleft := max(0, k-c.ns)
		if v, ok := loessEst(sub, subw, c.ns, c.sdeg, float64(k), nleft, k-1, s.work); ok {
			out[k+1] = v
		} else {
			out[k+1] = out[k]
		}
		fillNaN(out)
		for m := 0; m < k+2; m++ {
			s.cyc[m*np+j] = out[m]
		}
	}
}

// movingAverage writes the length-l running mean of x into out[:len(x)-l+1].
func movingAverage(x []float64, l int, out []float64) {
	var sum float64
	for i := 0; i < l; i++ {
		sum += x[i]
	}
	fl := float64(l)
	out[0] = sum / fl
	for i := l; i < len(x); i++ {
		sum += x[i] - x[i-l]
		out[i-l+1] = sum / fl
	}
}

// loessSmooth fits loess (window length, degree 0/1, tricube × w weights) at
// every point of y into ys, evaluating every jump-th point and interpolating.
// Points whose window has no weight are filled from their neighbours.
func loessSmooth(y, w []float64, length, deg, jump int, ys, work []float64) {
	n := len(y)
	if n == 1 {
		ys[0] = y[0]
		if w[0] <= 0 {
			ys[0] = math.NaN()
		}
		fillNaN(ys[:1])
		return
	}
	est := func(i, nleft, nright int) {
		if v, ok := loessEst(y, w, length, deg, float64(i), nleft, nright, work); ok {
			ys[i] = v
		} else {
			ys[i] = math.NaN()
		}
	}
	nj := min(jump, n-1)
	nsh := (length + 1) / 2 // 1-based half-window, as in stless
	switch {
	case length >= n:
		for i := 0; i < n; i += nj {
			est(i, 0, n-1)
		}
	case nj == 1:
		nleft, nright := 0, length-1
		for i := 0; i < n; i++ {
			if i+1 > nsh && nright != n-1 {
				nleft++
				nright++
			}
			est(i, nleft, nright)
		}
	default:
		for i := 0; i < n; i += nj {
			switch {
			case i+1 < nsh:
				est(i, 0, length-1)
			case i+1 >= n-nsh+1:
				est(i, n-length, n-1)
			default:
				est(i, i-nsh+1, i+length-nsh)
			}
		}
	}
	if nj > 1 {
		for i := 0; i+nj < n; i += nj {
			d := (ys[i+nj] - ys[i]) / float64(nj)
			for j := 1; j < nj; j++ {
				ys[i+j] = ys[i] + float64(j)*d
			}
		}
		last := ((n - 1) / nj) * nj
		if last != n-1 {
			if length >= n {
				est(n-1, 0, n-1)
			} else {
				est(n-1, n-length, n-1)
			}
			d := (ys[n-1] - ys[last]) / float64(n-1-last)
			for j := last + 1; j < n-1; j++ {
				ys[j] = ys[last] + float64(j-last)*d
			}
		}
	}
	fillNaN(ys[:n])
}

// loessEst is one local fit at position xs over y[nleft..nright] (stlest).
func loessEst(y, w []float64, length, deg int, xs float64, nleft, nright int, work []float64) (float64, bool) {
	n := len(y)
	h := math.Max(xs-float64(nleft), float64(nright)-xs)
	if length > n {
		h += float64((length - n) / 2)
	}
	h9, h1 := 0.999*h, 0.001*h
	var a float64
	for j := nleft; j <= nright; j++ {
		work[j] = 0
		r := math.Abs(float64(j) - xs)
		if r <= h9 {
			if r <= h1 {
				work[j] = 1
			} else {
				q := r / h
				q = 1 - q*q*q
				work[j] = q * q * q
			}
			work[j] *= w[j]
			a += work[j]
		}
	}
	if a <= 0 {
		return 0, false
	}
	for j := nleft; j <= nright; j++ {
		work[j] /= a
	}
	if deg > 0 && h > 0 {
		a = 0
		for j := nleft; j <= nright; j++ {
			a += work[j] * float64(j)
		}
		b := xs - a
		var cc float64
		for j := nleft; j <= nright; j++ {
			d := float64(j) - a
			cc += work[j] * d * d
		}
		if math.Sqrt(cc) > 0.001*float64(n-1) {
			b /= cc
			for j := nleft; j <= nright; j++ {
				work[j] *= b*(float64(j)-a) + 1
			}
		}
	}
	var ys float64
	for j := nleft; j <= nright; j++ {
		ys += work[j] * y[j]
	}
	return ys, true
}

// robustnessWeights sets rw to bisquare weights of |r| / (6·median|r|) over
// the non-missing points (mask 1); missing points keep weight 0. The cutoff
// never drops below minCut (see stl).
func robustnessWeights(r, mask, rw []float64, minCut float64) {
	abs := make([]float64, 0, len(r))
	for i, v := range r {
		if mask[i] > 0 {
			abs = append(abs, math.Abs(v))
		}
	}
	cmad := math.Max(6*median(abs), minCut)
	c9, c1 := 0.999*cmad, 0.001*cmad
	for i, v := range r {
		if mask[i] == 0 {
			rw[i] = 0
			continue
		}
		a := math.Abs(v)
		switch {
		case a <= c1:
			rw[i] = 1
		case a <= c9:
			u := a / cmad
			u = 1 - u*u
			rw[i] = u * u
		default:
			rw[i] = 0
		}
	}
}

// median sorts v in place and returns its median (NaN if empty).
func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	sort.Float64s(v)
	m := len(v) / 2
	if len(v)%2 == 1 {
		return v[m]
	}
	return (v[m-1] + v[m]) / 2
}

// fillNaN replaces NaNs by linear interpolation between the nearest finite
// neighbours, holding the end values constant; all-NaN becomes zeros.
func fillNaN(v []float64) {
	prev := -1
	for i := 0; i <= len(v); i++ {
		if i < len(v) && math.IsNaN(v[i]) {
			continue
		}
		if i-prev > 1 {
			for j := prev + 1; j < i; j++ {
				switch {
				case prev < 0 && i == len(v):
					v[j] = 0
				case prev < 0:
					v[j] = v[i]
				case i == len(v):
					v[j] = v[prev]
				default:
					v[j] = v[prev] + (v[i]-v[prev])*float64(j-prev)/float64(i-prev)
				}
			}
		}
		prev = i
	}
}

// DetectSTL decomposes y with STL and returns one band per point: forecast =
// trend + seasonal, band = forecast ± k·σ with σ = 1.4826·MAD of the
// residuals (a robust scale, so the outliers being hunted don't widen it),
// inflated for the smoothers' leverage: in-sample residuals of a point that
// helped fit its own trend and seasonal are smaller than the noise (about
// 0.78× with default windows), which would otherwise narrow the band. With
// Robust set, points the outer loop rejects (|r| ≳ 3σ) keep their full
// residual, so the false-positive rate on clean Gaussian noise at k=3 is
// about 2% rather than 0.3%; use k≈4 there. Without Robust, each outlier
// leaks into the fit of its neighbours (same phase in adjacent cycles, and
// nearby trend), whose residuals can then cross the band as echoes.
// Band semantics match Detect; STL is a batch fit so there is no warm-up,
// and a missing (NaN) actual is never anomalous.
func DetectSTL(p STLParams, y []float64, k float64) ([]Band, error) {
	if k <= 0 {
		return nil, errors.New("anomaly: k must be > 0")
	}
	trend, seasonal, resid, c, err := stl(y, p)
	if err != nil {
		return nil, err
	}
	// Floor σ at 0.1% of the series' own spread: on noise-free input (synthetic
	// or very regular metrics) the residuals are rounding error, and a band that
	// narrow flags most points.
	sigma := math.Max(noiseScale(resid, c), 1e-3*robustScale(y))
	out := make([]Band, len(y))
	for i, v := range y {
		f := trend[i] + seasonal[i]
		b := Band{Forecast: f, Lower: f - k*sigma, Upper: f + k*sigma, Actual: v}
		b.Anomalous = !math.IsNaN(v) && (v < b.Lower || v > b.Upper)
		out[i] = b
	}
	return out, nil
}

// robustScale is 1.4826·MAD of the finite residuals, falling back to their
// standard deviation when more than half are identical, so a near-perfect fit
// doesn't produce a zero-width band.
func robustScale(r []float64) float64 {
	fin := make([]float64, 0, len(r))
	for _, v := range r {
		if !math.IsNaN(v) {
			fin = append(fin, v)
		}
	}
	med := median(fin) // sorts fin; order no longer matters
	dev := make([]float64, len(fin))
	var mean, ss float64
	for i, v := range fin {
		dev[i] = math.Abs(v - med)
		mean += v
	}
	if s := 1.4826 * median(dev); s > 0 {
		return s
	}
	mean /= float64(len(fin))
	for _, v := range fin {
		ss += (v - mean) * (v - mean)
	}
	return math.Sqrt(ss / float64(len(fin)))
}

// noiseScale estimates the noise σ from in-sample residuals. A residual is
// smaller than the noise by the smoothers' leverage on its own point:
// resid_i = (1−h)e_i − Σ_{j≠i} h_j e_j, so sd(resid)/σ ≈ √(1 − 2h + Σh²), with
// h the self-weight of the seasonal (n_s over the cycles) and trend (n_t)
// loess kernels; the low-pass filter's small share is ignored. With default
// windows the factor is about 0.78.
func noiseScale(resid []float64, c stlConfig) float64 {
	n := len(resid)
	hs, ss := kernelSelfWeight(c.ns, c.sdeg, n/c.np)
	ht, st := kernelSelfWeight(c.nt, c.tdeg, n)
	f := 1 - 2*(hs+ht) + ss + st
	return robustScale(resid) / math.Sqrt(math.Max(f, 0.01))
}

// kernelSelfWeight returns the centre weight and Σw² of a unit-weight loess
// kernel of the given length, truncated to avail points.
func kernelSelfWeight(length, deg, avail int) (self, sq float64) {
	m := max(1, min(length, avail))
	y, w, work := make([]float64, m), make([]float64, m), make([]float64, m)
	for i := range w {
		w[i] = 1
	}
	mid := (m - 1) / 2
	if _, ok := loessEst(y, w, length, deg, float64(mid), 0, m-1, work); !ok {
		return 0, 0
	}
	for _, v := range work {
		sq += v * v
	}
	return work[mid], sq
}
