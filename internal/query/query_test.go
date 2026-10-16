package query

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/zerowatch/zerowatch/internal/model"
)

func TestParsePrecedence(t *testing.T) {
	cases := map[string]string{
		"m1 + m2 * 3":         "(m1 + (m2 * 3))",
		"(m1 + m2) / 2":       "((m1 + m2) / 2)",
		"2 ^ 3 ^ 2":           "(2 ^ (3 ^ 2))",
		"-m1 ^ 2":             "(-(m1 ^ 2))",
		"m1 - m2 - m3":        "((m1 - m2) - m3)",
		"rate(e1)":            "RATE(e1)",
		"FILL(m1, REPEAT)":    "FILL(m1, REPEAT)",
		"m1 / PERIOD(m1)":     "(m1 / PERIOD(m1))",
		"1.5e3 * m1":          "(1500 * m1)",
		"SUM(m1, m2, m3)*100": "(SUM(m1, m2, m3) * 100)",
	}
	for src, want := range cases {
		n, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if n.String() != want {
			t.Errorf("%s: got %s want %s", src, n, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{"m1 +", "(m1", "m1 m2", "FILL(m1,", "m1 $ 2", ""} {
		if _, err := Parse(src); err == nil {
			t.Errorf("%q: expected error", src)
		}
	}
}

func TestRefsExcludeFillKeywords(t *testing.T) {
	n, _ := Parse("FILL(m1, REPEAT) + FILL(m2, m3)")
	got := strings.Join(Refs(n), ",")
	if got != "m1,m2,m3" {
		t.Fatalf("refs %s", got)
	}
}

func TestBuildPlanTopoAndCycles(t *testing.T) {
	ms := &MetricStat{Namespace: "n", MetricName: "x", Stat: "Average"}
	p, err := BuildPlan([]MetricDataQuery{
		{ID: "e2", Expression: "RATE(e1)"},
		{ID: "e1", Expression: "(m1 + m2) / 2"},
		{ID: "m1", MetricStat: ms},
		{ID: "m2", MetricStat: ms},
	})
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range p.Order {
		pos[id] = i
	}
	if !(pos["m1"] < pos["e1"] && pos["m2"] < pos["e1"] && pos["e1"] < pos["e2"]) {
		t.Fatalf("bad order %v", p.Order)
	}
	_, err = BuildPlan([]MetricDataQuery{
		{ID: "a", Expression: "b + 1"},
		{ID: "b", Expression: "c * 2"},
		{ID: "c", Expression: "a - 1"},
		{ID: "m1", MetricStat: ms},
	})
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "a, b, c") {
		t.Fatalf("expected cycle error, got %v", err)
	}
	for _, bad := range [][]MetricDataQuery{
		{{ID: "e1", Expression: "m9 + 1"}},
		{{ID: "e1", Expression: "e1 + 1"}},
		{{ID: "Bad", Expression: "1"}},
		{{ID: "m1", MetricStat: ms}, {ID: "m1", MetricStat: ms}},
		{{ID: "m1"}},
	} {
		if _, err := BuildPlan(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("expected bad request for %+v, got %v", bad, err)
		}
	}
}

func TestAlignmentFloorsToPeriod(t *testing.T) {
	g, err := NewGrid(60_500, 300_000, 60)
	if err != nil {
		t.Fatal(err)
	}
	if g.Start != 60_000 || g.N != 4 {
		t.Fatalf("grid %+v", g)
	}
	// jittered points: 61s,119.9s land in bucket 0 (avg), 185s in bucket 2
	v := Align([]model.Point{{T: 61_000, V: 10}, {T: 119_900, V: 20}, {T: 185_000, V: 7}, {T: 999_999, V: 1}}, g)
	if v[0] != 15 || !math.IsNaN(v[1]) || v[2] != 7 || !math.IsNaN(v[3]) {
		t.Fatalf("aligned %v", v)
	}
	if AlignTimestamp(-1, 60_000) != -60_000 {
		t.Fatal("negative floor")
	}
}

func TestFillPolicies(t *testing.T) {
	n := math.NaN()
	in := []float64{n, 2, n, n, 8, n}
	eq := func(a, b []float64) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if got := Fill(in, FillPolicy{Kind: "VALUE", Value: 0}); !eq(got, []float64{0, 2, 0, 0, 8, 0}) {
		t.Errorf("value %v", got)
	}
	if got := Fill(in, FillPolicy{Kind: "REPEAT"}); !eq(got, []float64{2, 2, 2, 2, 8, 8}) {
		t.Errorf("repeat %v", got)
	}
	if got := Fill(in, FillPolicy{Kind: "LINEAR"}); !eq(got, []float64{2, 2, 4, 6, 8, 8}) {
		t.Errorf("linear %v", got)
	}
	if got := Fill(in, FillPolicy{Kind: "NONE"}); !math.IsNaN(got[0]) {
		t.Errorf("none %v", got)
	}
	if _, err := ParseFillPolicy("bogus"); err == nil {
		t.Error("expected error")
	}
}

type fakeFetcher map[string][]model.Point

func (f fakeFetcher) FetchStat(_ context.Context, _ uint32, ms MetricStat, _, _ int64, _ int) ([]model.Point, error) {
	if ms.MetricName == "broken" {
		return nil, errors.New("shard down")
	}
	if ms.MetricName == "partial" {
		return nil, &PartialError{Points: f["a"], Msg: "1 of 3 shards unreachable"}
	}
	return f[ms.MetricName], nil
}

// A fetcher's PartialError keeps the points it has and marks the query (and
// expressions built on it) PartialData, not InternalError.
func TestPartialData(t *testing.T) {
	base := int64(1_700_000_040_000) / 60_000 * 60_000
	f := fakeFetcher{"a": {{T: base, V: 1}, {T: base + 60_000, V: 2}}}
	e := &Engine{Fetcher: f}
	res, err := e.Execute(context.Background(), Request{TenantID: 1, StartMs: base, EndMs: base + 120_000, Period: 60, Queries: []MetricDataQuery{
		{ID: "m1", MetricStat: &MetricStat{Namespace: "App", MetricName: "partial", Stat: "Sum"}},
		{ID: "e1", Expression: "m1 * 2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.StatusCode != "PartialData" {
			t.Errorf("%s: status %q, want PartialData", r.ID, r.StatusCode)
		}
	}
	if res[0].Values[0] != 1 || len(res[0].Messages) != 1 || res[1].Values[1] != 4 {
		t.Errorf("partial values/messages lost: %+v", res)
	}
}

func TestEngineEndToEnd(t *testing.T) {
	f := fakeFetcher{
		"a":   {{T: 0, V: 10}, {T: 60_000, V: 20}, {T: 120_000, V: 30}, {T: 180_000, V: 40}},
		"b":   {{T: 0, V: 30}, {T: 60_000, V: 40}, {T: 180_000, V: 60}}, // gap at 120s
		"req": {{T: 0, V: 600}, {T: 60_000, V: 1200}, {T: 120_000, V: 1800}, {T: 180_000, V: 2400}},
	}
	e := &Engine{Fetcher: f}
	no := false
	res, err := e.Execute(context.Background(), Request{
		StartMs: 0, EndMs: 240_000, Period: 60,
		Queries: []MetricDataQuery{
			{ID: "m1", MetricStat: &MetricStat{MetricName: "a", Stat: "Average"}, ReturnData: &no},
			{ID: "m2", MetricStat: &MetricStat{MetricName: "b", Stat: "Average"}, ReturnData: &no},
			{ID: "m3", MetricStat: &MetricStat{MetricName: "req", Stat: "Sum"}, ReturnData: &no},
			{ID: "e1", Expression: "(m1 + m2) / 2"},
			{ID: "e2", Expression: "RATE(m1)"},
			{ID: "e3", Expression: "(FILL(m1, 0) + FILL(m2, LINEAR)) / 2"},
			{ID: "e4", Expression: "m3 / PERIOD(m3)"},
			{ID: "e5", Expression: "DIFF(m1)"},
			{ID: "e6", Expression: "m1 - AVG(m1)"},
			{ID: "e7", Expression: "MAX(m1, m2)"},
			{ID: "e8", Expression: "m1 / 0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Result{}
	for _, r := range res {
		byID[r.ID] = r
	}
	if len(res) != 8 {
		t.Fatalf("returnData=false should hide metric queries; got %d results", len(res))
	}
	check := func(id string, wantTS []int64, want []float64) {
		t.Helper()
		r := byID[id]
		if len(r.Values) != len(want) {
			t.Fatalf("%s: got %v want %v", id, r.Values, want)
		}
		for i := range want {
			if math.Abs(r.Values[i]-want[i]) > 1e-9 || r.Timestamps[i] != wantTS[i] {
				t.Fatalf("%s: got %v@%v want %v@%v", id, r.Values, r.Timestamps, want, wantTS)
			}
		}
	}
	all := []int64{0, 60_000, 120_000, 180_000}
	check("e1", []int64{0, 60_000, 180_000}, []float64{20, 30, 50}) // missing m2 point dropped
	check("e2", []int64{60_000, 120_000, 180_000}, []float64{10.0 / 60, 10.0 / 60, 10.0 / 60})
	check("e3", all, []float64{20, 30, 40, 50})
	check("e4", all, []float64{10, 20, 30, 40})
	check("e5", all[1:], []float64{10, 10, 10})
	check("e6", all, []float64{-15, -5, 5, 15})
	check("e7", all, []float64{30, 40, 30, 60})
	check("e8", nil, nil)
}

func TestEnginePartialData(t *testing.T) {
	e := &Engine{Fetcher: fakeFetcher{"a": {{T: 0, V: 1}}}}
	res, err := e.Execute(context.Background(), Request{StartMs: 0, EndMs: 60_000, Period: 60, Queries: []MetricDataQuery{
		{ID: "m1", MetricStat: &MetricStat{MetricName: "a"}},
		{ID: "m2", MetricStat: &MetricStat{MetricName: "broken"}},
		{ID: "e1", Expression: "m1 + m2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res[1].StatusCode != "InternalError" || res[2].StatusCode != "PartialData" {
		t.Fatalf("statuses %+v", res)
	}
}

func TestEngineRequestFill(t *testing.T) {
	e := &Engine{Fetcher: fakeFetcher{"a": {{T: 0, V: 1}, {T: 120_000, V: 3}}}}
	res, err := e.Execute(context.Background(), Request{StartMs: 0, EndMs: 180_000, Period: 60, Fill: "LINEAR",
		Queries: []MetricDataQuery{{ID: "m1", MetricStat: &MetricStat{MetricName: "a"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].Values) != 3 || res[0].Values[1] != 2 {
		t.Fatalf("got %v", res[0].Values)
	}
}

func TestEngineBadGrid(t *testing.T) {
	e := &Engine{Fetcher: fakeFetcher{}}
	_, err := e.Execute(context.Background(), Request{StartMs: 0, EndMs: 1e12, Period: 1, Queries: []MetricDataQuery{{ID: "e1", Expression: "1"}}})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("got %v", err)
	}
}
