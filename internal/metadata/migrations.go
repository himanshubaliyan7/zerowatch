package metadata

import (
	"context"
	"fmt"
	"time"
)

// migrations are append-only and portable across SQLite and PostgreSQL
// (TEXT ids, BIGINT unix-ms timestamps, JSON stored as TEXT). Never edit an
// applied migration; add a new one. Only additive, backward-compatible DDL.
var migrations = []string{
	// 1: core schema (ER diagram: DASHBOARD, WIDGET, ALERT_RULE, ALERT_HISTORY, LOG_SAVED_QUERY)
	`CREATE TABLE IF NOT EXISTS dashboards (
		id TEXT PRIMARY KEY,
		tenant_id BIGINT NOT NULL,
		title TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		layout_config TEXT,
		version INTEGER NOT NULL DEFAULT 1,
		updated_at BIGINT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_dashboards_tenant ON dashboards (tenant_id, updated_at);
	CREATE TABLE IF NOT EXISTS widgets (
		id TEXT PRIMARY KEY,
		dashboard_id TEXT NOT NULL REFERENCES dashboards (id) ON DELETE CASCADE,
		type TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		query_definition TEXT,
		visual_options TEXT,
		x INTEGER NOT NULL DEFAULT 0,
		y INTEGER NOT NULL DEFAULT 0,
		w INTEGER NOT NULL DEFAULT 6,
		h INTEGER NOT NULL DEFAULT 4
	);
	CREATE INDEX IF NOT EXISTS idx_widgets_dashboard ON widgets (dashboard_id);
	CREATE TABLE IF NOT EXISTS alert_rules (
		id TEXT PRIMARY KEY,
		tenant_id BIGINT NOT NULL,
		name TEXT NOT NULL,
		expression TEXT NOT NULL DEFAULT '',
		evaluation_period INTEGER NOT NULL,
		datapoints_to_alarm INTEGER NOT NULL,
		state TEXT NOT NULL DEFAULT 'INSUFFICIENT_DATA',
		definition TEXT NOT NULL,
		updated_at BIGINT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_alert_rules_tenant ON alert_rules (tenant_id);
	CREATE TABLE IF NOT EXISTS alert_history (
		id TEXT PRIMARY KEY,
		rule_id TEXT NOT NULL,
		tenant_id BIGINT NOT NULL,
		rule_name TEXT NOT NULL DEFAULT '',
		from_state TEXT NOT NULL,
		to_state TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		value DOUBLE PRECISION,
		ts BIGINT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_alert_history_tenant_ts ON alert_history (tenant_id, ts);
	CREATE INDEX IF NOT EXISTS idx_alert_history_rule_ts ON alert_history (rule_id, ts);
	CREATE TABLE IF NOT EXISTS saved_queries (
		id TEXT PRIMARY KEY,
		tenant_id BIGINT NOT NULL,
		name TEXT NOT NULL,
		query TEXT NOT NULL,
		log_group TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL
	);`,
}

func (s *SQLStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("metadata: migrations table: %w", err)
	}
	var cur int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&cur); err != nil {
		return err
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("metadata: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`), i+1, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the applied migration version.
func (s *SQLStore) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
