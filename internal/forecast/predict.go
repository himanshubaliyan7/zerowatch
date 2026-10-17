// Package forecast predicts future values of a metric. Built-in methods:
// additive Holt-Winters (anomaly.Model, small parameter search), STL with a
// linear trend extrapolation, damped Holt (no season), and seasonal-naive;
// on history shorter than two seasons auto also tries shorter seasons found
// by autocorrelation.
// An optional external model service (--forecaster-url, see forecaster/) is
// used by Service when configured, with fallback to the built-in methods.
package forecast

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/himanshubaliyan7/zerowatch/internal/anomaly"
)

// Method names.
const (
	Auto        = "auto"
	HoltWinters = "holtwinters"
	STL         = "stl"
	Holt        = "holt"
	Model       = "model"
)

// ErrInsufficientData is returned when there are fewer than MinPoints valid values.
var ErrInsufficientData = errors.New("forecast: need at least 3 values")

// MinPoints is the least history any method accepts.
const MinPoints = 3

// Options configure a built-in prediction.
type Options struct {
	Season   int     // points per season; < 2 disables the seasonal methods
	Horizon  int     // future periods, >= 1
	Interval float64 // central coverage of [Lower, Upper] in (0,1); 0 => 0.8
}

// Prediction holds Horizon future values.
type Prediction struct {
	Forecast, Lower, Upper []float64
}

// SeasonalNaive repeats the last season. auto considers it on history too
// short for two seasons; it is not a user-selectable method.
const SeasonalNaive = "snaive"

// Predict runs a built-in method on y (one value per period, NaN = missing).
// "auto" (or "") picks by holdout error among damped Holt and, with two
// seasons of history, Holt-Winters and STL. On shorter history it also tries
// seasonal-naive with the configured season and Holt-Winters, STL and
// seasonal-naive with shorter seasons found by autocorrelation; fallback then
// names the season used. A seasonal method asked for without two seasons
// uses a detected shorter season, else damped Holt; fallback says why. used
// is the method that ran.
func Predict(method string, y []float64, o Options) (p Prediction, used, fallback string, err error) {
	if o.Horizon < 1 {
		return p, "", "", fmt.Errorf("forecast: horizon must be >= 1")
	}
	if o.Interval == 0 {
		o.Interval = 0.8
	}
	if o.Interval <= 0 || o.Interval >= 1 {
		return p, "", "", fmt.Errorf("forecast: interval must be in (0,1)")
	}
	y, nonNeg := prepare(y)
	if len(y) < MinPoints {
		return p, "", "", ErrInsufficientData
	}
	z := NormalQuantile((1 + o.Interval) / 2)
	c := cand{method, o.Season}
	var ho *holdout
	switch method {
	case "", Auto:
		c, ho = choose(y, o.Season, o.Horizon, z)
		if c.method != Holt && c.season != o.Season {
			fallback = fmt.Sprintf("history shorter than two seasons (%d points), have %d; used season %d from autocorrelation", 2*o.Season, len(y), c.season)
		}
	case HoltWinters, STL:
		if o.Season < 2 || len(y) < 2*o.Season {
			if s := shortSeasons(y, o.Season); len(s) > 0 {
				c.season = s[0]
				fallback = fmt.Sprintf("%s needs two seasons (%d points), have %d; used season %d from autocorrelation", method, 2*o.Season, len(y), c.season)
			} else {
				c.method = Holt
				fallback = fmt.Sprintf("%s needs two seasons (%d points), have %d; used holt", method, 2*o.Season, len(y))
			}
		}
	case Holt:
	default:
		return p, "", "", fmt.Errorf("forecast: unknown method %q (auto|holtwinters|stl|holt|model)", method)
	}
	p, err = c.run(y, o.Horizon, z)
	if err != nil {
		return p, "", "", err
	}
	for i, s := range intervalScales(calibrationHoldouts(c, y, o.Horizon, z, ho), o.Interval, o.Horizon) {
		if s != 1 {
			f := p.Forecast[i]
			p.Lower[i] = f - s*(f-p.Lower[i])
			p.Upper[i] = f + s*(p.Upper[i]-f)
		}
	}
	if nonNeg {
		for i := range p.Forecast {
			p.Forecast[i] = math.Max(p.Forecast[i], 0)
			p.Lower[i] = math.Max(p.Lower[i], 0)
			p.Upper[i] = math.Max(p.Upper[i], 0)
		}
	}
	return p, c.method, fallback, nil
}

// cand is a method with the season it runs with.
type cand struct {
	method string
	season int
}

func (c cand) run(y []float64, horizon int, z float64) (Prediction, error) {
	switch c.method {
	case HoltWinters:
		return holtWinters(y, c.season, horizon, z)
	case STL:
		return stlForecast(y, c.season, horizon, z)
	case SeasonalNaive:
		return seasonalNaive(y, c.season, horizon, z), nil
	}
	return dampedHolt(y, horizon, z), nil
}

// fits reports whether c can run on n points.
func (c cand) fits(n int) bool {
	switch c.method {
	case HoltWinters, STL:
		return n >= 2*c.season
	case SeasonalNaive:
		return n >= c.season+2
	}
	return n >= MinPoints
}

// holdout is a method's forecast of the last points of the history, made
// from the points before them, and those points.
type holdout struct {
	p    Prediction
	test []float64
}

func holdoutLen(n, horizon int) int { return max(1, min(horizon, n/4)) }

// choose is the "auto" method: it holds out the last min(horizon, len/4)
// points, forecasts them with every eligible candidate and returns the one
// with the lowest mean absolute error, with its holdout. One candidate skips
// the holdout. With two seasons before the holdout the candidates are
// damped Holt, Holt-Winters and STL; with fewer (a young node) they are
// damped Holt, seasonal-naive with the configured season when one season
// fits, and Holt-Winters, STL and seasonal-naive with each shorter season
// found by autocorrelation.
func choose(y []float64, season, horizon int, z float64) (cand, *holdout) {
	v := holdoutLen(len(y), horizon)
	nt := len(y) - v
	cands := []cand{{Holt, season}}
	switch {
	case season < 2:
	case nt >= 2*season:
		cands = append(cands, cand{HoltWinters, season}, cand{STL, season})
	case len(y) >= 2*season:
		return cand{HoltWinters, season}, nil // seasonal, but too short to validate: keep the season
	default:
		if c := (cand{SeasonalNaive, season}); c.fits(nt) {
			cands = append(cands, c)
		}
		if nt >= MinPoints {
			for _, s := range shortSeasons(y[:nt], season) {
				cands = append(cands, cand{HoltWinters, s}, cand{STL, s}, cand{SeasonalNaive, s})
			}
		}
	}
	if len(cands) == 1 || nt < MinPoints {
		return cands[0], nil
	}
	train, test := y[:nt], y[nt:]
	best, bestErr := cands[0], math.Inf(1)
	var bestHO *holdout
	for _, c := range cands {
		if !c.fits(nt) {
			continue
		}
		p, err := c.run(train, v, z)
		if err != nil {
			continue
		}
		var e float64
		for i, a := range test {
			e += math.Abs(p.Forecast[i] - a)
		}
		if e < bestErr {
			best, bestErr, bestHO = c, e, &holdout{p, test}
		}
	}
	return best, bestHO
}

// Short-season detection: lags in [minShortSeason, min(n/2, season-1)]
// whose autocorrelation (after removing a linear trend) is a local maximum
// of at least minSeasonACF; the strongest maxShortSeasons are returned.
const (
	minShortSeason  = 4
	minSeasonACF    = 0.3
	maxShortSeasons = 2
)

// shortSeasons returns candidate seasons shorter than season for y, best
// first; each fits at least twice in y.
func shortSeasons(y []float64, season int) []int {
	n := len(y)
	maxLag := n / 2
	if season >= 2 {
		maxLag = min(maxLag, season-1)
	}
	if maxLag < minShortSeason+1 {
		return nil
	}
	slope := lsSlope(y)
	var mean float64
	for _, v := range y {
		mean += v
	}
	mean /= float64(n)
	x := make([]float64, n)
	var ss float64
	for i, v := range y {
		x[i] = v - mean - slope*(float64(i)-float64(n-1)/2)
		ss += x[i] * x[i]
	}
	if ss == 0 {
		return nil
	}
	ac := make([]float64, maxLag+2)
	for l := minShortSeason - 1; l <= maxLag+1 && l < n; l++ {
		var s float64
		for i := l; i < n; i++ {
			s += x[i] * x[i-l]
		}
		ac[l] = s / ss
	}
	type peak struct {
		lag int
		ac  float64
	}
	var peaks []peak
	for l := minShortSeason; l <= maxLag; l++ {
		if ac[l] >= minSeasonACF && ac[l] > ac[l-1] && ac[l] >= ac[l+1] {
			peaks = append(peaks, peak{l, ac[l]})
		}
	}
	sort.Slice(peaks, func(i, j int) bool { return peaks[i].ac > peaks[j].ac })
	out := make([]int, 0, maxShortSeasons)
	for _, p := range peaks[:min(len(peaks), maxShortSeasons)] {
		out = append(out, p.lag)
	}
	return out
}

// Calibration uses at least calOrigins non-overlapping holdouts (more for
// short horizons, up to calMaxOrigins, to reach calTarget points), needs
// minCalibration points, and fits one scale per horizon bucket of at least
// bucketPoints points (at most maxBuckets). Scales stay within bounds.
const (
	calOrigins, calMaxOrigins = 3, 8
	calTarget                 = 96
	minCalibration            = 8
	bucketPoints, maxBuckets  = 24, 4
	minScale, maxScale        = 0.5, 10.0
)

// calibrationHoldouts forecasts the last v = min(horizon, len/4) points with
// c from the points before them, then the v before those, and so on; first,
// when given, is the most recent one (auto's selection holdout).
func calibrationHoldouts(c cand, y []float64, horizon int, z float64, first *holdout) []*holdout {
	n := len(y)
	v := holdoutLen(n, horizon)
	k := min(calMaxOrigins, max(calOrigins, (calTarget+v-1)/v))
	var out []*holdout
	for i := 0; i < k; i++ {
		if i == 0 && first != nil {
			out = append(out, first)
			continue
		}
		cut := n - (i+1)*v
		if cut < 0 || !c.fits(cut) {
			break
		}
		p, err := c.run(y[:cut], v, z)
		if err != nil {
			break
		}
		out = append(out, &holdout{p, y[cut : cut+v]})
	}
	return out
}

// intervalScales returns, per forecast step, the factor that makes the
// holdouts' intervals cover `interval` of their actual values: the
// conformal interval-quantile of |actual - forecast| / half-width. The
// model-based widths assume well-behaved residuals; this corrects them to
// the series' real behaviour (the benchmark showed STL at 62 % and damped
// Holt at 91 % for an 80 % band). Errors usually grow faster with the
// horizon than the models assume, so the holdout steps are split into up
// to maxBuckets buckets, each with its own scale, made non-decreasing with
// the horizon and interpolated between bucket centres; steps past the
// holdout use the last bucket. All 1 when there is too little holdout.
func intervalScales(hs []*holdout, interval float64, horizon int) []float64 {
	out := make([]float64, horizon)
	for i := range out {
		out[i] = 1
	}
	if len(hs) == 0 {
		return out
	}
	v := len(hs[0].test)
	steps := make([][]float64, v)
	total := 0
	for _, h := range hs {
		for j, a := range h.test {
			if j >= v {
				break
			}
			if w := (h.p.Upper[j] - h.p.Lower[j]) / 2; w > 0 {
				steps[j] = append(steps[j], math.Abs(a-h.p.Forecast[j])/w)
				total++
			}
		}
	}
	if total < minCalibration {
		return out
	}
	nb := max(1, min(maxBuckets, v, total/bucketPoints))
	scales := make([]float64, nb)
	centres := make([]float64, nb)
	for b := range scales {
		lo, hi := b*v/nb, (b+1)*v/nb
		var r []float64
		for _, s := range steps[lo:hi] {
			r = append(r, s...)
		}
		if len(r) < minCalibration {
			return out
		}
		scales[b] = conformalQuantile(r, interval)
		centres[b] = float64(lo+hi-1) / 2
	}
	isotonic(scales)
	for i := range out {
		x := float64(i)
		s := scales[nb-1]
		switch {
		case x <= centres[0]:
			s = scales[0]
		case x < centres[nb-1]:
			b := 0
			for centres[b+1] < x {
				b++
			}
			f := (x - centres[b]) / (centres[b+1] - centres[b])
			s = scales[b] + f*(scales[b+1]-scales[b])
		}
		out[i] = max(minScale, min(maxScale, s))
	}
	return out
}

// isotonic replaces v with its least-squares non-decreasing fit (pool
// adjacent violators). A running maximum would also be monotone, but it
// keeps every noisy high bucket and over-covers.
func isotonic(v []float64) {
	type block struct {
		sum float64
		n   int
	}
	var bs []block
	for _, x := range v {
		bs = append(bs, block{x, 1})
		for len(bs) > 1 {
			a, b := bs[len(bs)-2], bs[len(bs)-1]
			if a.sum/float64(a.n) <= b.sum/float64(b.n) {
				break
			}
			bs = append(bs[:len(bs)-2], block{a.sum + b.sum, a.n + b.n})
		}
	}
	i := 0
	for _, b := range bs {
		for j := 0; j < b.n; j++ {
			v[i] = b.sum / float64(b.n)
			i++
		}
	}
}

// conformalQuantile is the ceil((n+1)·q)-th smallest of r (the largest when
// that is past the end): split-conformal, so a small holdout does not
// under-cover the way the plain empirical quantile does.
func conformalQuantile(r []float64, q float64) float64 {
	s := append([]float64(nil), r...)
	sort.Float64s(s)
	return s[min(len(s)-1, max(0, int(math.Ceil(q*float64(len(s)+1)))-1))]
}

// seasonalNaive repeats the last season; the interval comes from the spread
// of the season-on-season differences and widens once per season ahead.
func seasonalNaive(y []float64, season, horizon int, z float64) Prediction {
	n := len(y)
	var ss float64
	for i := season; i < n; i++ {
		d := y[i] - y[i-season]
		ss += d * d
	}
	sigma := math.Sqrt(ss / float64(max(1, n-season)))
	out := newPrediction(horizon)
	for h := 1; h <= horizon; h++ {
		f := y[n-season+(h-1)%season]
		w := z * sigma * math.Sqrt(float64((h-1)/season+1))
		out.Forecast[h-1], out.Lower[h-1], out.Upper[h-1] = f, f-w, f+w
	}
	return out
}

// prepare drops leading missing values, fills interior gaps linearly and
// carries the last value over trailing gaps. nonNeg reports whether every
// observed value is >= 0 (counts, latencies), so forecasts can be clamped.
func prepare(in []float64) (y []float64, nonNeg bool) {
	first := -1
	for i, v := range in {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, false
	}
	y = append([]float64(nil), in[first:]...)
	nonNeg = true
	last := 0 // index of the last valid value
	for i := 1; i < len(y); i++ {
		if math.IsNaN(y[i]) || math.IsInf(y[i], 0) {
			continue
		}
		if gap := i - last; gap > 1 {
			for j := last + 1; j < i; j++ {
				y[j] = y[last] + (y[i]-y[last])*float64(j-last)/float64(gap)
			}
		}
		last = i
	}
	for j := last + 1; j < len(y); j++ {
		y[j] = y[last]
	}
	for _, v := range y {
		if v < 0 {
			nonNeg = false
			break
		}
	}
	return y, nonNeg
}

// holtWinters fits anomaly.Model over a small grid of smoothing factors,
// keeps the one with the lowest one-step error and extrapolates it. The
// interval uses the additive Holt-Winters h-step variance, σ²·(1 + Σ_{j<h}
// (α(1+jβ) + γ·[m divides j])²), not the model's σ·√h, which grows like a
// random walk and made the long-horizon bands several times too wide.
//
// The model is fitted to y minus the slope between its first two seasons,
// which is added back to the forecast: anomaly.Fit seeds the seasonal
// indices from the raw first season, so on a trending series they start as
// a sawtooth that the small γ grid takes many seasons to wash out, and a
// forecast made at a season boundary was off by most of a season's growth.
func holtWinters(y []float64, season, horizon int, z float64) (Prediction, error) {
	if len(y) < 2*season {
		return Prediction{}, anomaly.ErrInsufficientData
	}
	var s1, s2 float64
	for i := 0; i < season; i++ {
		s1 += y[i]
		s2 += y[season+i]
	}
	slope := (s2 - s1) / float64(season*season)
	yd := make([]float64, len(y))
	for i, v := range y {
		yd[i] = v - slope*float64(i)
	}
	var best *anomaly.Model
	var bestP anomaly.Params
	bestSSE := math.Inf(1)
	for _, a := range []float64{0.1, 0.3, 0.5} {
		for _, b := range []float64{0.01, 0.05} {
			for _, g := range []float64{0.1, 0.3} {
				p := anomaly.Params{Alpha: a, Beta: b, Gamma: g, SeasonLength: season, K: z, VarianceDecay: 0.05}
				m, bands, err := anomaly.Fit(p, yd)
				if err != nil {
					return Prediction{}, err
				}
				var sse float64
				for _, bd := range bands {
					d := bd.Actual - bd.Forecast
					sse += d * d
				}
				if sse < bestSSE {
					best, bestP, bestSSE = m, p, sse
				}
			}
		}
	}
	out := newPrediction(horizon)
	sigma := math.Sqrt(bestSSE / float64(len(y)-season)) // all one-step errors: the model's EWMA sigma swings with the last few residuals
	var varSum float64
	for h := 1; h <= horizon; h++ {
		if j := h - 1; j > 0 {
			c := bestP.Alpha * (1 + float64(j)*bestP.Beta)
			if j%season == 0 {
				c += bestP.Gamma
			}
			varSum += c * c
		}
		f := best.Forecast(h) + slope*float64(len(y)-1+h)
		w := z * sigma * math.Sqrt(1+varSum)
		out.Forecast[h-1], out.Lower[h-1], out.Upper[h-1] = f, f-w, f+w
	}
	return out, nil
}

// stlForecast decomposes y with robust STL, extends the trend along the
// least-squares slope of its last season, repeats the last seasonal cycle and
// sizes the interval from the residuals' robust scale.
func stlForecast(y []float64, season, horizon int, z float64) (Prediction, error) {
	trend, seas, resid, err := anomaly.STL(y, anomaly.STLParams{Period: season, Robust: true})
	if err != nil {
		return Prediction{}, err
	}
	n := len(y)
	slope := lsSlope(trend[n-min(season, n):])
	// RMS, not the robust MAD: robust STL pushes large errors into the
	// residual tails, and the MAD then gave 29 % coverage for an 80 % band.
	var ss float64
	for _, r := range resid {
		ss += r * r
	}
	sigma := math.Sqrt(ss / float64(n))
	out := newPrediction(horizon)
	for h := 1; h <= horizon; h++ {
		f := trend[n-1] + slope*float64(h) + seas[n-season+(h-1)%season]
		w := z * sigma * math.Sqrt(1+float64(h)/float64(season))
		out.Forecast[h-1], out.Lower[h-1], out.Upper[h-1] = f, f-w, f+w
	}
	return out, nil
}

// dampedHolt is Holt's linear method with a damped trend; alpha, beta and
// the damping phi are picked by one-step error over a small grid (phi = 1 is
// an undamped trend, which wins on steady growth and matters for long
// horizons: phi = 0.98 caps the trend's total contribution at 49 steps).
func dampedHolt(y []float64, horizon int, z float64) Prediction {
	type fit struct{ l, b, a, beta, phi, sse float64 }
	run := func(a, beta, phi float64) fit {
		l, b := y[0], y[1]-y[0]
		var sse float64
		for _, v := range y[1:] {
			f := l + phi*b
			sse += (v - f) * (v - f)
			nl := a*v + (1-a)*(l+phi*b)
			b = beta*(nl-l) + (1-beta)*phi*b
			l = nl
		}
		return fit{l, b, a, beta, phi, sse}
	}
	best := fit{sse: math.Inf(1)}
	for _, phi := range []float64{0.9, 0.98, 1} {
		for _, a := range []float64{0.1, 0.2, 0.3, 0.5, 0.7, 0.9} {
			for _, beta := range []float64{0.01, 0.05, 0.1, 0.2} {
				// ties keep the more damped (earlier) fit
				if f := run(a, beta, phi); f.sse < best.sse*(1-1e-9) {
					best = f
				}
			}
		}
	}
	sigma := math.Sqrt(best.sse / float64(len(y)-1))
	out := newPrediction(horizon)
	var damp, varSum float64
	for h := 1; h <= horizon; h++ {
		damp += math.Pow(best.phi, float64(h))
		f := best.l + damp*best.b
		if h > 1 {
			c := best.a * (1 + float64(h-1)*best.beta)
			varSum += c * c
		}
		w := z * sigma * math.Sqrt(1+varSum)
		out.Forecast[h-1], out.Lower[h-1], out.Upper[h-1] = f, f-w, f+w
	}
	return out
}

func newPrediction(h int) Prediction {
	return Prediction{Forecast: make([]float64, h), Lower: make([]float64, h), Upper: make([]float64, h)}
}

func lsSlope(v []float64) float64 {
	n := float64(len(v))
	if n < 2 {
		return 0
	}
	mx := (n - 1) / 2
	var my float64
	for _, x := range v {
		my += x
	}
	my /= n
	var num, den float64
	for i, x := range v {
		dx := float64(i) - mx
		num += dx * (x - my)
		den += dx * dx
	}
	return num / den
}

// NormalQuantile is the inverse standard normal CDF (Acklam's rational
// approximation, relative error < 1.2e-9) for p in (0,1).
func NormalQuantile(p float64) float64 {
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const lo = 0.02425
	switch {
	case p <= 0:
		return math.Inf(-1)
	case p >= 1:
		return math.Inf(1)
	case p < lo:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > 1-lo:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}

// Breaches applies a CloudWatch comparison operator.
func Breaches(comparison string, v, threshold float64) (bool, error) {
	switch comparison {
	case "GreaterThanThreshold":
		return v > threshold, nil
	case "GreaterThanOrEqualToThreshold":
		return v >= threshold, nil
	case "LessThanThreshold":
		return v < threshold, nil
	case "LessThanOrEqualToThreshold":
		return v <= threshold, nil
	}
	return false, fmt.Errorf("forecast: unknown comparison %q", comparison)
}
