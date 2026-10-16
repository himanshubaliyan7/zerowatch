package forecast

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zerowatch/zerowatch/internal/query"
)

// seasonal returns n points of level + slope·t + amp·sin(2πt/season).
func seasonal(n, season int, level, slope, amp float64) []float64 {
	y := make([]float64, n)
	for t := range y {
		y[t] = level + slope*float64(t) + amp*math.Sin(2*math.Pi*float64(t)/float64(season))
	}
	return y
}

func mape(pred, want []float64) float64 {
	var s float64
	for i := range pred {
		s += math.Abs(pred[i]-want[i]) / math.Abs(want[i])
	}
	return s / float64(len(pred))
}

func TestNormalQuantile(t *testing.T) {
	for p, want := range map[float64]float64{0.5: 0, 0.9: 1.2815516, 0.975: 1.9599640, 0.01: -2.3263479} {
		if got := NormalQuantile(p); math.Abs(got-want) > 1e-6 {
			t.Errorf("NormalQuantile(%v) = %v, want %v", p, got, want)
		}
	}
}

func TestSeasonalMethodsTrackSignal(t *testing.T) {
	const season, h = 24, 48
	all := seasonal(10*season+h, season, 100, 0.2, 20)
	hist, future := all[:10*season], all[10*season:]
	for _, m := range []string{HoltWinters, STL, Auto} {
		p, used, fb, err := Predict(m, hist, Options{Season: season, Horizon: h})
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if fb != "" {
			t.Errorf("%s: unexpected fallback %q", m, fb)
		}
		if m == Auto && used != HoltWinters && used != STL {
			t.Errorf("auto used %s on a seasonal series, want a seasonal method", used)
		}
		if e := mape(p.Forecast, future); e > 0.02 {
			t.Errorf("%s: MAPE %.4f > 2%%", m, e)
		}
		for i := range p.Forecast {
			if !(p.Lower[i] <= p.Forecast[i] && p.Forecast[i] <= p.Upper[i]) {
				t.Fatalf("%s: point %d not inside its interval", m, i)
			}
		}
	}
}

func TestShortHistoryFallsBackToHolt(t *testing.T) {
	y := make([]float64, 30)
	for i := range y {
		y[i] = 10 + 2*float64(i)
	}
	p, used, fb, err := Predict(HoltWinters, y, Options{Season: 1440, Horizon: 5})
	if err != nil {
		t.Fatal(err)
	}
	if used != Holt || !strings.Contains(fb, "two seasons") {
		t.Fatalf("used=%s fallback=%q", used, fb)
	}
	// A clean ramp continues linearly (phi = 1 fits it exactly).
	if math.Abs(p.Forecast[0]-70) > 1 || p.Forecast[4] < p.Forecast[0] {
		t.Errorf("ramp forecast %v", p.Forecast)
	}
}

func TestPrepareFillsGapsAndClamps(t *testing.T) {
	nan := math.NaN()
	y, nonNeg := prepare([]float64{nan, nan, 1, nan, nan, 4, nan})
	want := []float64{1, 2, 3, 4, 4}
	if !nonNeg || len(y) != len(want) {
		t.Fatalf("got %v nonNeg=%v", y, nonNeg)
	}
	for i := range want {
		if y[i] != want[i] {
			t.Fatalf("got %v want %v", y, want)
		}
	}
	// A falling count must not forecast below zero.
	p, _, _, err := Predict(Holt, []float64{50, 40, 30, 20, 10}, Options{Horizon: 10})
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.Forecast {
		if p.Forecast[i] < 0 || p.Lower[i] < 0 {
			t.Fatalf("negative forecast for a count: %v", p.Forecast)
		}
	}
	if _, _, _, err := Predict(Auto, []float64{nan, 1}, Options{Horizon: 1}); !errors.Is(err, ErrInsufficientData) {
		t.Fatalf("want ErrInsufficientData, got %v", err)
	}
}

func ptrs(v []float64) []*float64 {
	out := make([]*float64, len(v))
	for i := range v {
		out[i] = &v[i]
	}
	return out
}

func TestServiceValuesAndCrossing(t *testing.T) {
	ramp := make([]float64, 60)
	for i := range ramp {
		ramp[i] = float64(i) // +1 per minute
	}
	thr := 70.0
	s := &Service{}
	end := int64(1_800_000_000_000)
	r, err := s.Forecast(context.Background(), 1, Request{Values: ptrs(ramp), End: end, Period: 60, Horizon: 30, Threshold: &thr, Comparison: "GreaterThanThreshold"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != Holt || len(r.History) != 60 || len(r.Points) != 30 {
		t.Fatalf("method=%s history=%d points=%d", r.Method, len(r.History), len(r.Points))
	}
	if r.History[59].T != end-60_000 || r.Points[0].T != end {
		t.Fatalf("timestamps: last history %d first point %d", r.History[59].T, r.Points[0].T)
	}
	if r.CrossesAt == nil {
		t.Fatal("expected a crossing")
	}
	// 59 + k > 70 first at k = 12.
	if k := (*r.CrossesAt-end)/60_000 + 1; k < 11 || k > 16 {
		t.Errorf("crosses after %d periods", k)
	}
	if _, err := s.Forecast(context.Background(), 1, Request{Period: 60, Horizon: 1}); !errors.Is(err, query.ErrBadRequest) {
		t.Errorf("missing input: %v", err)
	}
	if _, err := s.Forecast(context.Background(), 1, Request{Values: ptrs(ramp), Period: 60, Horizon: 1, Threshold: &thr, Comparison: "Bigger"}); !errors.Is(err, query.ErrBadRequest) {
		t.Errorf("bad comparison: %v", err)
	}
}

func TestServiceModelAndFallback(t *testing.T) {
	var got modelRequest
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/forecast" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		f := make([]float64, got.Horizon)
		for i := range f {
			f[i] = 42
		}
		_ = json.NewEncoder(w).Encode(modelResponse{Model: "fake", Forecast: f, Lower: f, Upper: f})
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer bad.Close()

	vals := ptrs(seasonal(100, 10, 50, 0, 5))
	vals[3] = nil
	r, err := (&Service{ModelURL: ok.URL}).Forecast(context.Background(), 1, Request{Values: vals, Period: 60, Horizon: 4})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "model:fake" || r.Points[3].Forecast != 42 || got.Horizon != 4 || got.Values[3] != nil {
		t.Fatalf("model path: %+v req=%+v", r, got)
	}
	r, err = (&Service{ModelURL: bad.URL}).Forecast(context.Background(), 1, Request{Values: vals, Period: 60, Horizon: 4, Season: 10})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(r.Method, "model") || !strings.Contains(r.Fallback, "HTTP 500") {
		t.Fatalf("fallback: method=%s fallback=%q", r.Method, r.Fallback)
	}
	r, err = (&Service{}).Forecast(context.Background(), 1, Request{Values: vals, Period: 60, Horizon: 4, Method: Model})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Fallback, "no model service") {
		t.Fatalf("model without url: fallback=%q", r.Fallback)
	}
	f, used, err := (&Service{}).Predict(context.Background(), vals, 60, 3)
	// 100 points of a 10-point cycle, daily season: a detected short season wins.
	if err != nil || len(f) != 3 || used == Holt || math.Abs(f[1]-(50+5*math.Sin(2*math.Pi*101/10))) > 0.5 {
		t.Fatalf("Predict: %v %s %v", f, used, err)
	}
}

func TestIntervalScale(t *testing.T) {
	mk := func(errs []float64) []*holdout {
		h := &holdout{p: newPrediction(len(errs)), test: make([]float64, len(errs))}
		for i, e := range errs {
			h.p.Forecast[i], h.p.Lower[i], h.p.Upper[i] = 10, 9, 11 // half-width 1
			h.test[i] = 10 + e
		}
		return []*holdout{h}
	}
	scale := func(hs []*holdout) float64 { return intervalScales(hs, 0.8, 1)[0] }
	// errors at 2 half-widths everywhere: the band must double.
	if s := scale(mk([]float64{2, -2, 2, -2, 2, -2, 2, -2, 2, -2})); s != 2 {
		t.Errorf("scale = %v, want 2", s)
	}
	// tiny errors: narrowed, but not below the floor.
	if s := scale(mk([]float64{0.01, 0.01, 0.01, 0.01, 0.01, 0.01, 0.01, 0.01})); s != minScale {
		t.Errorf("scale = %v, want %v", s, minScale)
	}
	if s := scale(mk([]float64{5, 5, 5})); s != 1 {
		t.Errorf("too few points should not calibrate, got %v", s)
	}
	if scale(nil) != 1 {
		t.Error("no holdout should not calibrate")
	}
	// Errors that grow with the horizon get growing scales: 48 steps whose
	// error is step/12 half-widths: two buckets, scales about 1.7 and 3.7.
	errs := make([]float64, 48)
	for i := range errs {
		errs[i] = float64(i+1) / 12
	}
	s := intervalScales(mk(errs), 0.8, 60)
	for i := 1; i < len(s); i++ {
		if s[i] < s[i-1] {
			t.Fatalf("scales not non-decreasing at %d: %v", i, s)
		}
	}
	if s[0] > 2 || s[47] < 3 || s[59] != s[47] {
		t.Errorf("horizon scales %v", s)
	}
}

// A young node: 1 h of a 10-minute cycle at 1-minute buckets, with the
// daily season configured. auto must find the short season and beat damped
// Holt (which used to be the only choice here).
func TestShortHistorySeason(t *testing.T) {
	const n, h, cycle = 60, 15, 10
	rng := rand.New(rand.NewSource(1))
	all := seasonal(n+h, cycle, 500, 0.5, 120)
	for i := range all {
		all[i] += 5 * rng.NormFloat64()
	}
	hist, future := all[:n], all[n:]
	if s := shortSeasons(hist, 1440); len(s) == 0 || s[0] != cycle {
		t.Fatalf("shortSeasons = %v, want %d first", s, cycle)
	}
	pa, used, fb, err := Predict(Auto, hist, Options{Season: 1440, Horizon: h})
	if err != nil {
		t.Fatal(err)
	}
	ph, _, _, _ := Predict(Holt, hist, Options{Season: 1440, Horizon: h})
	if used == Holt || !strings.Contains(fb, "season 10") {
		t.Fatalf("auto used %s (fallback %q), want a season-10 method", used, fb)
	}
	ea, eh := mape(pa.Forecast, future), mape(ph.Forecast, future)
	if ea > 0.05 || ea > eh/2 {
		t.Errorf("auto MAPE %.3f vs holt %.3f", ea, eh)
	}
	// An explicit seasonal method uses the detected season too.
	if _, used, fb, _ := Predict(HoltWinters, hist, Options{Season: 1440, Horizon: h}); used != HoltWinters || !strings.Contains(fb, "season 10") {
		t.Errorf("holtwinters on short history: used %s fallback %q", used, fb)
	}
	// One season of the configured season fits: seasonal-naive is a candidate.
	sine := seasonal(90+h, 60, 1000, 0, 300)
	p, used, _, err := Predict(Auto, sine[:90], Options{Season: 60, Horizon: h})
	if err != nil {
		t.Fatal(err)
	}
	if used != SeasonalNaive || mape(p.Forecast, sine[90:]) > 0.01 {
		t.Errorf("one season of a clean sine: used %s, MAPE %.4f", used, mape(p.Forecast, sine[90:]))
	}
}

// Heavy-tailed noise: the calibrated 80 % band should cover roughly 80 % of
// what actually happens next (raw Gaussian bands under-cover it).
// The same for every built-in method, on that series and on a random walk
// (where errors grow with the horizon faster than Holt-Winters and STL
// assume).
func TestCalibratedCoverage(t *testing.T) {
	const season, n, h = 24, 24 * 12, 48
	for _, walk := range []bool{false, true} {
		for _, m := range []string{HoltWinters, STL, Holt, Auto} {
			var covered, total int
			for seed := 0; seed < 20; seed++ {
				rng := rand.New(rand.NewSource(int64(seed)))
				y := seasonal(n+h, season, 100, 0, 20)
				for i := range y {
					y[i] += 6 * rng.NormFloat64() / math.Max(0.25, math.Abs(rng.NormFloat64())) * 0.3 // Cauchy-ish tails
				}
				if walk {
					y[0] = 1000
					for i := 1; i < len(y); i++ {
						y[i] = y[i-1] + 5*rng.NormFloat64()
					}
				}
				p, _, _, err := Predict(m, y[:n], Options{Season: season, Horizon: h})
				if err != nil {
					t.Fatal(err)
				}
				for i, a := range y[n:] {
					total++
					if a >= p.Lower[i] && a <= p.Upper[i] {
						covered++
					}
				}
			}
			if c := float64(covered) / float64(total); c < 0.7 || c > 0.92 {
				t.Errorf("%s (random walk %v): coverage %.2f, want about 0.80", m, walk, c)
			} else {
				t.Logf("%s (random walk %v): coverage %.2f", m, walk, c)
			}
		}
	}
}
