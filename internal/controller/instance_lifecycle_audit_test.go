package controller

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"

	"github.com/skeeeon/kiosk/internal/events"
)

// TestProjectInstanceLifecycle_ProjectsRow drives the projection path
// directly and verifies the controller writes an instance_lifecycle_audit
// row with the expected denormalized fields.
func TestProjectInstanceLifecycle_ProjectsRow(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")

	payload := EventPayload{
		SourceAuditID: "audit-1",
		KioskCode:     "KIOSK-A",
		LocationCode:  "WEST",
		ItemCode:      "DRILL-A",
		ItemName:      "Drill A",
		InstanceID:    "inst-1",
		InstanceCode:  "DRILL-A-001",
		Action:        "decommission",
		PrevActive:    true,
		NewActive:     false,
		Reason:        "damaged",
		Source:        events.SourceLocal,
		AdminID:       "admin-local-1",
		CompletedAt:   time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}

	if out := agg.ProjectInstanceLifecycle(payload); out != projectAck {
		t.Fatalf("ProjectInstanceLifecycle: got %v, want projectAck", out)
	}

	rec, err := app.FindFirstRecordByFilter("instance_lifecycle_audit",
		"source_audit_id = {:id}",
		dbx.Params{"id": "audit-1"})
	if err != nil || rec == nil {
		t.Fatalf("expected an instance_lifecycle_audit row for audit-1, got err=%v rec=%v", err, rec)
	}
	if got := rec.GetString("kiosk_code"); got != "KIOSK-A" {
		t.Errorf("kiosk_code: got %q, want KIOSK-A", got)
	}
	if got := rec.GetString("instance_id"); got != "inst-1" {
		t.Errorf("instance_id: got %q, want inst-1", got)
	}
	if got := rec.GetString("action"); got != "decommission" {
		t.Errorf("action: got %q, want decommission", got)
	}
	if got := rec.GetBool("prev_active"); got != true {
		t.Errorf("prev_active: got %v, want true", got)
	}
	if got := rec.GetString("admin_id"); got != "admin-local-1" {
		t.Errorf("admin_id: got %q, want admin-local-1", got)
	}

	// Idempotent under redelivery.
	if out := agg.ProjectInstanceLifecycle(payload); out != projectAck {
		t.Fatalf("ProjectInstanceLifecycle: got %v, want projectAck", out)
	}
	rows, err := app.FindRecordsByFilter("instance_lifecycle_audit",
		"source_audit_id = {:id}", "", 10, 0,
		dbx.Params{"id": "audit-1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after redelivery, got %d", len(rows))
	}
}

// TestProjectInstanceLifecycle_ControllerSource asserts that source=controller
// records ControllerAdminID into the shared admin_id column.
func TestProjectInstanceLifecycle_ControllerSource(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")

	payload := EventPayload{
		SourceAuditID:     "audit-remote-1",
		KioskCode:         "KIOSK-A",
		ItemCode:          "DRILL-A",
		InstanceID:        "inst-2",
		InstanceCode:      "DRILL-A-002",
		Action:            "decommission",
		PrevActive:        true,
		NewActive:         false,
		Reason:            "admin_close: damaged",
		Source:            events.SourceController,
		ControllerAdminID: "ctrl-admin-42",
		CommandID:         "cmd-uuid-xyz",
		CompletedAt:       time.Now().UTC(),
	}

	if out := agg.ProjectInstanceLifecycle(payload); out != projectAck {
		t.Fatalf("ProjectInstanceLifecycle: got %v, want projectAck", out)
	}

	rec, err := app.FindFirstRecordByFilter("instance_lifecycle_audit",
		"source_audit_id = {:id}",
		dbx.Params{"id": "audit-remote-1"})
	if err != nil || rec == nil {
		t.Fatalf("missing row for audit-remote-1: err=%v rec=%v", err, rec)
	}
	if got := rec.GetString("admin_id"); got != "ctrl-admin-42" {
		t.Errorf("admin_id: got %q, want ctrl-admin-42 (from ControllerAdminID)", got)
	}
	if got := rec.GetString("source"); got != events.SourceController {
		t.Errorf("source: got %q, want %q", got, events.SourceController)
	}
	if got := rec.GetString("command_id"); got != "cmd-uuid-xyz" {
		t.Errorf("command_id: got %q, want cmd-uuid-xyz", got)
	}
}

// TestProjectInstanceLifecycle_MissingSourceAuditID asserts we ack-and-warn
// rather than write an unkeyed row when an older kiosk publishes without the
// idempotency anchor.
func TestProjectInstanceLifecycle_MissingSourceAuditID(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")

	payload := EventPayload{
		KioskCode:    "KIOSK-A",
		InstanceID:   "inst-3",
		InstanceCode: "DRILL-A-003",
		Action:       "create",
		NewActive:    true,
		Source:       events.SourceLocal,
		AdminID:      "admin-local-1",
		CompletedAt:  time.Now().UTC(),
	}

	if out := agg.ProjectInstanceLifecycle(payload); out != projectAck {
		t.Fatalf("ProjectInstanceLifecycle: got %v, want projectAck", out)
	}

	rows, err := app.FindRecordsByFilter("instance_lifecycle_audit",
		"instance_id = {:id}", "", 10, 0,
		dbx.Params{"id": "inst-3"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for missing-anchor payload, got %d", len(rows))
	}
}
