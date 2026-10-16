package model

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSeriesIDOrderIndependent(t *testing.T) {
	a := SeriesID(1, "ns", "m", []Dimension{{"a", "1"}, {"b", "2"}})
	b := SeriesID(1, "ns", "m", []Dimension{{"b", "2"}, {"a", "1"}})
	if a != b {
		t.Fatal("dimension order must not affect SeriesID")
	}
	if a == SeriesID(2, "ns", "m", []Dimension{{"a", "1"}, {"b", "2"}}) {
		t.Fatal("tenant must affect SeriesID")
	}
	if SeriesID(1, "ab", "c", nil) == SeriesID(1, "a", "bc", nil) {
		t.Fatal("concatenation collision")
	}
}

func TestValidate(t *testing.T) {
	now := time.Now()
	ok := MetricDatum{Namespace: "n", MetricName: "m", Timestamp: now.UnixMilli(), Value: Float(1)}
	if err := ok.Validate(now); err != nil {
		t.Fatal(err)
	}
	cases := map[string]MetricDatum{
		"no value":    {Namespace: "n", MetricName: "m", Timestamp: now.UnixMilli()},
		"too old":     {Namespace: "n", MetricName: "m", Timestamp: now.Add(-15 * 24 * time.Hour).UnixMilli(), Value: Float(1)},
		"future":      {Namespace: "n", MetricName: "m", Timestamp: now.Add(3 * time.Hour).UnixMilli(), Value: Float(1)},
		"dup dim":     {Namespace: "n", MetricName: "m", Timestamp: now.UnixMilli(), Value: Float(1), Dimensions: []Dimension{{"a", "1"}, {"a", "2"}}},
		"long dim":    {Namespace: "n", MetricName: "m", Timestamp: now.UnixMilli(), Value: Float(1), Dimensions: []Dimension{{"a", strings.Repeat("x", 256)}}},
		"bad res":     {Namespace: "n", MetricName: "m", Timestamp: now.UnixMilli(), Value: Float(1), StorageResolution: 5},
		"bad statset": {Namespace: "n", MetricName: "m", Timestamp: now.UnixMilli(), StatisticValues: &StatisticSet{SampleCount: 0}},
	}
	for name, c := range cases {
		if err := c.Validate(now); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: expected validation error, got %v", name, err)
		}
	}
}
