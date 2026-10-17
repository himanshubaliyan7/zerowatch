// Package emf extracts metrics from CloudWatch Embedded Metric Format (EMF)
// log lines. EMF lets applications emit metrics simply by writing structured
// JSON logs; an asynchronous extractor turns them into metric points without
// any PutMetricData call.
//
// Specification: https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format_Specification.html
package emf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

// Limits from the EMF specification.
const (
	MaxMetricsPerDirective = 100
	MaxDimensionsPerSet    = 30
	MaxValuesPerMetric     = 100
)

var (
	// ErrNotEMF means the line is not an EMF document (no _aws root key). Callers
	// should treat it as a regular log line, not as a failure.
	ErrNotEMF = errors.New("emf: not an EMF document")
	// ErrInvalid means the line claims to be EMF but violates the spec.
	ErrInvalid = errors.New("emf: invalid document")
)

type metricDef struct {
	Name              string `json:"Name"`
	Unit              string `json:"Unit"`
	StorageResolution uint32 `json:"StorageResolution"`
}

type directive struct {
	Namespace  string      `json:"Namespace"`
	Dimensions [][]string  `json:"Dimensions"`
	Metrics    []metricDef `json:"Metrics"`
}

type awsMeta struct {
	Timestamp         json.Number `json:"Timestamp"`
	CloudWatchMetrics []directive `json:"CloudWatchMetrics"`
}

// IsCandidate is a cheap pre-filter: only lines that look like JSON objects
// containing "_aws" are worth decoding.
func IsCandidate(line []byte) bool {
	t := bytes.TrimSpace(line)
	return len(t) > 0 && t[0] == '{' && bytes.Contains(t, []byte(`"_aws"`))
}

// Extract parses a single EMF log line and returns the synthetic metric points.
// fallbackTS (unix ms) is used if the document's Timestamp is absent.
func Extract(tenant uint32, line []byte, fallbackTS int64) ([]model.MetricDatum, error) {
	if !IsCandidate(line) {
		return nil, ErrNotEMF
	}
	var root map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, ErrNotEMF
	}
	rawAws, ok := root["_aws"]
	if !ok {
		return nil, ErrNotEMF
	}
	var meta awsMeta
	if err := json.Unmarshal(rawAws, &meta); err != nil {
		return nil, fmt.Errorf("%w: _aws: %v", ErrInvalid, err)
	}
	if len(meta.CloudWatchMetrics) == 0 {
		return nil, fmt.Errorf("%w: CloudWatchMetrics is empty", ErrInvalid)
	}
	ts := fallbackTS
	if meta.Timestamp != "" {
		n, err := meta.Timestamp.Int64()
		if err != nil {
			return nil, fmt.Errorf("%w: Timestamp: %v", ErrInvalid, err)
		}
		// The spec mandates epoch milliseconds, but seconds are common in the
		// wild (and appear in the blueprint); anything < 1e11 is seconds.
		if n < 100_000_000_000 {
			n *= 1000
		}
		ts = n
	}

	var out []model.MetricDatum
	for di, d := range meta.CloudWatchMetrics {
		if d.Namespace == "" {
			return nil, fmt.Errorf("%w: directive %d missing Namespace", ErrInvalid, di)
		}
		if len(d.Metrics) == 0 || len(d.Metrics) > MaxMetricsPerDirective {
			return nil, fmt.Errorf("%w: directive %d has %d metrics", ErrInvalid, di, len(d.Metrics))
		}
		dimSets := d.Dimensions
		if len(dimSets) == 0 {
			dimSets = [][]string{{}} // metric with no dimensions
		}
		for _, set := range dimSets {
			if len(set) > MaxDimensionsPerSet {
				return nil, fmt.Errorf("%w: dimension set exceeds %d keys", ErrInvalid, MaxDimensionsPerSet)
			}
			dims := make([]model.Dimension, 0, len(set))
			for _, key := range set {
				v, err := stringMember(root, key)
				if err != nil {
					return nil, err
				}
				dims = append(dims, model.Dimension{Name: key, Value: v})
			}
			for _, m := range d.Metrics {
				vals, err := numericMember(root, m.Name)
				if err != nil {
					return nil, err
				}
				res := m.StorageResolution
				if res == 0 {
					res = 60
				}
				for _, v := range vals {
					out = append(out, model.MetricDatum{
						TenantID:          tenant,
						Namespace:         d.Namespace,
						MetricName:        m.Name,
						Dimensions:        dims,
						Timestamp:         ts,
						Value:             model.Float(v),
						Unit:              m.Unit,
						StorageResolution: res,
					})
				}
			}
		}
	}
	return out, nil
}

func stringMember(root map[string]json.RawMessage, key string) (string, error) {
	raw, ok := root[key]
	if !ok {
		return "", fmt.Errorf("%w: dimension %q missing from root", ErrInvalid, key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	// Numbers/bools are tolerated and stringified.
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b), nil
	}
	return "", fmt.Errorf("%w: dimension %q is not a scalar", ErrInvalid, key)
}

func numericMember(root map[string]json.RawMessage, key string) ([]float64, error) {
	raw, ok := root[key]
	if !ok {
		return nil, fmt.Errorf("%w: metric %q missing from root", ErrInvalid, key)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return []float64{f}, nil
	}
	var arr []float64
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) == 0 || len(arr) > MaxValuesPerMetric {
			return nil, fmt.Errorf("%w: metric %q array has %d values", ErrInvalid, key, len(arr))
		}
		return arr, nil
	}
	return nil, fmt.Errorf("%w: metric %q is not numeric", ErrInvalid, key)
}
