// Package metadata persists dashboards, widgets, alarm rules, alarm history
// and saved log queries. One SQL implementation serves both SQLite (WAL, the
// embedded default: data/metadata.db) and PostgreSQL (clustered tier); dialect
// differences are limited to placeholder syntax and the driver.
package metadata

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver "pgx"
	_ "modernc.org/sqlite"             // pure-Go sqlite driver "sqlite"

	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
)

// Errors.
var (
	ErrNotFound = errors.New("metadata: not found")
	ErrConflict = errors.New("metadata: version conflict")
)

// Widget is one panel on a dashboard (Gridstack coordinates, 12 columns).
type Widget struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"` // timeseries|stat|logs|alarm|text|anomaly|forecast
	Title  string          `json:"title"`
	Query  json.RawMessage `json:"query,omitempty"`
	Visual json.RawMessage `json:"visual,omitempty"`
	X      int             `json:"x"`
	Y      int             `json:"y"`
	W      int             `json:"w"`
	H      int             `json:"h"`
}

// Dashboard is a saved dashboard.
type Dashboard struct {
	ID          string          `json:"id"`
	TenantID    uint32          `json:"tenantId"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Layout      json.RawMessage `json:"layout,omitempty"` // global layout config (time range, refresh, variables)
	Widgets     []Widget        `json:"widgets"`
	Version     int             `json:"version"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

// Validate checks user input.
func (d *Dashboard) Validate() error {
	if strings.TrimSpace(d.Title) == "" || len(d.Title) > 200 {
		return errors.New("title is required (max 200 chars)")
	}
	if len(d.Description) > 2000 {
		return errors.New("description max 2000 chars")
	}
	if len(d.Widgets) > 100 {
		return errors.New("max 100 widgets per dashboard")
	}
	allowed := map[string]bool{"timeseries": true, "stat": true, "logs": true, "alarm": true, "text": true, "anomaly": true, "forecast": true}
	for i := range d.Widgets {
		w := &d.Widgets[i]
		if !allowed[w.Type] {
			return fmt.Errorf("widget %d: unknown type %q", i, w.Type)
		}
		if w.W <= 0 || w.H <= 0 || w.X < 0 || w.Y < 0 || w.X+w.W > 12 {
			return fmt.Errorf("widget %d: invalid grid position", i)
		}
		if len(w.Title) > 200 {
			return fmt.Errorf("widget %d: title too long", i)
		}
	}
	return nil
}

// DashboardSummary is the list view.
type DashboardSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Widgets   int       `json:"widgets"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SavedQuery is a saved Log Explorer query.
type SavedQuery struct {
	ID        string    `json:"id"`
	TenantID  uint32    `json:"tenantId"`
	Name      string    `json:"name"`
	Query     string    `json:"query"`
	LogGroup  string    `json:"logGroup,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Store is the MetadataStore abstraction (mirrors the blueprint trait and
// satisfies alarms.RuleSource / alarms.TransitionSink).
type Store interface {
	GetDashboard(ctx context.Context, tenant uint32, id string) (*Dashboard, error)
	SaveDashboard(ctx context.Context, d *Dashboard) error
	ListDashboards(ctx context.Context, tenant uint32) ([]DashboardSummary, error)
	DeleteDashboard(ctx context.Context, tenant uint32, id string) error

	ListAlarmRules(ctx context.Context, tenant uint32) ([]alarms.Rule, error)
	GetAlarmRule(ctx context.Context, tenant uint32, id string) (*alarms.Rule, error)
	SaveAlarmRule(ctx context.Context, r *alarms.Rule) error
	DeleteAlarmRule(ctx context.Context, tenant uint32, id string) error
	RecordAlarmTransition(ctx context.Context, tr alarms.Transition) error
	ListAlarmHistory(ctx context.Context, tenant uint32, ruleID string, limit int) ([]alarms.Transition, error)

	SaveQuery(ctx context.Context, q *SavedQuery) error
	ListSavedQueries(ctx context.Context, tenant uint32) ([]SavedQuery, error)
	DeleteSavedQuery(ctx context.Context, tenant uint32, id string) error

	Ping(ctx context.Context) error
	Close() error
}

// SQLStore implements Store over database/sql.
type SQLStore struct {
	db       *sql.DB
	postgres bool
}

// Open parses a DSN ("sqlite:<path>" or "postgres://...") and migrates.
func Open(ctx context.Context, dsn string) (*SQLStore, error) {
	var s *SQLStore
	switch {
	case strings.HasPrefix(dsn, "sqlite:"):
		path := strings.TrimPrefix(dsn, "sqlite:")
		if path != ":memory:" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, fmt.Errorf("metadata: mkdir: %w", err)
			}
		}
		conn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
		if path == ":memory:" {
			conn = "file::memory:?_pragma=foreign_keys(1)"
		}
		db, err := sql.Open("sqlite", conn)
		if err != nil {
			return nil, err
		}
		// SQLite allows one writer; a single connection avoids SQLITE_BUSY storms
		// and is plenty for metadata traffic.
		db.SetMaxOpenConns(1)
		s = &SQLStore{db: db}
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(10)
		db.SetConnMaxIdleTime(5 * time.Minute)
		s = &SQLStore{db: db, postgres: true}
	default:
		return nil, fmt.Errorf("metadata: unsupported dsn %q", dsn)
	}
	if err := s.db.PingContext(ctx); err != nil {
		s.db.Close()
		return nil, fmt.Errorf("metadata: ping: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		s.db.Close()
		return nil, err
	}
	return s, nil
}

// rebind converts ? placeholders to $n for Postgres.
func (s *SQLStore) rebind(q string) string {
	if !s.postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *SQLStore) exec(ctx context.Context, tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	if tx != nil {
		return tx.ExecContext(ctx, s.rebind(q), args...)
	}
	return s.db.ExecContext(ctx, s.rebind(q), args...)
}

func newID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }
func rawOrNull(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// ---- dashboards

// GetDashboard loads a dashboard with widgets.
func (s *SQLStore) GetDashboard(ctx context.Context, tenant uint32, id string) (*Dashboard, error) {
	d := &Dashboard{Widgets: []Widget{}}
	var layout sql.NullString
	var upd int64
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT id, tenant_id, title, description, layout_config, version, updated_at FROM dashboards WHERE tenant_id = ? AND id = ?`), tenant, id).
		Scan(&d.ID, &d.TenantID, &d.Title, &d.Description, &layout, &d.Version, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if layout.Valid {
		d.Layout = json.RawMessage(layout.String)
	}
	d.UpdatedAt = fromMs(upd)
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, type, title, query_definition, visual_options, x, y, w, h FROM widgets WHERE dashboard_id = ? ORDER BY y, x`), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w Widget
		var q, v sql.NullString
		if err := rows.Scan(&w.ID, &w.Type, &w.Title, &q, &v, &w.X, &w.Y, &w.W, &w.H); err != nil {
			return nil, err
		}
		if q.Valid {
			w.Query = json.RawMessage(q.String)
		}
		if v.Valid {
			w.Visual = json.RawMessage(v.String)
		}
		d.Widgets = append(d.Widgets, w)
	}
	return d, rows.Err()
}

// SaveDashboard creates (empty ID) or updates with optimistic concurrency:
// the caller's Version must equal the stored version (compare-and-swap).
func (s *SQLStore) SaveDashboard(ctx context.Context, d *Dashboard) error {
	if err := d.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	now := time.Now().UTC()
	if d.ID == "" {
		d.ID, d.Version = newID(), 1
		if _, err := s.exec(ctx, tx, `INSERT INTO dashboards (id, tenant_id, title, description, layout_config, version, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.TenantID, d.Title, d.Description, rawOrNull(d.Layout), d.Version, ms(now)); err != nil {
			return err
		}
	} else {
		res, err := s.exec(ctx, tx, `UPDATE dashboards SET title = ?, description = ?, layout_config = ?, version = version + 1, updated_at = ? WHERE tenant_id = ? AND id = ? AND version = ?`,
			d.Title, d.Description, rawOrNull(d.Layout), ms(now), d.TenantID, d.ID, d.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM dashboards WHERE tenant_id = ? AND id = ?`), d.TenantID, d.ID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrConflict
		}
		d.Version++
		if _, err := s.exec(ctx, tx, `DELETE FROM widgets WHERE dashboard_id = ?`, d.ID); err != nil {
			return err
		}
	}
	for i := range d.Widgets {
		w := &d.Widgets[i]
		if w.ID == "" {
			w.ID = newID()
		}
		if _, err := s.exec(ctx, tx, `INSERT INTO widgets (id, dashboard_id, type, title, query_definition, visual_options, x, y, w, h) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.ID, d.ID, w.Type, w.Title, rawOrNull(w.Query), rawOrNull(w.Visual), w.X, w.Y, w.W, w.H); err != nil {
			return err
		}
	}
	d.UpdatedAt = now
	return tx.Commit()
}

// ListDashboards returns summaries for a tenant.
func (s *SQLStore) ListDashboards(ctx context.Context, tenant uint32) ([]DashboardSummary, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT d.id, d.title, d.updated_at, (SELECT COUNT(*) FROM widgets w WHERE w.dashboard_id = d.id) FROM dashboards d WHERE d.tenant_id = ? ORDER BY d.updated_at DESC`), tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardSummary{}
	for rows.Next() {
		var d DashboardSummary
		var upd int64
		if err := rows.Scan(&d.ID, &d.Title, &upd, &d.Widgets); err != nil {
			return nil, err
		}
		d.UpdatedAt = fromMs(upd)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDashboard removes a dashboard (widgets cascade).
func (s *SQLStore) DeleteDashboard(ctx context.Context, tenant uint32, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := s.exec(ctx, tx, `DELETE FROM dashboards WHERE tenant_id = ? AND id = ?`, tenant, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := s.exec(ctx, tx, `DELETE FROM widgets WHERE dashboard_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- alarm rules

func scanRule(def string, state string) (alarms.Rule, error) {
	var r alarms.Rule
	if err := json.Unmarshal([]byte(def), &r); err != nil {
		return r, err
	}
	r.State = alarms.State(state)
	return r, nil
}

// ListAlarmRules lists rules; tenant 0 lists every tenant (alarm engine).
func (s *SQLStore) ListAlarmRules(ctx context.Context, tenant uint32) ([]alarms.Rule, error) {
	q := `SELECT definition, state FROM alert_rules`
	args := []any{}
	if tenant != 0 {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenant)
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(q+` ORDER BY name`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []alarms.Rule{}
	for rows.Next() {
		var def, state string
		if err := rows.Scan(&def, &state); err != nil {
			return nil, err
		}
		r, err := scanRule(def, state)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAlarmRule loads one rule.
func (s *SQLStore) GetAlarmRule(ctx context.Context, tenant uint32, id string) (*alarms.Rule, error) {
	var def, state string
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT definition, state FROM alert_rules WHERE tenant_id = ? AND id = ?`), tenant, id).Scan(&def, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r, err := scanRule(def, state)
	return &r, err
}

// SaveAlarmRule upserts a rule. UpdatedAt changes force the engine to rebuild
// the rule's state machine.
func (s *SQLStore) SaveAlarmRule(ctx context.Context, r *alarms.Rule) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.ID == "" {
		r.ID = newID()
	}
	if r.State == "" {
		r.State = alarms.StateInsufficientData
	}
	r.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	def, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, nil, `INSERT INTO alert_rules (id, tenant_id, name, expression, evaluation_period, datapoints_to_alarm, state, definition, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET name = excluded.name, expression = excluded.expression, evaluation_period = excluded.evaluation_period,
 datapoints_to_alarm = excluded.datapoints_to_alarm, definition = excluded.definition, updated_at = excluded.updated_at`,
		r.ID, r.TenantID, r.Name, r.Expression, r.Config.EvaluationPeriods, r.Config.DatapointsToAlarm, string(r.State), string(def), ms(r.UpdatedAt))
	return err
}

// DeleteAlarmRule removes a rule (history is retained).
func (s *SQLStore) DeleteAlarmRule(ctx context.Context, tenant uint32, id string) error {
	res, err := s.exec(ctx, nil, `DELETE FROM alert_rules WHERE tenant_id = ? AND id = ?`, tenant, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordAlarmTransition stores history and the rule's new state atomically.
func (s *SQLStore) RecordAlarmTransition(ctx context.Context, tr alarms.Transition) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var val any
	if tr.Value != nil {
		val = *tr.Value
	}
	if _, err := s.exec(ctx, tx, `INSERT INTO alert_history (id, rule_id, tenant_id, rule_name, from_state, to_state, reason, value, ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(), tr.RuleID, tr.TenantID, tr.RuleName, string(tr.From), string(tr.To), tr.Reason, val, ms(tr.Timestamp)); err != nil {
		return err
	}
	if _, err := s.exec(ctx, tx, `UPDATE alert_rules SET state = ? WHERE id = ?`, string(tr.To), tr.RuleID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAlarmHistory returns newest-first transitions (ruleID optional).
func (s *SQLStore) ListAlarmHistory(ctx context.Context, tenant uint32, ruleID string, limit int) ([]alarms.Transition, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT rule_id, tenant_id, rule_name, from_state, to_state, reason, value, ts FROM alert_history WHERE tenant_id = ?`
	args := []any{tenant}
	if ruleID != "" {
		q += ` AND rule_id = ?`
		args = append(args, ruleID)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []alarms.Transition{}
	for rows.Next() {
		var tr alarms.Transition
		var from, to string
		var val sql.NullFloat64
		var ts int64
		if err := rows.Scan(&tr.RuleID, &tr.TenantID, &tr.RuleName, &from, &to, &tr.Reason, &val, &ts); err != nil {
			return nil, err
		}
		tr.From, tr.To, tr.Timestamp = alarms.State(from), alarms.State(to), fromMs(ts)
		if val.Valid {
			v := val.Float64
			tr.Value = &v
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// ---- saved queries

// SaveQuery inserts a saved log query.
func (s *SQLStore) SaveQuery(ctx context.Context, q *SavedQuery) error {
	if strings.TrimSpace(q.Name) == "" || len(q.Name) > 200 || len(q.Query) > 4000 {
		return errors.New("name required (max 200), query max 4000 chars")
	}
	q.ID, q.CreatedAt = newID(), time.Now().UTC()
	_, err := s.exec(ctx, nil, `INSERT INTO saved_queries (id, tenant_id, name, query, log_group, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		q.ID, q.TenantID, q.Name, q.Query, q.LogGroup, ms(q.CreatedAt))
	return err
}

// ListSavedQueries lists saved queries for a tenant.
func (s *SQLStore) ListSavedQueries(ctx context.Context, tenant uint32) ([]SavedQuery, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, tenant_id, name, query, log_group, created_at FROM saved_queries WHERE tenant_id = ? ORDER BY created_at DESC`), tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedQuery{}
	for rows.Next() {
		var q SavedQuery
		var c int64
		if err := rows.Scan(&q.ID, &q.TenantID, &q.Name, &q.Query, &q.LogGroup, &c); err != nil {
			return nil, err
		}
		q.CreatedAt = fromMs(c)
		out = append(out, q)
	}
	return out, rows.Err()
}

// DeleteSavedQuery removes one saved query (ErrNotFound when absent).
func (s *SQLStore) DeleteSavedQuery(ctx context.Context, tenant uint32, id string) error {
	res, err := s.exec(ctx, nil, `DELETE FROM saved_queries WHERE tenant_id = ? AND id = ?`, tenant, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Ping checks connectivity.
func (s *SQLStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close closes the pool.
func (s *SQLStore) Close() error { return s.db.Close() }
