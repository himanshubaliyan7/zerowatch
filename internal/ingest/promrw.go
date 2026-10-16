package ingest

import (
	"fmt"
	"math"

	"github.com/golang/snappy"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/zerowatch/zerowatch/internal/model"
)

// Prometheus remote write v1 (prometheus/prompb):
//
//	message WriteRequest { repeated TimeSeries timeseries = 1; ... }
//	message TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	message Label        { string name = 1; string value = 2; }
//	message Sample       { double value = 1; int64 timestamp = 2; }
//
// We hand-decode with protowire instead of generating code: zero codegen,
// no reflection, and it lets us skip fields (exemplars, histograms,
// metadata) cheaply. Body is snappy *block* encoded.

// PromNamespace is the namespace assigned to remote-write series.
const PromNamespace = "Prometheus"

// MaxPromDecoded bounds the decompressed body (zip-bomb protection).
const MaxPromDecoded = 32 << 20

// DecodeRemoteWrite decompresses and decodes a remote-write body.
func DecodeRemoteWrite(body []byte) ([]model.MetricDatum, error) {
	n, err := snappy.DecodedLen(body)
	if err != nil {
		return nil, fmt.Errorf("%w: snappy: %v", model.ErrValidation, err)
	}
	if n > MaxPromDecoded {
		return nil, fmt.Errorf("%w: decoded body %d bytes exceeds %d", model.ErrValidation, n, MaxPromDecoded)
	}
	raw, err := snappy.Decode(nil, body)
	if err != nil {
		return nil, fmt.Errorf("%w: snappy: %v", model.ErrValidation, err)
	}
	return decodeWriteRequest(raw)
}

func decodeWriteRequest(b []byte) ([]model.MetricDatum, error) {
	var out []model.MetricDatum
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protoErr(n)
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			msg, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, protoErr(m)
			}
			b = b[m:]
			ds, err := decodeTimeSeries(msg)
			if err != nil {
				return nil, err
			}
			out = append(out, ds...)
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return nil, protoErr(m)
		}
		b = b[m:]
	}
	return out, nil
}

func decodeTimeSeries(b []byte) ([]model.MetricDatum, error) {
	var (
		name    string
		dims    []model.Dimension
		samples []model.Point
	)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protoErr(n)
		}
		b = b[n:]
		if typ != protowire.BytesType || (num != 1 && num != 2) {
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return nil, protoErr(m)
			}
			b = b[m:]
			continue
		}
		msg, m := protowire.ConsumeBytes(b)
		if m < 0 {
			return nil, protoErr(m)
		}
		b = b[m:]
		if num == 1 {
			k, v, err := decodeLabel(msg)
			if err != nil {
				return nil, err
			}
			if k == "__name__" {
				name = v
			} else if k != "" && v != "" && len(dims) < model.MaxDimensions {
				dims = append(dims, model.Dimension{Name: k, Value: v})
			}
		} else {
			p, err := decodeSample(msg)
			if err != nil {
				return nil, err
			}
			samples = append(samples, p)
		}
	}
	if name == "" {
		return nil, fmt.Errorf("%w: timeseries without __name__", model.ErrValidation)
	}
	dims = model.SortDimensions(dims)
	out := make([]model.MetricDatum, 0, len(samples))
	for _, s := range samples {
		// Prometheus staleness markers are a special NaN; drop all NaN/Inf.
		if math.IsNaN(s.V) || math.IsInf(s.V, 0) {
			continue
		}
		out = append(out, model.MetricDatum{
			Namespace: PromNamespace, MetricName: name, Dimensions: dims,
			Timestamp: s.T, Value: model.Float(s.V),
		})
	}
	return out, nil
}

func decodeLabel(b []byte) (k, v string, err error) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", "", protoErr(n)
		}
		b = b[n:]
		if typ == protowire.BytesType && (num == 1 || num == 2) {
			s, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return "", "", protoErr(m)
			}
			b = b[m:]
			if num == 1 {
				k = string(s)
			} else {
				v = string(s)
			}
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return "", "", protoErr(m)
		}
		b = b[m:]
	}
	return k, v, nil
}

func decodeSample(b []byte) (model.Point, error) {
	var p model.Point
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return p, protoErr(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.Fixed64Type:
			v, m := protowire.ConsumeFixed64(b)
			if m < 0 {
				return p, protoErr(m)
			}
			p.V = math.Float64frombits(v)
			b = b[m:]
		case num == 2 && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return p, protoErr(m)
			}
			p.T = int64(v)
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return p, protoErr(m)
			}
			b = b[m:]
		}
	}
	return p, nil
}

func protoErr(n int) error {
	return fmt.Errorf("%w: protobuf: %v", model.ErrValidation, protowire.ParseError(n))
}

// EncodeRemoteWrite is the inverse of DecodeRemoteWrite (used by tests and
// the federation exporter). Each datum becomes one TimeSeries.
func EncodeRemoteWrite(ms []model.MetricDatum) []byte {
	var req []byte
	for _, m := range ms {
		var ts []byte
		ts = appendLabel(ts, "__name__", m.MetricName)
		for _, d := range m.Dimensions {
			ts = appendLabel(ts, d.Name, d.Value)
		}
		var s []byte
		v := 0.0
		if m.Value != nil {
			v = *m.Value
		}
		s = protowire.AppendTag(s, 1, protowire.Fixed64Type)
		s = protowire.AppendFixed64(s, math.Float64bits(v))
		s = protowire.AppendTag(s, 2, protowire.VarintType)
		s = protowire.AppendVarint(s, uint64(m.Timestamp))
		ts = protowire.AppendTag(ts, 2, protowire.BytesType)
		ts = protowire.AppendBytes(ts, s)
		req = protowire.AppendTag(req, 1, protowire.BytesType)
		req = protowire.AppendBytes(req, ts)
	}
	return snappy.Encode(nil, req)
}

func appendLabel(b []byte, k, v string) []byte {
	var l []byte
	l = protowire.AppendTag(l, 1, protowire.BytesType)
	l = protowire.AppendString(l, k)
	l = protowire.AppendTag(l, 2, protowire.BytesType)
	l = protowire.AppendString(l, v)
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	return protowire.AppendBytes(b, l)
}
