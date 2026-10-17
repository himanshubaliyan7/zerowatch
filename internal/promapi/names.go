package promapi

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
)

// Name mapping between ZeroWatch series and Prometheus label sets.
//
// A series (namespace, metric name, dimensions) is exposed as:
//
//	__name__  = sanitize(namespace + "_" + metricName)
//	namespace = namespace (verbatim)
//	metric    = metricName (verbatim)
//	<dim>     = dimension value, for each dimension
//	stat      = the statistic (Average by default; see below)
//
// sanitize replaces every character outside [a-zA-Z0-9_:] with "_" and
// prefixes "_" when the result starts with a digit. Case is kept, so
// ZeroWatch/Host + CPUUtilization is ZeroWatch_Host_CPUUtilization.
// Dimension names are sanitized the same way except that ":" is also
// replaced (label names cannot hold it); a dimension whose sanitized name
// is reserved (__name__, namespace, metric, stat, or any "__" prefix) is
// exposed as dim_<name>. Different series can map to the same __name__;
// the namespace and metric labels keep them apart.
//
// The stat label picks the statistic each sample is computed with: one of
// Average, Sum, Minimum, Maximum, SampleCount or pNN (p99, p99.9). It is
// not stored: stat="Sum" selects Sum; stat!=/=~/!~ select among the base
// statistics (Average, Sum, Minimum, Maximum, SampleCount and p50, p90, p95,
// p99); without a stat matcher the series is read as Average.

const (
	nameLabel = "__name__"
	nsLabel   = "namespace"
	metLabel  = "metric"
	statLabel = "stat"
	defStat   = "Average"
)

// baseStats are the statistics stat!=, =~ and !~ choose from.
var baseStats = []string{"Average", "Sum", "Minimum", "Maximum", "SampleCount", "p50", "p90", "p95", "p99"}

// SanitizeMetricName maps namespace and metric to a Prometheus metric name.
func SanitizeMetricName(namespace, metric string) string {
	return sanitize(namespace+"_"+metric, true)
}

func sanitize(s string, allowColon bool) string {
	var b strings.Builder
	b.Grow(len(s) + 1)
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || allowColon && c == ':'
		if i == 0 && c >= '0' && c <= '9' {
			b.WriteByte('_')
		}
		if ok {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func dimLabel(dim string) string {
	l := sanitize(dim, false)
	switch {
	case l == nsLabel || l == metLabel || l == statLabel || strings.HasPrefix(l, "__"):
		return "dim_" + l
	}
	return l
}

// entry is one stored series with its Prometheus labels (stat excluded).
type entry struct {
	meta   model.SeriesMeta
	labels map[string]string
}

func newEntry(m model.SeriesMeta) entry {
	l := make(map[string]string, len(m.Dimensions)+3)
	for k, v := range m.Dimensions {
		l[dimLabel(k)] = v
	}
	l[nameLabel] = SanitizeMetricName(m.Namespace, m.MetricName)
	l[nsLabel] = m.Namespace
	l[metLabel] = m.MetricName
	return entry{meta: m, labels: l}
}

// catalog is a tenant's series list with the reverse lookup by __name__.
type catalog struct {
	at      time.Time
	all     []entry
	byName  map[string][]int
	partial string
}

func buildCatalog(metas []model.SeriesMeta, at time.Time) *catalog {
	c := &catalog{at: at, byName: map[string][]int{}}
	for _, m := range metas {
		e := newEntry(m)
		c.byName[e.labels[nameLabel]] = append(c.byName[e.labels[nameLabel]], len(c.all))
		c.all = append(c.all, e)
	}
	return c
}

// candidates narrows by an __name__ equality matcher when there is one.
func (c *catalog) candidates(ms []*Matcher) []entry {
	for _, m := range ms {
		if m.Name == nameLabel && m.Type == MatchEqual {
			idx := c.byName[m.Value]
			out := make([]entry, len(idx))
			for i, j := range idx {
				out[i] = c.all[j]
			}
			return out
		}
	}
	return c.all
}

// match returns the entries whose labels satisfy every non-stat matcher.
func (c *catalog) match(ms []*Matcher) []entry {
	var out []entry
	for _, e := range c.candidates(ms) {
		ok := true
		for _, m := range ms {
			if m.Name == statLabel {
				continue
			}
			if !m.Matches(e.labels[m.Name]) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, e)
		}
	}
	return out
}

// statsFor resolves the stat matchers of a selector.
func statsFor(ms []*Matcher) ([]string, error) {
	var sm []*Matcher
	for _, m := range ms {
		if m.Name == statLabel {
			sm = append(sm, m)
		}
	}
	if len(sm) == 0 {
		return []string{defStat}, nil
	}
	var cands []string
	for _, m := range sm {
		if m.Type == MatchEqual {
			if _, err := storage.ParseStat(m.Value); err != nil || m.Value == "" {
				return nil, badData("invalid stat %q: use Average, Sum, Minimum, Maximum, SampleCount or pNN", m.Value)
			}
			cands = []string{m.Value}
			break
		}
	}
	if cands == nil {
		cands = baseStats
	}
	var out []string
	for _, s := range cands {
		ok := true
		for _, m := range sm {
			if !m.Matches(s) {
				ok = false
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// catalogCache keeps each tenant's catalog for ttl.
type catalogCache struct {
	mu sync.Mutex
	m  map[uint32]*catalog
}

func (cc *catalogCache) get(ctx context.Context, st Store, tenant uint32, now time.Time, ttl time.Duration) (*catalog, error) {
	cc.mu.Lock()
	if c := cc.m[tenant]; c != nil && now.Sub(c.at) < ttl {
		cc.mu.Unlock()
		return c, nil
	}
	cc.mu.Unlock()
	metas, err := st.ListSeries(ctx, tenant, storage.SeriesFilter{})
	partial := ""
	if err != nil {
		if !storage.IsPartial(err) {
			return nil, err
		}
		partial = err.Error()
	}
	c := buildCatalog(metas, now)
	c.partial = partial
	cc.mu.Lock()
	if cc.m == nil {
		cc.m = map[uint32]*catalog{}
	}
	if partial == "" {
		cc.m[tenant] = c
	}
	cc.mu.Unlock()
	return c, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// labelKey is a canonical string for a label set.
func labelKey(m map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(k)
		b.WriteByte(0xfe)
		b.WriteString(m[k])
		b.WriteByte(0xff)
	}
	return b.String()
}
