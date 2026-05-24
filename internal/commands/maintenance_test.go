package commands

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// integrity.rebuild covers the pure-DB path of PerformIntegrityRebuild,
// driven through the dispatcher. The handler doesn't add anything
// substantive beyond JSON wrapping; the test just confirms the wiring
// and that the result envelope carries the expected counts shape.
func TestIntegrityRebuild_HappyPath(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")

	// Empty body is valid — the kiosk reads its own ledger.
	reply := d.handleIntegrityRebuild(context.Background(), nil)
	if !reply.Success {
		t.Fatalf("expected success, got error %q", reply.Error)
	}
	dataBytes, err := json.Marshal(reply.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var out struct {
		Deleted  int `json:"deleted"`
		Inserted int `json:"inserted"`
	}
	if err := json.Unmarshal(dataBytes, &out); err != nil {
		t.Fatalf("unmarshal reply.Data: %v", err)
	}
	// Fresh setupApp has no ledger and no open_checkouts, so both counts
	// must be zero — but the operation must still succeed.
	if out.Deleted != 0 || out.Inserted != 0 {
		t.Errorf("empty-ledger rebuild: want deleted=0 inserted=0, got %+v", out)
	}
}

func TestIntegrityRebuild_InvalidJSON(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	reply := d.handleIntegrityRebuild(context.Background(), []byte("not json"))
	if reply.Success {
		t.Errorf("expected validation failure on bad JSON")
	}
}

func TestLedgerRepublish_EmptyWindow_Succeeds(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")

	reply := d.handleLedgerRepublish(context.Background(), []byte("{}"))
	if !reply.Success {
		t.Fatalf("expected success, got error %q", reply.Error)
	}
	dataBytes, _ := json.Marshal(reply.Data)
	var out struct {
		TransactionsPublished int `json:"transactions_published"`
		LinesPublished        int `json:"lines_published"`
	}
	if err := json.Unmarshal(dataBytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TransactionsPublished != 0 || out.LinesPublished != 0 {
		t.Errorf("fresh app has no ledger; want zeros, got %+v", out)
	}
}

func TestLedgerRepublish_RangeFiltering(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")

	// Seed one completed transaction so we have something to (not) republish
	// depending on the window.
	seedCompletedTx(t, app, "2026-05-15T12:00:00.000Z")

	// Range before the transaction — should publish nothing.
	beforePayload, _ := json.Marshal(map[string]string{
		"from": "2026-05-01T00:00:00Z",
		"to":   "2026-05-10T00:00:00Z",
	})
	reply := d.handleLedgerRepublish(context.Background(), beforePayload)
	if !reply.Success {
		t.Fatalf("range-before failed: %q", reply.Error)
	}
	dataBytes, _ := json.Marshal(reply.Data)
	var out struct {
		TransactionsPublished int `json:"transactions_published"`
	}
	_ = json.Unmarshal(dataBytes, &out)
	if out.TransactionsPublished != 0 {
		t.Errorf("range-before: want 0 transactions, got %d", out.TransactionsPublished)
	}

	// Range covering the transaction — should publish 1.
	coveringPayload, _ := json.Marshal(map[string]string{
		"from": "2026-05-10T00:00:00Z",
		"to":   "2026-05-20T00:00:00Z",
	})
	reply = d.handleLedgerRepublish(context.Background(), coveringPayload)
	if !reply.Success {
		t.Fatalf("range-covering failed: %q", reply.Error)
	}
	dataBytes, _ = json.Marshal(reply.Data)
	_ = json.Unmarshal(dataBytes, &out)
	if out.TransactionsPublished != 1 {
		t.Errorf("range-covering: want 1 transaction, got %d", out.TransactionsPublished)
	}
}

func TestLedgerRepublish_BadTimestamp(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")

	payload, _ := json.Marshal(map[string]string{"from": "not a date"})
	reply := d.handleLedgerRepublish(context.Background(), payload)
	if reply.Success {
		t.Errorf("expected validation failure on bad timestamp")
	}
}

// seedCompletedTx writes a minimal completed transaction at a fixed time
// so the range-filter test can predict what falls in/out of the window.
func seedCompletedTx(t *testing.T, app core.App, completedAtPB string) {
	t.Helper()
	// Seed a user first — transactions FK to users and the republish
	// handler does an FK lookup to expand into the event payload.
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	user := core.NewRecord(users)
	user.Set("code", "W-REPUB")
	user.Set("name", "Repub")
	user.Set("email", "repub@example.com")
	user.Set("role", "worker")
	user.Set("active", true)
	user.SetPassword("not-used")
	if err := app.Save(user); err != nil {
		t.Fatalf("save user: %v", err)
	}

	tx, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		t.Fatalf("find transactions: %v", err)
	}
	rec := core.NewRecord(tx)
	rec.Set("kiosk_code", "KIOSK01")
	rec.Set("location_code", "WEST")
	rec.Set("user", user.Id)
	rec.Set("started_at", completedAtPB)
	rec.Set("completed_at", completedAtPB)
	rec.Set("status", "completed")
	if err := app.Save(rec); err != nil {
		t.Fatalf("save transaction: %v", err)
	}
}
