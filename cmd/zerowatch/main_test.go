package main

import (
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
	"github.com/himanshubaliyan7/zerowatch/internal/live"
)

// The hub delivers an event only to subscribers of its TenantID, so a
// transition must carry the rule's tenant.
func TestAlarmEventKeepsRuleTenant(t *testing.T) {
	hub := live.NewHub(4, 0)
	mine, ok := hub.Subscribe(7, nil)
	if !ok {
		t.Fatal("subscribe")
	}
	other, ok := hub.Subscribe(1, nil)
	if !ok {
		t.Fatal("subscribe")
	}
	hub.Publish(alarmEvent(alarms.Transition{RuleID: "r", TenantID: 7}, time.Unix(1, 0)))
	select {
	case e := <-mine.C:
		if e.Kind != live.KindAlarm {
			t.Fatalf("kind %q", e.Kind)
		}
	default:
		t.Fatal("tenant 7 did not get its alarm")
	}
	select {
	case e := <-other.C:
		t.Fatalf("tenant 1 got tenant 7's alarm: %+v", e)
	default:
	}
}
