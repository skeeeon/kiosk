package controller

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"
)

// TestHandleInventoryAdjust_ProjectsRow drives the projection path directly
// (bypassing JetStream) and verifies that the controller writes an
// inventory_audit row with the expected denormalized fields.
func TestHandleInventoryAdjust_ProjectsRow(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")

	payload := EventPayload{
		AdjustmentID: "adj-1",
		KioskCode:    "KIOSK-A",
		LocationCode: "WEST",
		ItemCode:     "BOLT-3",
		ItemName:     "Deck Bolt 3in",
		Mode:         "delta",
		Value:        -5,
		Delta:        -5,
		PrevQuantity: 20,
		NewQuantity:  15,
		Reason:       "shrinkage",
		Source:       "local",
		AdminID:      "admin-local-1",
		CompletedAt:  time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}

	// Direct call rather than through handle() — we have no real
	// jetstream.Msg, and the dispatch switch is one line. The real value
	// is asserting the side effect on the audit collection.
	if out := agg.ProjectInventoryAudit(payload); out != projectAck {
		t.Fatalf("ProjectInventoryAudit: got %v, want projectAck", out)
	}

	rec, err := app.FindFirstRecordByFilter("inventory_audit",
		"source_adjustment_id = {:id}",
		dbx.Params{"id": "adj-1"})
	if err != nil || rec == nil {
		t.Fatalf("expected an inventory_audit row for adj-1, got err=%v rec=%v", err, rec)
	}
	if got := rec.GetString("kiosk_code"); got != "KIOSK-A" {
		t.Errorf("kiosk_code: got %q, want KIOSK-A", got)
	}
	if got := rec.GetString("item_code"); got != "BOLT-3" {
		t.Errorf("item_code: got %q, want BOLT-3", got)
	}
	if got := rec.GetInt("delta"); got != -5 {
		t.Errorf("delta: got %d, want -5", got)
	}
	if got := rec.GetString("source"); got != "local" {
		t.Errorf("source: got %q, want local", got)
	}
	if got := rec.GetString("admin_id"); got != "admin-local-1" {
		t.Errorf("admin_id: got %q, want admin-local-1", got)
	}

	// Idempotent under redelivery: re-projecting the same payload must
	// not create a second row.
	if out := agg.ProjectInventoryAudit(payload); out != projectAck {
		t.Fatalf("ProjectInventoryAudit: got %v, want projectAck", out)
	}
	rows, err := app.FindRecordsByFilter("inventory_audit",
		"source_adjustment_id = {:id}", "", 10, 0,
		dbx.Params{"id": "adj-1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after redelivery, got %d", len(rows))
	}
}

// TestHandleInventoryAdjust_ControllerSourceUsesControllerAdminID asserts
// that the source=controller branch records ControllerAdminID into the
// shared admin_id column, since the kiosk's admin pool doesn't carry the
// controller's admin IDs.
func TestHandleInventoryAdjust_ControllerSourceUsesControllerAdminID(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil, "")

	payload := EventPayload{
		AdjustmentID:      "adj-remote-1",
		KioskCode:         "KIOSK-A",
		ItemCode:          "BOLT-3",
		ItemName:          "Deck Bolt 3in",
		Mode:              "absolute",
		Value:             30,
		Delta:             10,
		PrevQuantity:      20,
		NewQuantity:       30,
		Reason:            "recount",
		Source:            "controller",
		ControllerAdminID: "ctrl-admin-42",
		CommandID:         "cmd-uuid-abc",
		CompletedAt:       time.Now().UTC(),
	}

	if out := agg.ProjectInventoryAudit(payload); out != projectAck {
		t.Fatalf("ProjectInventoryAudit: got %v, want projectAck", out)
	}

	rec, err := app.FindFirstRecordByFilter("inventory_audit",
		"source_adjustment_id = {:id}",
		dbx.Params{"id": "adj-remote-1"})
	if err != nil || rec == nil {
		t.Fatalf("missing row for adj-remote-1: err=%v rec=%v", err, rec)
	}
	if got := rec.GetString("admin_id"); got != "ctrl-admin-42" {
		t.Errorf("admin_id: got %q, want ctrl-admin-42 (from ControllerAdminID)", got)
	}
	if got := rec.GetString("source"); got != "controller" {
		t.Errorf("source: got %q, want controller", got)
	}
	if got := rec.GetString("command_id"); got != "cmd-uuid-abc" {
		t.Errorf("command_id: got %q, want cmd-uuid-abc", got)
	}
}
