package exports

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	_ "github.com/skeeeon/kiosk/migrations"
)

// TestCSVSafe pins the formula-injection neutralization: dangerous leading
// characters get a single-quote prefix, ordinary text and negative numbers
// pass through unchanged.
func TestCSVSafe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Hammer", "Hammer"},
		{"=HYPERLINK(\"http://evil\",\"x\")", "'=HYPERLINK(\"http://evil\",\"x\")"},
		{"+1+1", "'+1+1"},
		{"@SUM(A1)", "'@SUM(A1)"},
		{"\tcmd", "'\tcmd"},
		{"\rcmd", "'\rcmd"},
		{"-1", "-1"},          // negative number — must NOT be mangled
		{"-12.5", "-12.5"},    // negative decimal — unchanged
		{"-1+cmd(1)", "'-1+cmd(1)"}, // leading '-' but not a number — guarded
		{"EMP-1", "EMP-1"},    // dash mid-string is fine
	}
	for _, c := range cases {
		if got := csvSafe(c.in); got != c.want {
			t.Errorf("csvSafe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// setupApp mirrors the pattern in internal/commit/commit_test.go: boot a PB
// in a temp dir, apply migrations explicitly because Automigrate hooks
// OnServe (not OnBootstrap).
func setupApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")
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

func seedItem(t *testing.T, app core.App, code, name, typ string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("code", code)
	rec.Set("name", name)
	rec.Set("type", typ)
	rec.Set("tracking_mode", "quantity")
	rec.Set("active", true)
	rec.Set("quantity_on_hand", 5)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save item: %v", err)
	}
}

func TestWriteItemsCSV(t *testing.T) {
	app := setupApp(t)
	seedItem(t, app, "WRENCH-10", "10mm Wrench", "tool")
	seedItem(t, app, "GLOVES", "Nitrile Gloves", "consumable")

	var buf bytes.Buffer
	if err := WriteItemsCSV(app, &buf); err != nil {
		t.Fatalf("WriteItemsCSV: %v", err)
	}

	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected header + 2 rows, got %d", len(rows))
	}
	if rows[0][0] != "code" {
		t.Errorf("first column header: got %q, want %q", rows[0][0], "code")
	}
	// Items are sorted by code: GLOVES then WRENCH-10.
	if rows[1][0] != "GLOVES" {
		t.Errorf("row1 code: got %q, want %q", rows[1][0], "GLOVES")
	}
	if rows[2][0] != "WRENCH-10" {
		t.Errorf("row2 code: got %q, want %q", rows[2][0], "WRENCH-10")
	}
}

func TestWriteTransactionsCSV_IncludesSourceKioskWhenRequested(t *testing.T) {
	app := setupApp(t)

	// Seed a user + an item so we can create transactions.
	usersCol, _ := app.FindCollectionByNameOrId("users")
	u := core.NewRecord(usersCol)
	u.Set("code", "WORKER-1")
	u.Set("name", "Alice")
	u.Set("email", "alice@example.com")
	u.Set("role", "worker")
	u.Set("active", true)
	u.SetPassword("not-used")
	if err := app.Save(u); err != nil {
		t.Fatalf("save user: %v", err)
	}

	// Seed one completed transaction with source_* fields (as the controller
	// aggregator would write).
	txCol, _ := app.FindCollectionByNameOrId("transactions")
	tx := core.NewRecord(txCol)
	tx.Set("kiosk_code", "KIOSK-A")
	tx.Set("location_code", "WEST")
	tx.Set("user", u.Id)
	tx.Set("completed_at", time.Now().UTC())
	tx.Set("status", "completed")
	tx.Set("lines_count", 3)
	// These fields only exist on the controller — register the migration so
	// the test app has them.
	if err := app.Save(tx); err != nil {
		t.Fatalf("save tx: %v", err)
	}

	t.Run("without source kiosk", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteTransactionsCSV(app, &buf, TransactionsOptions{}); err != nil {
			t.Fatalf("WriteTransactionsCSV: %v", err)
		}
		header := firstLine(t, &buf)
		if strings.Contains(header, "source_kiosk_code") {
			t.Errorf("source_kiosk_code present when not requested: %q", header)
		}
	})

	t.Run("with source kiosk", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteTransactionsCSV(app, &buf,
			TransactionsOptions{IncludeSourceKiosk: true}); err != nil {
			t.Fatalf("WriteTransactionsCSV: %v", err)
		}
		header := firstLine(t, &buf)
		if !strings.Contains(header, "source_kiosk_code") {
			t.Errorf("source_kiosk_code missing when requested: %q", header)
		}
	})
}

func TestWriteTransactionsCSV_DateFilter(t *testing.T) {
	app := setupApp(t)
	usersCol, _ := app.FindCollectionByNameOrId("users")
	u := core.NewRecord(usersCol)
	u.Set("code", "WORKER-1")
	u.Set("name", "Alice")
	u.Set("email", "alice@example.com")
	u.Set("role", "worker")
	u.Set("active", true)
	u.SetPassword("not-used")
	if err := app.Save(u); err != nil {
		t.Fatalf("save user: %v", err)
	}
	txCol, _ := app.FindCollectionByNameOrId("transactions")
	old := core.NewRecord(txCol)
	old.Set("kiosk_code", "KA")
	old.Set("location_code", "W")
	old.Set("user", u.Id)
	old.Set("completed_at", "2026-01-01T00:00:00Z")
	old.Set("status", "completed")
	if err := app.Save(old); err != nil {
		t.Fatalf("save old: %v", err)
	}
	recent := core.NewRecord(txCol)
	recent.Set("kiosk_code", "KA")
	recent.Set("location_code", "W")
	recent.Set("user", u.Id)
	recent.Set("completed_at", "2026-05-15T00:00:00Z")
	recent.Set("status", "completed")
	if err := app.Save(recent); err != nil {
		t.Fatalf("save recent: %v", err)
	}

	var buf bytes.Buffer
	if err := WriteTransactionsCSV(app, &buf, TransactionsOptions{
		From: "2026-05-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("WriteTransactionsCSV: %v", err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Header + 1 row (recent only).
	if len(rows) != 2 {
		t.Errorf("expected 2 rows (header + 1 recent), got %d (full body: %q)",
			len(rows), buf.String())
	}
}

func firstLine(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil || len(rows) == 0 {
		t.Fatalf("parse: %v (len=%d)", err, len(rows))
	}
	return strings.Join(rows[0], ",")
}
