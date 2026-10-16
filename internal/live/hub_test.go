package live

import (
	"testing"

	"github.com/zerowatch/zerowatch/internal/model"
)

func TestHubFilterTenantAndDrop(t *testing.T) {
	h := NewHub(2, 2)
	errs, _ := h.Subscribe(1, func(e *Event) bool { return e.Log != nil && e.Log.Level == "error" })
	all, _ := h.Subscribe(1, nil)
	if _, ok := h.Subscribe(1, nil); ok {
		t.Fatal("expected maxSubs to be enforced")
	}
	h.PublishLogs([]model.LogEvent{
		{TenantID: 1, Level: "info", Message: "a"},
		{TenantID: 1, Level: "error", Message: "b"},
		{TenantID: 2, Level: "error", Message: "other tenant"},
		{TenantID: 1, Level: "error", Message: "c"},
	})
	if got := len(errs.C); got != 2 {
		t.Fatalf("error subscriber got %d events", got)
	}
	// "all" has buffer 2 and received 3 tenant-1 events -> 1 dropped.
	if all.Dropped() != 1 || len(all.C) != 2 {
		t.Fatalf("all: dropped=%d len=%d", all.Dropped(), len(all.C))
	}
	all.Close()
	all.Close() // idempotent
	if h.Subscribers() != 1 {
		t.Fatalf("subscribers=%d", h.Subscribers())
	}
	if e := <-errs.C; e.Log.Message != "b" {
		t.Fatalf("unexpected first event %q", e.Log.Message)
	}
}
