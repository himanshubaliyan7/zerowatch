package storage

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
	"github.com/zerowatch/zerowatch/internal/query"
)

//go:embed clickhouse_schema.sql
var clickhouseSchema string

// ClickHouse implements Engine over ClickHouse's HTTP interface (port 8123).
// Every user-controlled value is sent as a typed query parameter
// ({name:Type} + param_name=...), never interpolated into SQL.
//
// Query semantics mirror the memory engine (the integration tests in
// clickhouse_integration_test.go check the two side by side): exact
// dimension-set matching with "*" wildcards, rollup-tier statistics for
// minute-multiple periods, R-7 percentiles, substring log terms.
type ClickHouse struct {
	base   string
	db     string
	user   string
	pass   string
	client *http.Client
}

// NewClickHouse creates the engine. baseURL like http://localhost:8123;
// credentials may also be given as userinfo (http://user:pass@host:8123)
// when user is empty.
func NewClickHouse(baseURL, db, user, pass string) *ClickHouse {
	if u, err := url.Parse(baseURL); err == nil && u.User != nil {
		if user == "" {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		u.User = nil
		baseURL = u.String()
	}
	return &ClickHouse{base: strings.TrimRight(baseURL, "/"), db: db, user: user, pass: pass,
		client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *ClickHouse) do(ctx context.Context, sql string, params map[string]string, body io.Reader) (io.ReadCloser, error) {
	return c.doDB(ctx, c.db, sql, params, body)
}

func (c *ClickHouse) doDB(ctx context.Context, db, sql string, params map[string]string, body io.Reader) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("database", db)
	q.Set("output_format_json_quote_64bit_integers", "0")
	q.Set("date_time_input_format", "best_effort")
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	if body == nil {
		body = strings.NewReader(sql)
	} else {
		q.Set("query", sql)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.Header.Set("X-ClickHouse-User", c.user)
		req.Header.Set("X-ClickHouse-Key", c.pass)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("clickhouse: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

func (c *ClickHouse) exec(ctx context.Context, sql string, params map[string]string, body io.Reader) error {
	return c.execDB(ctx, c.db, sql, params, body)
}

func (c *ClickHouse) execDB(ctx context.Context, db, sql string, params map[string]string, body io.Reader) error {
	rc, err := c.doDB(ctx, db, sql, params, body)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, rc)
	return rc.Close()
}

// EnsureSchema creates the database, tables and materialized view. It is
// idempotent and upgrades older tables in place (ADD COLUMN IF NOT EXISTS).
func (c *ClickHouse) EnsureSchema(ctx context.Context) error {
	if err := c.execDB(ctx, "default", "CREATE DATABASE IF NOT EXISTS "+quoteIdent(c.db), nil, nil); err != nil {
		return err
	}
	for _, stmt := range strings.Split(stripComments(clickhouseSchema), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			if err := c.exec(ctx, s, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "") + "`" }

func chTime(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05.000") }

// WriteMetrics inserts rows with JSONEachRow. A StatisticSet becomes one row
// whose value is its average and whose stat_* columns keep the summary, the
// same split the memory engine makes between raw chunks and the 1m tier.
func (c *ClickHouse) WriteMetrics(ctx context.Context, ms []model.MetricDatum) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range ms {
		d := &ms[i]
		var v, n, sum, lo, hi float64
		switch {
		case d.StatisticValues != nil && d.StatisticValues.SampleCount > 0:
			sv := d.StatisticValues
			v, n, sum, lo, hi = sv.Sum/sv.SampleCount, sv.SampleCount, sv.Sum, sv.Minimum, sv.Maximum
		case d.StatisticValues == nil && d.Value != nil:
			v = *d.Value
			n, sum, lo, hi = 1, v, v, v
		default:
			continue
		}
		// Encode fails (and writes nothing) for NaN/Inf, which JSON cannot carry.
		_ = enc.Encode(map[string]any{
			"tenant_id": d.TenantID, "series_id": d.ID(), "timestamp": chTime(d.Timestamp), "value": v,
			"namespace": d.Namespace, "metric_name": d.MetricName, "dimensions": d.DimMap(), "unit": d.Unit,
			"stat_count": n, "stat_sum": sum, "stat_min": lo, "stat_max": hi,
		})
	}
	if buf.Len() == 0 {
		return nil
	}
	return c.exec(ctx, "INSERT INTO metrics_raw FORMAT JSONEachRow", nil, &buf)
}

// WriteLogs inserts log rows.
func (c *ClickHouse) WriteLogs(ctx context.Context, ls []model.LogEvent) error {
	if len(ls) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range ls {
		attrs := e.Attributes
		if attrs == nil {
			attrs = map[string]string{}
		}
		_ = enc.Encode(map[string]any{"tenant_id": e.TenantID, "log_group": e.LogGroup, "log_stream": e.LogStream,
			"timestamp": chTime(e.Timestamp), "message": e.Message, "level": e.Level, "attributes": attrs})
	}
	return c.exec(ctx, "INSERT INTO logs FORMAT JSONEachRow", nil, &buf)
}

// statSQL maps a stat to a ClickHouse aggregate. rollup selects the stat_*
// summary columns (the memory engine's 1m tier); otherwise raw values.
// Percentiles use quantileExactInclusive, the R-7 definition Stat.Reduce
// implements (linear interpolation on the (n-1) rank scale).
func statSQL(st Stat, rollup bool) string {
	if rollup {
		switch st.Kind {
		case "sum":
			return "sum(stat_sum)"
		case "min":
			return "min(stat_min)"
		case "max":
			return "max(stat_max)"
		case "count":
			return "sum(stat_count)"
		}
		return "sum(stat_sum) / sum(stat_count)"
	}
	switch st.Kind {
	case "sum":
		return "sum(value)"
	case "min":
		return "min(value)"
	case "max":
		return "max(value)"
	case "count":
		return "toFloat64(count())"
	case "pct":
		return "quantileExactInclusive(" + strconv.FormatFloat(st.Pct/100, 'g', -1, 64) + ")(value)"
	}
	return "avg(value)"
}

// BuildFetchSQL is exported for tests: SQL plus parameters for FetchStat.
func BuildFetchSQL(tenant uint32, ms query.MetricStat, startMs, endMs int64, periodSec int) (string, map[string]string, error) {
	st, err := ParseStat(ms.Stat)
	if err != nil {
		return "", nil, err
	}
	if periodSec <= 0 {
		return "", nil, fmt.Errorf("storage: period must be positive, got %d", periodSec)
	}
	pMs := int64(periodSec) * 1000
	// Memory serves rollup-able stats on minute-multiple periods from its 1m
	// tier, which selects whole minutes: those starting at or after the
	// minute containing start and before end.
	rollup := st.Rollupable() && pMs%minuteMs == 0
	if rollup {
		startMs = query.AlignTimestamp(startMs, minuteMs)
		endMs = query.AlignTimestamp(endMs+minuteMs-1, minuteMs)
	}
	p := map[string]string{
		"tenant": strconv.FormatUint(uint64(tenant), 10), "ns": ms.Namespace, "metric": ms.MetricName,
		"start": strconv.FormatInt(startMs, 10), "end": strconv.FormatInt(endMs, 10),
		"period": strconv.FormatInt(pMs, 10), "ndims": strconv.Itoa(len(ms.Dimensions)),
	}
	where := dimsWhere(ms.Dimensions, p, true)
	sql := "SELECT intDiv(toUnixTimestamp64Milli(timestamp), {period:Int64}) * {period:Int64} AS t, " + statSQL(st, rollup) + " AS v" +
		" FROM metrics_raw WHERE tenant_id = {tenant:UInt32} AND namespace = {ns:String} AND metric_name = {metric:String}" +
		" AND timestamp >= fromUnixTimestamp64Milli({start:Int64}) AND timestamp < fromUnixTimestamp64Milli({end:Int64})" +
		" AND length(dimensions) = {ndims:UInt32}" + where +
		" GROUP BY t ORDER BY t FORMAT JSONEachRow"
	return sql, p, nil
}

// dimsWhere renders dimension conditions, adding dk<i>/dv<i> parameters.
// Every named key must be present (matchDims / subsetDims); "*" matches any
// value, and so does "" when loose (subsetDims, for discovery).
func dimsWhere(dims map[string]string, p map[string]string, exact bool) string {
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var w strings.Builder
	for i, k := range keys {
		p["dk"+strconv.Itoa(i)] = k
		fmt.Fprintf(&w, " AND mapContains(dimensions, {dk%d:String})", i)
		if v := dims[k]; v != "*" && (exact || v != "") {
			p["dv"+strconv.Itoa(i)] = v
			fmt.Fprintf(&w, " AND dimensions[{dk%d:String}] = {dv%d:String}", i, i)
		}
	}
	return w.String()
}

// FetchStat runs the aggregation server-side.
func (c *ClickHouse) FetchStat(ctx context.Context, tenant uint32, ms query.MetricStat, startMs, endMs int64, periodSec int) ([]model.Point, error) {
	sql, p, err := BuildFetchSQL(tenant, ms, startMs, endMs, periodSec)
	if err != nil {
		return nil, err
	}
	rc, err := c.do(ctx, sql, p, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var out []model.Point
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var row struct {
			T int64    `json:"t"`
			V *float64 `json:"v"` // null for NaN
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("clickhouse: decode row: %w", err)
		}
		v := math.NaN()
		if row.V != nil {
			v = *row.V
		}
		out = append(out, model.Point{T: row.T, V: v})
	}
	return out, sc.Err()
}

// ListSeries returns distinct series (seen in the last 24h) matching the
// filter, ordered like the memory engine (namespace, metric, series ID).
func (c *ClickHouse) ListSeries(ctx context.Context, tenant uint32, f SeriesFilter) ([]model.SeriesMeta, error) {
	limit := f.Limit
	if limit <= 0 || limit > 100_000 {
		limit = 100_000
	}
	p := map[string]string{"tenant": strconv.FormatUint(uint64(tenant), 10), "ns": f.Namespace, "metric": f.MetricName, "limit": strconv.Itoa(limit)}
	sql := "SELECT series_id, any(namespace) AS ns, any(metric_name) AS mn, any(dimensions) AS dims, any(unit) AS u FROM metrics_raw" +
		" WHERE tenant_id = {tenant:UInt32} AND timestamp > now64(3) - INTERVAL 1 DAY" +
		" AND ({ns:String} = '' OR namespace = {ns:String}) AND ({metric:String} = '' OR metric_name = {metric:String})" +
		dimsWhere(f.Dimensions, p, false) +
		" GROUP BY series_id ORDER BY ns, mn, series_id LIMIT {limit:UInt32} FORMAT JSONEachRow"
	rc, err := c.do(ctx, sql, p, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	out := []model.SeriesMeta{}
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var row struct {
			ID         uint64            `json:"series_id"`
			Namespace  string            `json:"ns"`
			MetricName string            `json:"mn"`
			Dimensions map[string]string `json:"dims"`
			Unit       string            `json:"u"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, err
		}
		out = append(out, model.SeriesMeta{ID: row.ID, TenantID: tenant, Namespace: row.Namespace, MetricName: row.MetricName, Dimensions: row.Dimensions, Unit: row.Unit})
	}
	return out, sc.Err()
}

// ListLogGroups summarises the last 24h of logs.
func (c *ClickHouse) ListLogGroups(ctx context.Context, tenant uint32) ([]LogGroupInfo, error) {
	sql := "SELECT log_group, arraySort(groupUniqArray(1000)(log_stream)) AS streams, toUInt64(count()) AS events, toUnixTimestamp64Milli(max(timestamp)) AS last" +
		" FROM logs WHERE tenant_id = {tenant:UInt32} AND timestamp > now64(3) - INTERVAL 1 DAY GROUP BY log_group ORDER BY log_group FORMAT JSONEachRow"
	rc, err := c.do(ctx, sql, map[string]string{"tenant": strconv.FormatUint(uint64(tenant), 10)}, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	out := []LogGroupInfo{}
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var row struct {
			G string   `json:"log_group"`
			S []string `json:"streams"`
			E int      `json:"events"`
			L int64    `json:"last"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, err
		}
		out = append(out, LogGroupInfo{Name: row.G, Streams: row.S, Events: row.E, LastEvent: row.L})
	}
	return out, sc.Err()
}

// SearchLogs translates MatchLog into SQL. Message terms are case-insensitive
// substrings like the memory engine (positionCaseInsensitiveUTF8; a
// hasToken match would miss "time" in "timeout"); key=value terms compare
// attributes, falling back to the event level for key "level".
func (c *ClickHouse) SearchLogs(ctx context.Context, q LogQuery) (LogResult, error) {
	limit := q.Limit
	if limit <= 0 || limit > 10000 {
		limit = 500
	}
	p := map[string]string{"tenant": strconv.FormatUint(uint64(q.TenantID), 10), "group": q.LogGroup, "stream": q.LogStream,
		"level": q.Level, "start": strconv.FormatInt(q.StartMs, 10), "end": strconv.FormatInt(q.EndMs, 10), "limit": strconv.Itoa(limit + 1)}
	var w strings.Builder
	for i, t := range Terms(q.Query) {
		neg := strings.HasPrefix(t, "-") && len(t) > 1
		if neg {
			t = t[1:]
		}
		name := "t" + strconv.Itoa(i)
		var cond string
		if k, v, ok := strings.Cut(t, "="); ok && k != "" {
			p[name+"k"], p[name] = k, v
			cond = fmt.Sprintf("if(mapContains(attributes, {%[1]sk:String}), lowerUTF8(attributes[{%[1]sk:String}]) = {%[1]s:String},"+
				" {%[1]sk:String} = 'level' AND lowerUTF8(level) = {%[1]s:String})", name)
		} else {
			p[name] = t
			cond = fmt.Sprintf("positionCaseInsensitiveUTF8(message, {%s:String}) > 0", name)
		}
		if neg {
			cond = "NOT " + cond
		}
		w.WriteString(" AND " + cond)
	}
	sql := "SELECT tenant_id, log_group, log_stream, toUnixTimestamp64Milli(timestamp) AS ts, message, level, attributes FROM logs" +
		" WHERE ({tenant:UInt32} = 0 OR tenant_id = {tenant:UInt32}) AND ({group:String} = '' OR log_group = {group:String})" +
		" AND ({stream:String} = '' OR log_stream = {stream:String}) AND ({level:String} = '' OR lowerUTF8(level) = lowerUTF8({level:String}))" +
		" AND ({start:Int64} = 0 OR timestamp >= fromUnixTimestamp64Milli({start:Int64}))" +
		" AND ({end:Int64} = 0 OR timestamp < fromUnixTimestamp64Milli({end:Int64}))" + w.String() +
		" ORDER BY timestamp DESC LIMIT {limit:UInt32} FORMAT JSONEachRow"
	rc, err := c.do(ctx, sql, p, nil)
	if err != nil {
		return LogResult{}, err
	}
	defer rc.Close()
	res := LogResult{Events: []model.LogEvent{}}
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var row struct {
			Tenant uint32            `json:"tenant_id"`
			G      string            `json:"log_group"`
			S      string            `json:"log_stream"`
			TS     int64             `json:"ts"`
			M      string            `json:"message"`
			L      string            `json:"level"`
			A      map[string]string `json:"attributes"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return res, err
		}
		if len(res.Events) == limit {
			res.Truncated = true
			break
		}
		if len(row.A) == 0 {
			row.A = nil
		}
		res.Events = append(res.Events, model.LogEvent{TenantID: row.Tenant, LogGroup: row.G, LogStream: row.S, Timestamp: row.TS, Message: row.M, Level: row.L, Attributes: row.A})
	}
	res.Scanned = len(res.Events)
	return res, sc.Err()
}

// Stats reads cheap totals from system.parts (plus a uniq over series IDs).
// It is best-effort: on error only Backend is set.
func (c *ClickHouse) Stats() Stats {
	st := Stats{Backend: "clickhouse"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sql := "SELECT (SELECT uniq(series_id) FROM metrics_raw) AS series," +
		" (SELECT sum(rows) FROM system.parts WHERE active AND database = currentDatabase() AND table = 'metrics_raw') AS samples," +
		" (SELECT sum(data_compressed_bytes) FROM system.parts WHERE active AND database = currentDatabase() AND table = 'metrics_raw') AS bytes," +
		" (SELECT sum(rows) FROM system.parts WHERE active AND database = currentDatabase() AND table = 'logs') AS logs" +
		" FORMAT JSONEachRow"
	rc, err := c.do(ctx, sql, nil, nil)
	if err != nil {
		return st
	}
	defer rc.Close()
	var row struct {
		Series  int   `json:"series"`
		Samples int64 `json:"samples"`
		Bytes   int64 `json:"bytes"`
		Logs    int   `json:"logs"`
	}
	if err := json.NewDecoder(rc).Decode(&row); err != nil {
		return st
	}
	st.Series, st.Samples, st.ChunkBytes, st.LogEvents = row.Series, row.Samples, row.Bytes, row.Logs
	if row.Samples > 0 {
		st.BytesPerPt = math.Round(float64(row.Bytes)/float64(row.Samples)*100) / 100
	}
	return st
}

// Close releases idle connections.
func (c *ClickHouse) Close() error { c.client.CloseIdleConnections(); return nil }

// Ping checks connectivity and credentials; it works before EnsureSchema has
// created the database.
func (c *ClickHouse) Ping(ctx context.Context) error {
	return c.execDB(ctx, "default", "SELECT 1", nil, nil)
}
