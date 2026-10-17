package ingest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

// OTLP/HTTP JSON encoding (opentelemetry-proto, JSON mapping). Only the
// fields ZeroWatch needs are modelled; unknown fields are ignored as the
// spec requires. int64 fields may arrive as strings (proto3 JSON mapping).

type otlpAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	IntValue    json.RawMessage `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
}

func (v otlpAnyValue) String() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case len(v.IntValue) > 0:
		return strings.Trim(string(v.IntValue), `"`)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	}
	return ""
}

type otlpKV struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpResource struct {
	Attributes []otlpKV `json:"attributes"`
}

type otlpNumberDP struct {
	Attributes   []otlpKV        `json:"attributes"`
	TimeUnixNano json.RawMessage `json:"timeUnixNano"`
	AsDouble     *float64        `json:"asDouble,omitempty"`
	AsInt        json.RawMessage `json:"asInt,omitempty"`
}

type otlpHistDP struct {
	Attributes   []otlpKV        `json:"attributes"`
	TimeUnixNano json.RawMessage `json:"timeUnixNano"`
	Count        json.RawMessage `json:"count"`
	Sum          *float64        `json:"sum,omitempty"`
	Min          *float64        `json:"min,omitempty"`
	Max          *float64        `json:"max,omitempty"`
}

type otlpNumberDPs struct {
	DataPoints []otlpNumberDP `json:"dataPoints"`
}

type otlpHistDPs struct {
	DataPoints []otlpHistDP `json:"dataPoints"`
}

type otlpMetric struct {
	Name      string         `json:"name"`
	Unit      string         `json:"unit"`
	Gauge     *otlpNumberDPs `json:"gauge,omitempty"`
	Sum       *otlpNumberDPs `json:"sum,omitempty"`
	Histogram *otlpHistDPs   `json:"histogram,omitempty"`
}

type otlpScopeMetrics struct {
	Metrics []otlpMetric `json:"metrics"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

// OTLPMetricsRequest is ExportMetricsServiceRequest. The protobuf path
// (otlp_proto.go) converts into these same structs so both encodings share
// one conversion to the model.
type OTLPMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpLogRecord struct {
	TimeUnixNano         json.RawMessage `json:"timeUnixNano"`
	ObservedTimeUnixNano json.RawMessage `json:"observedTimeUnixNano"`
	SeverityText         string          `json:"severityText"`
	SeverityNumber       int             `json:"severityNumber"`
	Body                 otlpAnyValue    `json:"body"`
	Attributes           []otlpKV        `json:"attributes"`
	TraceID              string          `json:"traceId"`
	SpanID               string          `json:"spanId"`
}

type otlpScopeLogs struct {
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

// OTLPLogsRequest is ExportLogsServiceRequest.
type OTLPLogsRequest struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

func rawInt(r json.RawMessage) (int64, bool) {
	s := strings.Trim(string(r), `"`)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func nanoToMs(r json.RawMessage) int64 {
	v, ok := rawInt(r)
	if !ok || v <= 0 {
		return 0 // pipeline defaults to "now"
	}
	return v / 1e6
}

func attrsToDims(res, dp []otlpKV) []model.Dimension {
	m := make(map[string]string, len(dp)+2)
	for _, kv := range res {
		// Only identity-bearing resource attributes become dimensions, to keep
		// cardinality bounded (host/process attrs would explode series count).
		switch kv.Key {
		case "service.name", "service.namespace", "deployment.environment", "k8s.namespace.name":
			m[kv.Key] = kv.Value.String()
		}
	}
	for _, kv := range dp {
		m[kv.Key] = kv.Value.String()
	}
	dims := make([]model.Dimension, 0, len(m))
	for k, v := range m {
		if k == "" || v == "" {
			continue
		}
		dims = append(dims, model.Dimension{Name: k, Value: v})
		if len(dims) == model.MaxDimensions {
			break
		}
	}
	return model.SortDimensions(dims)
}

func resourceNamespace(res otlpResource) string {
	for _, kv := range res.Attributes {
		if kv.Key == "service.namespace" && kv.Value.String() != "" {
			return kv.Value.String()
		}
	}
	return "OTLP"
}

// ConvertOTLPMetrics maps OTLP gauges/sums to Value datums and histograms
// to StatisticSet datums (count/sum/min/max — exact for Sum/SampleCount/
// Average/Min/Max; percentiles need raw samples or exponential histograms).
func ConvertOTLPMetrics(req *OTLPMetricsRequest) ([]model.MetricDatum, error) {
	var out []model.MetricDatum
	for _, rm := range req.ResourceMetrics {
		ns := resourceNamespace(rm.Resource)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name == "" {
					return nil, fmt.Errorf("%w: metric without name", model.ErrValidation)
				}
				var nums []otlpNumberDP
				if m.Gauge != nil {
					nums = append(nums, m.Gauge.DataPoints...)
				}
				if m.Sum != nil {
					nums = append(nums, m.Sum.DataPoints...)
				}
				for _, dp := range nums {
					var v float64
					switch {
					case dp.AsDouble != nil:
						v = *dp.AsDouble
					case len(dp.AsInt) > 0:
						iv, ok := rawInt(dp.AsInt)
						if !ok {
							return nil, fmt.Errorf("%w: bad asInt on %s", model.ErrValidation, m.Name)
						}
						v = float64(iv)
					default:
						continue
					}
					out = append(out, model.MetricDatum{
						Namespace: ns, MetricName: m.Name, Unit: m.Unit,
						Dimensions: attrsToDims(rm.Resource.Attributes, dp.Attributes),
						Timestamp:  nanoToMs(dp.TimeUnixNano), Value: model.Float(v),
					})
				}
				if m.Histogram != nil {
					for _, dp := range m.Histogram.DataPoints {
						cnt, _ := rawInt(dp.Count)
						if cnt <= 0 || dp.Sum == nil {
							continue
						}
						mean := *dp.Sum / float64(cnt)
						s := &model.StatisticSet{SampleCount: float64(cnt), Sum: *dp.Sum, Minimum: mean, Maximum: mean}
						if dp.Min != nil {
							s.Minimum = *dp.Min
						}
						if dp.Max != nil {
							s.Maximum = *dp.Max
						}
						out = append(out, model.MetricDatum{
							Namespace: ns, MetricName: m.Name, Unit: m.Unit,
							Dimensions:      attrsToDims(rm.Resource.Attributes, dp.Attributes),
							Timestamp:       nanoToMs(dp.TimeUnixNano),
							StatisticValues: s,
						})
					}
				}
			}
		}
	}
	return out, nil
}

// ConvertOTLPLogs maps log records: log group = service.name (or "otlp"),
// stream = host.name (or "default").
func ConvertOTLPLogs(req *OTLPLogsRequest) []model.LogEvent {
	var out []model.LogEvent
	for _, rl := range req.ResourceLogs {
		group, stream := "otlp", "default"
		for _, kv := range rl.Resource.Attributes {
			switch kv.Key {
			case "service.name":
				group = kv.Value.String()
			case "host.name":
				stream = kv.Value.String()
			}
		}
		for _, sl := range rl.ScopeLogs {
			for _, r := range sl.LogRecords {
				ts := nanoToMs(r.TimeUnixNano)
				if ts == 0 {
					ts = nanoToMs(r.ObservedTimeUnixNano)
				}
				attrs := map[string]string{}
				for _, kv := range r.Attributes {
					attrs[kv.Key] = kv.Value.String()
				}
				if r.TraceID != "" {
					attrs["trace_id"] = r.TraceID
				}
				if r.SpanID != "" {
					attrs["span_id"] = r.SpanID
				}
				out = append(out, model.LogEvent{
					LogGroup: group, LogStream: stream, Timestamp: ts,
					Message: r.Body.String(), Level: severity(r.SeverityText, r.SeverityNumber),
					Attributes: attrs,
				})
			}
		}
	}
	return out
}

// severity maps OTLP SeverityNumber ranges (1-4 trace, 5-8 debug, 9-12 info,
// 13-16 warn, 17-20 error, 21-24 fatal).
func severity(text string, num int) string {
	if text != "" {
		return strings.ToLower(text)
	}
	switch {
	case num == 0:
		return ""
	case num <= 4:
		return "trace"
	case num <= 8:
		return "debug"
	case num <= 12:
		return "info"
	case num <= 16:
		return "warn"
	case num <= 20:
		return "error"
	}
	return "fatal"
}
