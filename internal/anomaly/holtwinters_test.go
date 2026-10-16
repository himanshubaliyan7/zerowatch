package anomaly

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"
)

func seasonal(n, m int, noise float64, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	y := make([]float64, n)
	for i := range y {
		y[i] = 100 + 0.05*float64(i) + 20*math.Sin(2*math.Pi*float64(i)/float64(m)) + r.NormFloat64()*noise
	}
	return y
}

func TestFitTracksSeasonality(t *testing.T) {
	m := 24
	y := seasonal(24*14, m, 1, 1)
	mod, bands, err := Fit(DefaultParams(m), y)
	if err != nil {
		t.Fatal(err)
	}
	// after warm-up the false-positive rate should be low (< 10%)
	anom := 0
	for _, b := range bands[m*3:] {
		if b.Anomalous {
			anom++
		}
	}
	if rate := float64(anom) / float64(len(bands[m*3:])); rate > 0.10 {
		t.Fatalf("false positive rate too high: %.2f", rate)
	}
	// forecast one season ahead should be close to truth (no noise)
	n := len(y)
	for h := 1; h <= m; h++ {
		truth := 100 + 0.05*float64(n+h-1) + 20*math.Sin(2*math.Pi*float64(n+h-1)/float64(m))
		if d := math.Abs(mod.Forecast(h) - truth); d > 6 {
			t.Fatalf("h=%d forecast %.2f truth %.2f", h, mod.Forecast(h), truth)
		}
	}
}

func TestDetectsSpike(t *testing.T) {
	m := 12
	y := seasonal(12*10, m, 0.5, 2)
	y[100] += 40
	bands, err := Detect(DefaultParams(m), y)
	if err != nil {
		t.Fatal(err)
	}
	if !bands[100].Anomalous {
		t.Fatalf("spike not detected: %+v", bands[100])
	}
	if !math.IsNaN(bands[0].Forecast) {
		t.Fatal("warm-up band should be NaN")
	}
	// band should not explode after the spike
	if bands[110].Upper-bands[110].Lower > 30 {
		t.Fatalf("band exploded: %+v", bands[110])
	}
}

func TestMissingAndErrors(t *testing.T) {
	if _, _, err := Fit(DefaultParams(10), make([]float64, 15)); err != ErrInsufficientData {
		t.Fatalf("want ErrInsufficientData got %v", err)
	}
	if _, _, err := Fit(Params{Alpha: 2, Beta: .1, Gamma: .1, SeasonLength: 4, K: 2}, make([]float64, 20)); err == nil {
		t.Fatal("bad alpha should fail")
	}
	mod, _, _ := Fit(DefaultParams(4), seasonal(40, 4, 0, 3))
	b := mod.Update(math.NaN())
	if b.Anomalous {
		t.Fatal("NaN must not be anomalous")
	}
}

// /api/v1/anomaly encodes Detect output directly; the warm-up NaNs must
// become null instead of failing the whole response.
func TestDetectJSONWarmupIsNull(t *testing.T) {
	m := 6
	bands, err := Detect(DefaultParams(m), seasonal(m*4, m, 0.5, 5))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"bands": bands})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out struct {
		Bands []struct {
			Forecast, Lower, Upper, Actual *float64
		} `json:"bands"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Bands) != len(bands) {
		t.Fatalf("got %d bands, want %d", len(out.Bands), len(bands))
	}
	for i, ob := range out.Bands {
		for k, v := range map[string]*float64{"forecast": ob.Forecast, "lower": ob.Lower, "upper": ob.Upper} {
			if (i < m) != (v == nil) {
				t.Fatalf("band %d %s = %v; want null only in warm-up", i, k, v)
			}
		}
		if ob.Actual == nil || *ob.Actual != bands[i].Actual {
			t.Fatalf("band %d actual = %v, want %v", i, ob.Actual, bands[i].Actual)
		}
	}
	if _, err := json.Marshal(Band{Forecast: math.Inf(1)}); err != nil {
		t.Fatalf("Inf: %v", err)
	}
}

func TestSTLLite(t *testing.T) {
	y := seasonal(96, 24, 0, 4)
	tr, se, re := STLLite(y, 24)
	for i := range y {
		if d := math.Abs(tr[i] + se[i] + re[i] - y[i]); d > 1e-9 {
			t.Fatal("decomposition must sum to input")
		}
	}
}
