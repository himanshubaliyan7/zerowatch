package emf

import (
	"errors"
	"testing"
)

const blueprintDoc = `{
  "_aws": {
    "Timestamp": 1697040000,
    "CloudWatchMetrics": [
      {
        "Namespace": "CustomApp/Payments",
        "Dimensions": [["Service", "Environment"]],
        "Metrics": [
          { "Name": "Latency", "Unit": "Milliseconds" },
          { "Name": "RequestCount", "Unit": "Count" }
        ]
      }
    ]
  },
  "Service": "Checkout",
  "Environment": "Production",
  "Latency": 84.2,
  "RequestCount": 1
}`

func TestExtractBlueprintExample(t *testing.T) {
	pts, err := Extract(7, []byte(blueprintDoc), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("want 2 points got %d", len(pts))
	}
	p := pts[0]
	if p.Namespace != "CustomApp/Payments" || p.MetricName != "Latency" || *p.Value != 84.2 || p.Unit != "Milliseconds" {
		t.Fatalf("bad point %+v", p)
	}
	if p.Timestamp != 1697040000000 {
		t.Fatalf("seconds timestamp should be normalised to ms, got %d", p.Timestamp)
	}
	if p.TenantID != 7 || len(p.Dimensions) != 2 || p.Dimensions[0].Value != "Checkout" {
		t.Fatalf("bad dims %+v", p.Dimensions)
	}
}

func TestMultipleDimensionSetsAndArrays(t *testing.T) {
	doc := `{"_aws":{"Timestamp":1697040000123,"CloudWatchMetrics":[{"Namespace":"A","Dimensions":[["svc"],["svc","az"]],"Metrics":[{"Name":"lat","StorageResolution":1}]}]},
	"svc":"api","az":"us-1a","lat":[1,2,3]}`
	pts, err := Extract(1, []byte(doc), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 6 { // 2 dim sets x 3 values
		t.Fatalf("want 6 got %d", len(pts))
	}
	if pts[0].Timestamp != 1697040000123 || pts[0].StorageResolution != 1 {
		t.Fatalf("bad %+v", pts[0])
	}
}

func TestNoDimensionsAndFallbackTimestamp(t *testing.T) {
	doc := `{"_aws":{"CloudWatchMetrics":[{"Namespace":"A","Dimensions":[],"Metrics":[{"Name":"x"}]}]},"x":5}`
	pts, err := Extract(1, []byte(doc), 42)
	if err != nil || len(pts) != 1 || pts[0].Timestamp != 42 || len(pts[0].Dimensions) != 0 {
		t.Fatalf("got %+v %v", pts, err)
	}
}

func TestNotEMF(t *testing.T) {
	for _, l := range []string{`plain text`, `{"level":"info"}`, `{"_aws": broken`} {
		if _, err := Extract(1, []byte(l), 0); !errors.Is(err, ErrNotEMF) {
			t.Errorf("%q: want ErrNotEMF got %v", l, err)
		}
	}
}

func TestInvalidEMF(t *testing.T) {
	cases := []string{
		`{"_aws":{"CloudWatchMetrics":[]}}`,
		`{"_aws":{"CloudWatchMetrics":[{"Namespace":"A","Dimensions":[["svc"]],"Metrics":[{"Name":"x"}]}]},"x":1}`, // missing dim
		`{"_aws":{"CloudWatchMetrics":[{"Namespace":"A","Metrics":[{"Name":"x"}]}]},"x":"high"}`,                   // non-numeric
		`{"_aws":{"CloudWatchMetrics":[{"Metrics":[{"Name":"x"}]}]},"x":1}`,                                        // no namespace
		`{"_aws":{"CloudWatchMetrics":[{"Namespace":"A","Metrics":[{"Name":"x"}]}]}}`,                              // missing metric
	}
	for _, c := range cases {
		if _, err := Extract(1, []byte(c), 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid got %v", c, err)
		}
	}
}
