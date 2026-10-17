package traces

import (
	"math"
	"sort"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

// REDNamespace matches the namespace the eBPF probe and the generator use,
// so traced services show up on Overview and the Map like probed ones.
const REDNamespace = "ZeroWatch/RED"

// MaxLatencySamplesPerGroup caps raw latency samples per (service, route,
// second) in one batch; Requests/Errors stay exact, percentiles become a
// sample once a route exceeds this rate within a single export.
const MaxLatencySamplesPerGroup = 200

type redKey struct {
	service, route string
	sec            int64
}

type redAgg struct {
	reqs, errs int
	lat        []model.MetricDatum
}

// REDMetrics derives Requests/Errors counts (per second) and raw Latency
// samples from SERVER spans, with dims {Route: span name, Service}. Spans
// tagged SyntheticAttr are skipped: the generator writes RED itself.
//
// Not deduplicated against the probe: a service that is both traced and
// probed under the same Service name is counted twice.
func REDMetrics(spans []Span) []model.MetricDatum {
	groups := map[redKey]*redAgg{}
	var keys []redKey
	for i := range spans {
		sp := &spans[i]
		if sp.Kind != KindServer || sp.Attributes[SyntheticAttr] == "true" {
			continue
		}
		startMs := int64(sp.Start)
		k := redKey{service: sp.Service, route: truncate(sp.Name, model.MaxDimensionLength), sec: startMs / 1000 * 1000}
		g := groups[k]
		if g == nil {
			g = &redAgg{}
			groups[k] = g
			keys = append(keys, k)
		}
		g.reqs++
		if sp.IsError() {
			g.errs++
		}
		if len(g.lat) < MaxLatencySamplesPerGroup {
			g.lat = append(g.lat, model.MetricDatum{Namespace: REDNamespace, MetricName: "Latency", Dimensions: redDims(k),
				Timestamp: startMs, Value: model.Float(math.Round(sp.DurationMs*100) / 100), Unit: "Milliseconds"})
		}
	}
	// Deterministic order keeps tests and commit-log batches stable.
	sort.Slice(keys, func(a, b int) bool {
		ka, kb := keys[a], keys[b]
		if ka.service != kb.service {
			return ka.service < kb.service
		}
		if ka.route != kb.route {
			return ka.route < kb.route
		}
		return ka.sec < kb.sec
	})
	var out []model.MetricDatum
	for _, k := range keys {
		g := groups[k]
		dims := redDims(k)
		out = append(out,
			model.MetricDatum{Namespace: REDNamespace, MetricName: "Requests", Dimensions: dims, Timestamp: k.sec, Value: model.Float(float64(g.reqs)), Unit: "Count"},
			model.MetricDatum{Namespace: REDNamespace, MetricName: "Errors", Dimensions: dims, Timestamp: k.sec, Value: model.Float(float64(g.errs)), Unit: "Count"},
		)
		out = append(out, g.lat...)
	}
	return out
}

func redDims(k redKey) []model.Dimension {
	return []model.Dimension{{Name: "Route", Value: k.route}, {Name: "Service", Value: k.service}}
}
