// Package anomaly implements CloudWatch-style anomaly detection bands.
//
// The primary model is additive Holt-Winters (triple exponential smoothing):
//
//	level_t  = α(y_t − s_{t−m}) + (1−α)(level_{t−1} + trend_{t−1})
//	trend_t  = β(level_t − level_{t−1}) + (1−β)trend_{t−1}
//	season_t = γ(y_t − level_t) + (1−γ)s_{t−m}
//	ŷ_{t+1}  = level_t + trend_t + s_{t+1−m}
//
// The band is ŷ ± k·σ where σ is the standard deviation of one-step-ahead
// residuals, tracked with an exponentially weighted variance so the band
// adapts to regime changes.
package anomaly

import (
	"encoding/json"
	"errors"
	"math"
)

// ErrInsufficientData is returned when fewer than two full seasons are given.
var ErrInsufficientData = errors.New("anomaly: need at least two full seasons of data")

// Params configure the Holt-Winters model.
type Params struct {
	Alpha, Beta, Gamma float64 // smoothing factors in (0,1)
	SeasonLength       int     // m, number of points per season (e.g. 24 for hourly/daily)
	K                  float64 // band width in standard deviations (CloudWatch default 2)
	VarianceDecay      float64 // EW decay for residual variance in (0,1); 0 => 0.1
}

// DefaultParams returns sensible defaults for a given season length.
func DefaultParams(season int) Params {
	return Params{Alpha: 0.3, Beta: 0.05, Gamma: 0.2, SeasonLength: season, K: 2, VarianceDecay: 0.1}
}

// Validate checks parameter ranges.
func (p Params) Validate() error {
	for _, v := range []float64{p.Alpha, p.Beta, p.Gamma} {
		if v <= 0 || v >= 1 {
			return errors.New("anomaly: alpha/beta/gamma must be in (0,1)")
		}
	}
	if p.SeasonLength < 2 {
		return errors.New("anomaly: season length must be >= 2")
	}
	if p.K <= 0 {
		return errors.New("anomaly: k must be > 0")
	}
	return nil
}

// Band is the expected range for one point.
type Band struct {
	Forecast  float64 `json:"forecast"`
	Lower     float64 `json:"lower"`
	Upper     float64 `json:"upper"`
	Actual    float64 `json:"actual"`
	Anomalous bool    `json:"anomalous"`
}

// MarshalJSON encodes NaN/Inf fields (the warm-up season, overflowed
// forecasts) as null; encoding/json rejects them otherwise.
func (b Band) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Forecast  *float64 `json:"forecast"`
		Lower     *float64 `json:"lower"`
		Upper     *float64 `json:"upper"`
		Actual    *float64 `json:"actual"`
		Anomalous bool     `json:"anomalous"`
	}{finite(b.Forecast), finite(b.Lower), finite(b.Upper), finite(b.Actual), b.Anomalous})
}

func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// Model is an online additive Holt-Winters model.
type Model struct {
	p            Params
	level, trend float64
	season       []float64
	variance     float64
	t            int
	initialized  bool
}

// Fit initialises a model from history (needs >= 2 seasons) and returns the
// in-sample bands for every point after the first season.
func Fit(p Params, y []float64) (*Model, []Band, error) {
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	m := p.SeasonLength
	if len(y) < 2*m {
		return nil, nil, ErrInsufficientData
	}
	if p.VarianceDecay <= 0 || p.VarianceDecay >= 1 {
		p.VarianceDecay = 0.1
	}
	mod := &Model{p: p, season: make([]float64, m)}
	// Classic initialisation: level = mean of season 1; trend = mean slope between
	// seasons 1 and 2; seasonal indices = deviation from season-1 mean.
	var s1, s2 float64
	for i := 0; i < m; i++ {
		s1 += y[i]
		s2 += y[m+i]
	}
	s1 /= float64(m)
	s2 /= float64(m)
	mod.level = s1
	mod.trend = (s2 - s1) / float64(m)
	for i := 0; i < m; i++ {
		mod.season[i] = y[i] - s1
	}
	// seed variance from season-1 dispersion around seasonal profile (=0) -> use
	// simple variance of first-season residuals against level as a weak prior.
	var v float64
	for i := 0; i < m; i++ {
		d := y[i] - s1 - mod.season[i]
		v += d * d
	}
	mod.variance = v / float64(m)
	mod.t = m
	mod.initialized = true
	bands := make([]Band, 0, len(y)-m)
	for _, val := range y[m:] {
		bands = append(bands, mod.Update(val))
	}
	return mod, bands, nil
}

// Forecast returns the h-step-ahead point forecast (h >= 1).
func (m *Model) Forecast(h int) float64 {
	if h < 1 {
		h = 1
	}
	idx := (m.t + h - 1) % m.p.SeasonLength
	return m.level + float64(h)*m.trend + m.season[idx]
}

// Sigma returns the current residual standard deviation.
func (m *Model) Sigma() float64 { return math.Sqrt(m.variance) }

// BandFor returns the band for the next point without updating state.
func (m *Model) BandFor(h int) Band {
	f := m.Forecast(h)
	w := m.p.K * m.Sigma() * math.Sqrt(float64(h))
	return Band{Forecast: f, Lower: f - w, Upper: f + w, Actual: math.NaN()}
}

// Update consumes a new observation, returning the band that was predicted for
// it (computed before the update) and whether the observation fell outside.
// NaN observations advance time using the forecast (missing data imputation).
func (m *Model) Update(y float64) Band {
	b := m.BandFor(1)
	b.Actual = y
	sl := m.p.SeasonLength
	idx := m.t % sl
	if math.IsNaN(y) {
		y = b.Forecast
	} else {
		b.Anomalous = y < b.Lower || y > b.Upper
		r := y - b.Forecast
		d := m.p.VarianceDecay
		// Anomalous points update variance with a capped residual so a single
		// spike doesn't blow the band open (robustness).
		if b.Anomalous {
			capR := 3 * m.Sigma()
			if capR > 0 && math.Abs(r) > capR {
				r = math.Copysign(capR, r)
			}
		}
		m.variance = (1-d)*m.variance + d*r*r
	}
	prevLevel := m.level
	m.level = m.p.Alpha*(y-m.season[idx]) + (1-m.p.Alpha)*(m.level+m.trend)
	m.trend = m.p.Beta*(m.level-prevLevel) + (1-m.p.Beta)*m.trend
	m.season[idx] = m.p.Gamma*(y-m.level) + (1-m.p.Gamma)*m.season[idx]
	m.t++
	return b
}

// Detect is a convenience: fit on y and return bands aligned with y (the first
// season has NaN bands because the model is still warming up).
func Detect(p Params, y []float64) ([]Band, error) {
	_, bands, err := Fit(p, y)
	if err != nil {
		return nil, err
	}
	out := make([]Band, len(y))
	nan := math.NaN()
	for i := 0; i < p.SeasonLength; i++ {
		out[i] = Band{Forecast: nan, Lower: nan, Upper: nan, Actual: y[i]}
	}
	copy(out[p.SeasonLength:], bands)
	return out, nil
}

// STLLite decomposes y with STL using DefaultSTLParams(season).
//
// Deprecated: use STL, which exposes the parameters and reports errors. When
// STL rejects the input (season < 2, fewer than two seasons) STLLite returns
// trend = y with zero seasonal and residual, as the old placeholder did for
// short input.
func STLLite(y []float64, season int) (trend, seasonal, resid []float64) {
	if t, s, r, err := STL(y, DefaultSTLParams(season)); err == nil {
		return t, s, r
	}
	n := len(y)
	trend, seasonal, resid = make([]float64, n), make([]float64, n), make([]float64, n)
	copy(trend, y)
	return
}
