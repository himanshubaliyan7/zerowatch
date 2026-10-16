package tier

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Policy is a retention policy. Zero fields inherit (tenant -> Default ->
// built-in defaults).
type Policy struct {
	// RawHot is how long raw samples stay in memory before they move to the
	// bucket (only with a bucket).
	RawHot time.Duration `json:"rawHot,omitempty"`
	// Raw, Rollup1m and Logs are the total ages after which data is deleted,
	// locally and in the bucket.
	Raw      time.Duration `json:"raw,omitempty"`
	Rollup1m time.Duration `json:"rollup1m,omitempty"`
	Logs     time.Duration `json:"logs,omitempty"`
}

// Built-in defaults.
const (
	DefaultRawHot   = 24 * time.Hour
	DefaultRaw      = 7 * 24 * time.Hour
	DefaultRollup1m = 90 * 24 * time.Hour
	DefaultLogs     = 7 * 24 * time.Hour
)

// Retention is the default policy plus per-tenant overrides.
type Retention struct {
	Default   Policy            `json:"default"`
	PerTenant map[uint32]Policy `json:"perTenant,omitempty"`
}

func inherit(p, from Policy) Policy {
	if p.RawHot <= 0 {
		p.RawHot = from.RawHot
	}
	if p.Raw <= 0 {
		p.Raw = from.Raw
	}
	if p.Rollup1m <= 0 {
		p.Rollup1m = from.Rollup1m
	}
	if p.Logs <= 0 {
		p.Logs = from.Logs
	}
	return p
}

// For returns the effective policy of a tenant.
func (r Retention) For(tenant uint32) Policy {
	p := r.PerTenant[tenant]
	p = inherit(p, r.Default)
	return inherit(p, Policy{RawHot: DefaultRawHot, Raw: DefaultRaw, Rollup1m: DefaultRollup1m, Logs: DefaultLogs})
}

// ParseDuration accepts Go durations plus d (days) and w (weeks), e.g. 90d.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	for suf, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if strings.HasSuffix(s, suf) {
			f, err := strconv.ParseFloat(strings.TrimSuffix(s, suf), 64)
			if err != nil || f <= 0 {
				return 0, fmt.Errorf("bad duration %q", s)
			}
			return time.Duration(f * float64(unit)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

// ParsePolicy parses "raw=7d,rawhot=24h,rollup=90d,logs=7d" (any subset;
// rollup1m is accepted for rollup).
func ParsePolicy(s string) (Policy, error) {
	var p Policy
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return p, fmt.Errorf("retention: %q is not key=duration", part)
		}
		d, err := ParseDuration(v)
		if err != nil {
			return p, fmt.Errorf("retention: %s: %w", k, err)
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "rawhot", "hot":
			p.RawHot = d
		case "raw":
			p.Raw = d
		case "rollup", "rollup1m", "1m":
			p.Rollup1m = d
		case "logs":
			p.Logs = d
		default:
			return p, fmt.Errorf("retention: unknown key %q (rawhot, raw, rollup, logs)", k)
		}
	}
	return p, nil
}

// ParseRetention parses --retention (a policy) and each --retention-tenant
// value ("<tenant>:<policy>", several may be joined with ';').
func ParseRetention(def string, tenants []string) (Retention, error) {
	r := Retention{PerTenant: map[uint32]Policy{}}
	var err error
	if r.Default, err = ParsePolicy(def); err != nil {
		return r, err
	}
	for _, v := range tenants {
		for _, item := range strings.Split(v, ";") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			ts, pol, ok := strings.Cut(item, ":")
			if !ok {
				return r, fmt.Errorf("retention-tenant: %q is not <tenant>:<policy>", item)
			}
			id, err := strconv.ParseUint(strings.TrimSpace(ts), 10, 32)
			if err != nil {
				return r, fmt.Errorf("retention-tenant: bad tenant %q", ts)
			}
			p, err := ParsePolicy(pol)
			if err != nil {
				return r, err
			}
			r.PerTenant[uint32(id)] = p
		}
	}
	return r, nil
}
