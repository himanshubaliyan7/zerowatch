package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
)

func open(t *testing.T) *SQLStore {
	t.Helper()
	s, err := Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDashboardsCRUDAndCAS(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	d := &Dashboard{TenantID: 1, Title: "RED", Layout: json.RawMessage(`{"range":"1h"}`),
		Widgets: []Widget{{Type: "timeseries", Title: "p99", X: 0, Y: 0, W: 6, H: 4, Query: json.RawMessage(`{"expr":"m1"}`)}}}
	if err := s.SaveDashboard(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDashboard(ctx, 1, d.ID)
	if err != nil || len(got.Widgets) != 1 || got.Version != 1 || string(got.Widgets[0].Query) != `{"expr":"m1"}` {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetDashboard(ctx, 2, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("tenant isolation")
	}
	got.Title = "RED v2"
	got.Widgets = append(got.Widgets, Widget{Type: "logs", X: 6, Y: 0, W: 6, H: 4})
	if err := s.SaveDashboard(ctx, got); err != nil || got.Version != 2 {
		t.Fatalf("update %v v=%d", err, got.Version)
	}
	stale := *d // version 1
	if err := s.SaveDashboard(ctx, &stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict got %v", err)
	}
	list, _ := s.ListDashboards(ctx, 1)
	if len(list) != 1 || list[0].Widgets != 2 {
		t.Fatalf("%+v", list)
	}
	bad := &Dashboard{TenantID: 1, Title: "x", Widgets: []Widget{{Type: "timeseries", X: 10, W: 6, H: 1}}}
	if err := s.SaveDashboard(ctx, bad); err == nil {
		t.Fatal("grid validation")
	}
	if err := s.DeleteDashboard(ctx, 1, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDashboard(ctx, 1, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete")
	}
}

// A tenant that learns another tenant's rule id must not be able to overwrite
// the rule through PUT /api/v1/alarms/{id}, which takes the id from the URL.
func TestSaveAlarmRuleRefusesAnotherTenantsID(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	rule := func(tenant uint32, id, name string) *alarms.Rule {
		return &alarms.Rule{ID: id, TenantID: tenant, Name: name, Enabled: true, Metric: &query.MetricStat{Namespace: "n", MetricName: "cpu"},
			Config: alarms.Config{Threshold: 80, Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 3}}
	}
	if err := s.SaveAlarmRule(ctx, rule(1, "shared-id", "owner's rule")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAlarmRule(ctx, rule(2, "shared-id", "attacker's rule")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	got, err := s.GetAlarmRule(ctx, 1, "shared-id")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "owner's rule" || got.TenantID != 1 {
		t.Fatalf("rule overwritten: %+v", got)
	}
	if err := s.SaveAlarmRule(ctx, rule(1, "shared-id", "owner's edit")); err != nil {
		t.Fatalf("the owner must still be able to update: %v", err)
	}
	if got, _ = s.GetAlarmRule(ctx, 1, "shared-id"); got.Name != "owner's edit" {
		t.Fatalf("update lost: %+v", got)
	}
}

func TestAlarmRulesAndHistory(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	r := &alarms.Rule{TenantID: 1, Name: "cpu", Enabled: true, Metric: &query.MetricStat{Namespace: "n", MetricName: "cpu"},
		Config: alarms.Config{Threshold: 80, Comparison: alarms.GreaterThanThreshold, EvaluationPeriods: 3}}
	if err := s.SaveAlarmRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListAlarmRules(ctx, 0)
	if len(all) != 1 || all[0].State != alarms.StateInsufficientData || all[0].Config.DatapointsToAlarm != 3 {
		t.Fatalf("%+v", all)
	}
	v := 91.0
	tr := alarms.Transition{RuleID: r.ID, RuleName: "cpu", TenantID: 1, From: alarms.StateOK, To: alarms.StateAlarm, Reason: "3/3", Value: &v, Timestamp: time.Now()}
	if err := s.RecordAlarmTransition(ctx, tr); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAlarmRule(ctx, 1, r.ID)
	if got.State != alarms.StateAlarm {
		t.Fatal("state not persisted")
	}
	h, _ := s.ListAlarmHistory(ctx, 1, "", 10)
	if len(h) != 1 || *h[0].Value != 91 {
		t.Fatalf("%+v", h)
	}
	if err := s.DeleteAlarmRule(ctx, 1, r.ID); err != nil {
		t.Fatal(err)
	}
	sq := &SavedQuery{TenantID: 1, Name: "errors", Query: "level=error"}
	if err := s.SaveQuery(ctx, sq); err != nil {
		t.Fatal(err)
	}
	qs, _ := s.ListSavedQueries(ctx, 1)
	if len(qs) != 1 {
		t.Fatal("saved queries")
	}
	if v, _ := s.SchemaVersion(ctx); v != len(migrations) {
		t.Fatal("schema version")
	}
}

func TestRebind(t *testing.T) {
	s := &SQLStore{postgres: true}
	if got := s.rebind("a = ? AND b = ?"); got != "a = $1 AND b = $2" {
		t.Fatal(got)
	}
}
