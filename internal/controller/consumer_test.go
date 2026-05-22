package controller

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/migrations"
)

// setupApp boots a fresh PB instance with both kiosk and controller-only
// migrations applied. Same pattern as internal/commit/commit_test.go's
// setupApp but with KIOSK_ROLE=controller in scope so the role-guarded
// migration registers.
func setupApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")
	t.Setenv("KIOSK_ROLE", "controller")

	// Register controller-only schema additions. Idempotent — sync.Once
	// guards multiple test invocations from re-registering.
	migrations.RegisterControllerMigrations()

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  t.TempDir(),
		HideStartBanner: true,
	})

	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	runner := core.NewMigrationsRunner(app, core.AppMigrations)
	if _, err := runner.Up(); err != nil {
		t.Fatalf("migrations up: %v", err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

func seedUser(t *testing.T, app core.App, code, name string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("code", code)
	rec.Set("name", name)
	rec.Set("email", code+"@example.com")
	rec.Set("role", "worker")
	rec.Set("active", true)
	rec.SetPassword("not-used-in-tests-but-required")
	if err := app.Save(rec); err != nil {
		t.Fatalf("save user: %v", err)
	}
	return rec.Id
}

func seedItem(t *testing.T, app core.App, code, name string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("code", code)
	rec.Set("name", name)
	rec.Set("type", "tool")
	rec.Set("tracking_mode", "quantity")
	rec.Set("active", true)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save item: %v", err)
	}
	return rec.Id
}

func TestAggregator_ProjectTransaction_Idempotent(t *testing.T) {
	app := setupApp(t)
	seedUser(t, app, "WORKER-1", "Alice")

	agg := NewAggregator(app, nil)
	payload := EventPayload{
		TransactionID: "src-tx-1",
		KioskCode:     "KIOSK-A",
		LocationCode:  "WEST",
		UserCode:      "WORKER-1",
		StartedAt:     time.Now().Add(-1 * time.Minute),
		CompletedAt:   time.Now(),
		LinesCount:    3,
	}

	if out := agg.ProjectTransaction(payload); out != projectAck {
		t.Fatalf("first projection: got %v, want projectAck", out)
	}
	// Idempotent under redelivery: a second call must not create a duplicate.
	if out := agg.ProjectTransaction(payload); out != projectAck {
		t.Fatalf("second projection: got %v, want projectAck", out)
	}

	rows, err := app.FindRecordsByFilter("transactions",
		"source_kiosk_code = 'KIOSK-A' && source_transaction_id = 'src-tx-1'",
		"", 10, 0)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after redelivery, got %d", len(rows))
	}
	if rows[0].GetInt("lines_count") != 3 {
		t.Errorf("lines_count not projected: got %d", rows[0].GetInt("lines_count"))
	}
}

func TestAggregator_ProjectTransaction_UnknownUserSkipped(t *testing.T) {
	app := setupApp(t)
	// No user seeded.

	agg := NewAggregator(app, nil)
	payload := EventPayload{
		TransactionID: "tx-no-user",
		KioskCode:     "KIOSK-A",
		UserCode:      "GHOST",
		CompletedAt:   time.Now(),
	}
	// Unknown user: we ack (retrying won't help) but produce no row.
	if out := agg.ProjectTransaction(payload); out != projectAck {
		t.Fatalf("got %v, want projectAck", out)
	}
	rows, err := app.FindRecordsByFilter("transactions",
		"source_transaction_id = 'tx-no-user'", "", 10, 0)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for unknown user, got %d", len(rows))
	}
}

func TestAggregator_ProjectLine_RetriesWhenParentMissing(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "ITEM-1", "Widget")

	agg := NewAggregator(app, nil)
	payload := EventPayload{
		TransactionID: "tx-parent-missing",
		KioskCode:     "KIOSK-A",
		LineID:        "line-1",
		ItemCode:      "ITEM-1",
		Action:        "checkout",
		Qty:           1,
		CompletedAt:   time.Now(),
	}
	if out := agg.ProjectLine(payload); out != projectRetry {
		t.Fatalf("got %v, want projectRetry (parent not present)", out)
	}
	rows, err := app.FindRecordsByFilter("transaction_lines",
		"source_line_id = 'line-1'", "", 10, 0)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows when parent missing, got %d", len(rows))
	}
}

func TestAggregator_ProjectLine_LinksToParent(t *testing.T) {
	app := setupApp(t)
	userID := seedUser(t, app, "WORKER-1", "Alice")
	itemID := seedItem(t, app, "ITEM-1", "Widget")

	// Seed a controller-side transaction row to act as the parent.
	txCol, _ := app.FindCollectionByNameOrId("transactions")
	tx := core.NewRecord(txCol)
	tx.Set("kiosk_code", "KIOSK-A")
	tx.Set("location_code", "WEST")
	tx.Set("user", userID)
	tx.Set("status", "completed")
	tx.Set("source_kiosk_code", "KIOSK-A")
	tx.Set("source_transaction_id", "src-tx-1")
	if err := app.Save(tx); err != nil {
		t.Fatalf("seed tx: %v", err)
	}

	agg := NewAggregator(app, nil)
	payload := EventPayload{
		TransactionID: "src-tx-1",
		KioskCode:     "KIOSK-A",
		LineID:        "line-1",
		ItemCode:      "ITEM-1",
		Action:        "checkout",
		Qty:           1,
		CompletedAt:   time.Now(),
	}
	if out := agg.ProjectLine(payload); out != projectAck {
		t.Fatalf("got %v, want projectAck", out)
	}

	rows, err := app.FindRecordsByFilter("transaction_lines",
		"source_line_id = {:l}", "", 10, 0, dbx.Params{"l": "line-1"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 line, got %d", len(rows))
	}
	if rows[0].GetString("transaction") != tx.Id {
		t.Errorf("line not linked to parent: got %q want %q",
			rows[0].GetString("transaction"), tx.Id)
	}
	if rows[0].GetString("item") != itemID {
		t.Errorf("line not linked to item: got %q want %q",
			rows[0].GetString("item"), itemID)
	}

	// Redelivery is idempotent.
	if out := agg.ProjectLine(payload); out != projectAck {
		t.Fatalf("second projection: got %v, want projectAck", out)
	}
	rows, err = app.FindRecordsByFilter("transaction_lines",
		"source_line_id = {:l}", "", 10, 0, dbx.Params{"l": "line-1"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("redelivery created duplicate: got %d rows", len(rows))
	}
}

func TestAggregator_TouchKiosk_AutoRegistersUnknown(t *testing.T) {
	app := setupApp(t)
	agg := NewAggregator(app, nil)

	if err := agg.TouchKiosk("KIOSK-NEW", "EAST"); err != nil {
		t.Fatalf("first touch: %v", err)
	}
	rec, err := app.FindFirstRecordByFilter("kiosks",
		"kiosk_code = 'KIOSK-NEW'", nil)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got := rec.GetString("status"); got != "unknown" {
		t.Errorf("status: got %q want %q", got, "unknown")
	}
	if got := rec.GetString("location_code"); got != "EAST" {
		t.Errorf("location_code: got %q want %q", got, "EAST")
	}
	firstSeen := rec.GetDateTime("last_seen").Time()

	// Second touch updates last_seen without creating a duplicate row.
	time.Sleep(10 * time.Millisecond)
	if err := agg.TouchKiosk("KIOSK-NEW", "EAST"); err != nil {
		t.Fatalf("second touch: %v", err)
	}
	rows, err := app.FindRecordsByFilter("kiosks",
		"kiosk_code = 'KIOSK-NEW'", "", 10, 0)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("duplicate kiosk row created: got %d", len(rows))
	}
	if !rows[0].GetDateTime("last_seen").Time().After(firstSeen) {
		t.Errorf("last_seen not advanced on second touch")
	}
}
