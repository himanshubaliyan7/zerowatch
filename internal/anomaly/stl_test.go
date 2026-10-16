package anomaly

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"
)

// synth returns trend, seasonal and y = trend + seasonal + N(0, noise²).
func synth(n, period int, noise float64, seed int64) (tr, se, y []float64) {
	r := rand.New(rand.NewSource(seed))
	tr, se, y = make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range y {
		x := float64(i)
		tr[i] = 50 + 0.1*x + 5*math.Sin(2*math.Pi*x/float64(8*period)) // slow wander
		ph := 2 * math.Pi * float64(i%period) / float64(period)
		se[i] = 10*math.Sin(ph) + 4*math.Cos(2*ph)
		y[i] = tr[i] + se[i] + r.NormFloat64()*noise
	}
	return
}

func rmse(a, b []float64, from, to int) float64 {
	var s float64
	for i := from; i < to; i++ {
		s += (a[i] - b[i]) * (a[i] - b[i])
	}
	return math.Sqrt(s / float64(to-from))
}

func checkSum(t *testing.T, y, tr, se, re []float64) {
	t.Helper()
	for i := range y {
		if math.IsNaN(y[i]) {
			if !math.IsNaN(re[i]) {
				t.Fatalf("resid[%d] = %v for missing y, want NaN", i, re[i])
			}
			continue
		}
		if d := math.Abs(tr[i] + se[i] + re[i] - y[i]); d > 1e-9*math.Max(1, math.Abs(y[i])) {
			t.Fatalf("i=%d: trend+seasonal+resid-y = %g", i, d)
		}
		if math.IsNaN(tr[i]) || math.IsNaN(se[i]) {
			t.Fatalf("i=%d: NaN component", i)
		}
	}
}

func TestSTLRecoversComponents(t *testing.T) {
	for _, period := range []int{7, 12, 24} {
		n := period * 20
		tr0, se0, y := synth(n, period, 0.5, int64(period))
		tr, se, re, err := STL(y, DefaultSTLParams(period))
		if err != nil {
			t.Fatal(err)
		}
		checkSum(t, y, tr, se, re)
		// skip one period at each end where loess extrapolates
		lo, hi := period, n-period
		if e := rmse(se, se0, lo, hi); e > 0.6 {
			t.Errorf("period %d: seasonal RMSE %.3f", period, e)
		}
		if e := rmse(tr, tr0, lo, hi); e > 0.6 {
			t.Errorf("period %d: trend RMSE %.3f", period, e)
		}
		// residual should look like the injected noise (σ=0.5)
		if s := robustScale(re); s < 0.3 || s > 0.7 {
			t.Errorf("period %d: residual scale %.3f, want ≈0.5", period, s)
		}
	}
}

func TestSTLNoiselessIsExact(t *testing.T) {
	period := 12
	tr0, se0, y := synth(period*10, period, 0, 1)
	tr, se, re, err := STL(y, DefaultSTLParams(period))
	if err != nil {
		t.Fatal(err)
	}
	checkSum(t, y, tr, se, re)
	if e := rmse(se, se0, period, len(y)-period); e > 0.15 {
		t.Fatalf("seasonal RMSE %.3f", e)
	}
	if e := rmse(tr, tr0, period, len(y)-period); e > 0.15 {
		t.Fatalf("trend RMSE %.3f", e)
	}
}

func TestSTLRobustOutliers(t *testing.T) {
	period := 24
	n := period * 16
	_, se0, y := synth(n, period, 0.3, 7)
	r := rand.New(rand.NewSource(8))
	out := map[int]bool{}
	for len(out) < n/20 { // 5% gross outliers
		i := r.Intn(n)
		out[i] = true
		y[i] += 60 + 40*r.Float64()
	}
	_, sePlain, _, err := STL(y, DefaultSTLParams(period))
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultSTLParams(period)
	p.Robust = true
	tr, se, re, err := STL(y, p)
	if err != nil {
		t.Fatal(err)
	}
	checkSum(t, y, tr, se, re)
	lo, hi := period, n-period
	eRobust, ePlain := rmse(se, se0, lo, hi), rmse(sePlain, se0, lo, hi)
	if eRobust > 1.0 {
		t.Fatalf("robust seasonal RMSE %.3f (plain %.3f)", eRobust, ePlain)
	}
	if eRobust >= ePlain {
		t.Fatalf("robust (%.3f) should beat non-robust (%.3f)", eRobust, ePlain)
	}
	// outliers end up in the residual
	for i := range out {
		if re[i] < 40 {
			t.Fatalf("outlier at %d absorbed: resid %.2f", i, re[i])
		}
	}
}

func TestSTLMissing(t *testing.T) {
	period := 12
	n := period * 12
	tr0, se0, y := synth(n, period, 0.2, 3)
	for i := 40; i < 40+period+5; i++ { // a gap longer than a period
		y[i] = math.NaN()
	}
	y[0], y[n-1], y[77] = math.NaN(), math.NaN(), math.NaN()
	for _, robust := range []bool{false, true} {
		p := DefaultSTLParams(period)
		p.Robust = robust
		tr, se, re, err := STL(y, p)
		if err != nil {
			t.Fatal(err)
		}
		checkSum(t, y, tr, se, re)
		if e := rmse(se, se0, period, n-period); e > 0.8 {
			t.Errorf("robust=%v: seasonal RMSE %.3f", robust, e)
		}
		if e := rmse(tr, tr0, period, n-period); e > 0.8 {
			t.Errorf("robust=%v: trend RMSE %.3f", robust, e)
		}
	}
}

func TestSTLValidationAndEdges(t *testing.T) {
	y := make([]float64, 50)
	cases := []struct {
		name string
		y    []float64
		p    STLParams
	}{
		{"period<2", y, STLParams{Period: 1}},
		{"too short", y[:19], STLParams{Period: 10}},
		{"even seasonal", y, STLParams{Period: 5, Seasonal: 8}},
		{"tiny trend", y, STLParams{Period: 5, Trend: 1}},
		{"negative inner", y, STLParams{Period: 5, Inner: -1}},
	}
	for _, c := range cases {
		if _, _, _, err := STL(c.y, c.p); err == nil {
			t.Errorf("%s: want error", c.name)
		}
	}
	if _, _, _, err := STL(y[:19], STLParams{Period: 10}); err != ErrInsufficientData {
		t.Errorf("want ErrInsufficientData, got %v", err)
	}
	inf := append([]float64(nil), y...)
	inf[3] = math.Inf(1)
	if _, _, _, err := STL(inf, STLParams{Period: 5}); err == nil {
		t.Error("Inf: want error")
	}
	mostlyNaN := make([]float64, 40)
	for i := range mostlyNaN {
		mostlyNaN[i] = math.NaN()
	}
	mostlyNaN[0], mostlyNaN[5] = 1, 2
	if _, _, _, err := STL(mostlyNaN, STLParams{Period: 4}); err != ErrInsufficientData {
		t.Errorf("mostly NaN: want ErrInsufficientData, got %v", err)
	}

	// Edge shapes must all decompose cleanly: period 2, exactly two periods,
	// n not a multiple of the period, windows longer than the series, jump 1,
	// constant fits, constant input.
	edge := []struct {
		name string
		n    int
		p    STLParams
	}{
		{"period 2", 30, STLParams{Period: 2}},
		{"two periods", 48, STLParams{Period: 24}},
		{"ragged", 24*5 + 7, STLParams{Period: 24, Robust: true}},
		{"huge windows", 40, STLParams{Period: 10, Seasonal: 51, Trend: 101, LowPass: 101}},
		{"jump 1", 100, STLParams{Period: 10, SeasonalJump: 1, TrendJump: 1, LowPassJump: 1}},
		{"constant fits", 100, STLParams{Period: 10, SeasonalConstant: true, TrendConstant: true, LowPassConstant: true}},
	}
	for _, e := range edge {
		_, _, y := synth(e.n, e.p.Period, 0.3, 11)
		tr, se, re, err := STL(y, e.p)
		if err != nil {
			t.Fatalf("%s: %v", e.name, err)
		}
		if len(tr) != e.n || len(se) != e.n || len(re) != e.n {
			t.Fatalf("%s: wrong lengths", e.name)
		}
		checkSum(t, y, tr, se, re)
	}
	flat := make([]float64, 60)
	for i := range flat {
		flat[i] = 3
	}
	tr, se, re, err := STL(flat, STLParams{Period: 6, Robust: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range flat {
		if math.Abs(tr[i]-3) > 1e-9 || math.Abs(se[i]) > 1e-9 || math.Abs(re[i]) > 1e-9 {
			t.Fatalf("flat: i=%d t=%g s=%g r=%g", i, tr[i], se[i], re[i])
		}
	}
}

func TestSTLLiteWrapper(t *testing.T) {
	y := seasonal(96, 24, 0, 4)
	tr, se, re := STLLite(y, 24)
	want, wantS, _, _ := STL(y, DefaultSTLParams(24))
	for i := range y {
		if tr[i] != want[i] || se[i] != wantS[i] {
			t.Fatal("STLLite must delegate to STL")
		}
		if math.Abs(tr[i]+se[i]+re[i]-y[i]) > 1e-9 {
			t.Fatal("decomposition must sum to input")
		}
	}
	tr, se, _ = STLLite(y[:30], 24) // too short: trend = y
	if tr[5] != y[5] || se[5] != 0 {
		t.Fatal("short input fallback changed")
	}
}

// Noise-free periodic input: residuals are rounding error, so without the
// σ floor nearly every point was flagged (seen on the live API: 88 of 240).
func TestDetectSTLNoiseFree(t *testing.T) {
	y := make([]float64, 240)
	for i := range y {
		y[i] = 50 + 10*math.Sin(float64(i+1)*2*math.Pi/24)
	}
	y[199] += 40
	p := DefaultSTLParams(24)
	p.Robust = true
	bands, err := DetectSTL(p, y, 4)
	if err != nil {
		t.Fatal(err)
	}
	var flagged []int
	for i, b := range bands {
		if b.Anomalous {
			flagged = append(flagged, i)
		}
	}
	if len(flagged) != 1 || flagged[0] != 199 {
		t.Fatalf("flagged %v, want only the spike at 199", flagged)
	}
}

func TestDetectSTL(t *testing.T) {
	period := 24
	n := period * 20
	_, _, y := synth(n, period, 0.5, 21)
	spikes := []int{50, 130, 200, 201, 202, 400} // incl. a 3-point burst
	for _, i := range spikes {
		y[i] += 15
	}
	y[90] = math.NaN()
	// Calibration differs (see DetectSTL): k=3 plain, k=4 robust. Plain STL
	// leaks each spike into its neighbours' fit, so only robust is held to a
	// false-positive budget; plain must at least do worse.
	fps := map[bool]int{}
	for _, c := range []struct {
		robust bool
		k      float64
	}{{false, 3}, {true, 4}} {
		p := DefaultSTLParams(period)
		p.Robust = c.robust
		bands, err := DetectSTL(p, y, c.k)
		if err != nil {
			t.Fatal(err)
		}
		if len(bands) != n {
			t.Fatalf("got %d bands", len(bands))
		}
		anom := 0
		for i, b := range bands {
			if b.Anomalous {
				anom++
			}
			// σ=0.5 noise: the band should be ≈ ±k·0.5 wide
			if w := (b.Upper - b.Lower) / (2 * c.k); math.IsNaN(b.Forecast) || w < 0.35 || w > 0.75 {
				t.Fatalf("robust=%v band %d: %+v", c.robust, i, b)
			}
		}
		for _, i := range spikes {
			if !bands[i].Anomalous {
				t.Errorf("robust=%v: spike at %d not flagged: %+v", c.robust, i, bands[i])
			}
		}
		if bands[90].Anomalous {
			t.Error("missing point flagged")
		}
		fps[c.robust] = anom - len(spikes)
		// JSON: NaN actual -> null, everything else numeric.
		b, err := json.Marshal(bands[89:92])
		if err != nil {
			t.Fatal(err)
		}
		var out []struct{ Forecast, Actual *float64 }
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if out[1].Actual != nil || out[1].Forecast == nil || out[0].Actual == nil {
			t.Fatalf("json: %s", b)
		}
	}
	if fps[true] > n/100 || fps[true] >= fps[false] {
		t.Errorf("false positives: robust %d, plain %d", fps[true], fps[false])
	}
	if _, err := DetectSTL(DefaultSTLParams(period), y, 0); err == nil {
		t.Error("k=0: want error")
	}
	if _, err := DetectSTL(STLParams{Period: 1}, y, 2); err == nil {
		t.Error("bad params: want error")
	}
}

func BenchmarkSTL10k(b *testing.B) {
	for _, bc := range []struct {
		name   string
		period int
		robust bool
	}{
		{"p24", 24, false},
		{"p24-robust", 24, true},
		{"p288", 288, false},
		{"p1440-robust", 1440, true},
	} {
		_, _, y := synth(10000, bc.period, 1, 1)
		p := DefaultSTLParams(bc.period)
		p.Robust = bc.robust
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, _, err := STL(y, p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
