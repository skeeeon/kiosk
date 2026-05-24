package csvimport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	// Side-effect: register kiosk-side migrations including the groups
	// collection that the users importer auto-resolves against.
	_ "github.com/skeeeon/kiosk/migrations"
)

// setupApp boots a real PocketBase app per case via the migration runner,
// matching the established test pattern (see internal/commit/commit_test.go
// and internal/controller/consumer_test.go for the same shape).
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

func TestRun_Items_DryRunWritesNothing(t *testing.T) {
	app := setupApp(t)
	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode,active",
		"HAMMER,Hammer,tool,quantity,true",
	}, "\n")

	result, err := Run(app, KindItems, strings.NewReader(csvBody), true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.DryRun || result.RowsInserted != 0 || result.RowsUpdated != 0 {
		t.Fatalf("dry run should not write: %+v", result)
	}
	if _, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "HAMMER"}); err == nil {
		t.Errorf("dry run wrote a record")
	}
}

func TestRun_Items_InsertThenUpdate(t *testing.T) {
	app := setupApp(t)
	insertCSV := strings.Join([]string{
		"code,name,type,tracking_mode,active",
		"HAMMER,Hammer,tool,quantity,true",
	}, "\n")
	r1, err := Run(app, KindItems, strings.NewReader(insertCSV), false)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if r1.RowsInserted != 1 || r1.RowsUpdated != 0 {
		t.Fatalf("insert pass: want 1 inserted/0 updated, got %+v", r1)
	}

	updateCSV := strings.Join([]string{
		"code,name,type,tracking_mode,active",
		"HAMMER,Reframing Hammer,tool,quantity,true",
	}, "\n")
	r2, err := Run(app, KindItems, strings.NewReader(updateCSV), false)
	if err != nil {
		t.Fatalf("update run: %v", err)
	}
	if r2.RowsInserted != 0 || r2.RowsUpdated != 1 {
		t.Fatalf("update pass: want 0 inserted/1 updated, got %+v", r2)
	}
	rec, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "HAMMER"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rec.GetString("name"); got != "Reframing Hammer" {
		t.Errorf("name not updated: got %q", got)
	}
}

// TestRun_Items_OmittedQuantityColumnsPreserveExistingValues pins the same
// invariant the legacy handlers test pinned: omitting quantity_on_hand /
// reorder_threshold headers must NOT overwrite the kiosk-local stock state
// with zero. The watcher leaves these fields untouched on catalog sync,
// and the importer must respect the same contract — otherwise a "refresh
// names" re-import would silently zero out every kiosk's stock.
func TestRun_Items_OmittedQuantityColumnsPreserveExistingValues(t *testing.T) {
	app := setupApp(t)

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	existing := core.NewRecord(items)
	existing.Set("code", "HAMMER")
	existing.Set("name", "Hammer")
	existing.Set("type", "tool")
	existing.Set("tracking_mode", "quantity")
	existing.Set("active", true)
	existing.Set("quantity_on_hand", 42)
	existing.Set("reorder_threshold", 10)
	if err := app.Save(existing); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// CSV omits quantity columns — like an admin uploading a renaming pass.
	updateCSV := strings.Join([]string{
		"code,name,type,tracking_mode,active,category",
		"HAMMER,Hammer (Reframed),tool,quantity,true,Hand Tools",
	}, "\n")
	if _, err := Run(app, KindItems, strings.NewReader(updateCSV), false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "HAMMER"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rec.GetInt("quantity_on_hand"); got != 42 {
		t.Errorf("quantity_on_hand: want 42, got %d", got)
	}
	if got := rec.GetInt("reorder_threshold"); got != 10 {
		t.Errorf("reorder_threshold: want 10, got %d", got)
	}
	if got := rec.GetString("name"); got != "Hammer (Reframed)" {
		t.Errorf("name not updated: got %q", got)
	}
}

func TestRun_Items_PerRowErrorsDontAbortRun(t *testing.T) {
	app := setupApp(t)
	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode",
		",MissingCode,tool,quantity",            // row 2: MISSING_CODE
		"BAD,Bad Type,widget,quantity",          // row 3: INVALID_TYPE
		"GOOD,Good,tool,quantity",               // row 4: ok
	}, "\n")

	result, err := Run(app, KindItems, strings.NewReader(csvBody), false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsInserted != 1 {
		t.Errorf("want 1 inserted (GOOD), got %d", result.RowsInserted)
	}
	if len(result.Errors) != 2 {
		t.Fatalf("want 2 errors, got %d: %+v", len(result.Errors), result.Errors)
	}
	// Errors carry the original 1-based row numbers (header is row 1).
	rows := map[int]string{}
	for _, e := range result.Errors {
		rows[e.Row] = e.Code
	}
	if rows[2] != "MISSING_CODE" {
		t.Errorf("row 2 code: want MISSING_CODE, got %q", rows[2])
	}
	if rows[3] != "INVALID_TYPE" {
		t.Errorf("row 3 code: want INVALID_TYPE, got %q", rows[3])
	}
}

func TestRun_Users_AutoCreatesGroupByCode(t *testing.T) {
	app := setupApp(t)
	csvBody := strings.Join([]string{
		"code,name,email,role,group,active",
		"W001,Alex Worker,alex@example.com,worker,CREW-A,true",
	}, "\n")
	result, err := Run(app, KindUsers, strings.NewReader(csvBody), false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsInserted != 1 || len(result.Errors) != 0 {
		t.Fatalf("want 1 inserted, no errors; got %+v", result)
	}

	group, err := app.FindFirstRecordByFilter("groups", "code = {:c}", dbx.Params{"c": "CREW-A"})
	if err != nil {
		t.Fatalf("group should have been auto-created: %v", err)
	}
	user, err := app.FindFirstRecordByFilter("users", "code = {:c}", dbx.Params{"c": "W001"})
	if err != nil {
		t.Fatalf("user lookup: %v", err)
	}
	if got := user.GetString("group"); got != group.Id {
		t.Errorf("user.group FK: want %q, got %q", group.Id, got)
	}
	if got := user.GetString("role"); got != "worker" {
		t.Errorf("role: got %q", got)
	}
}

func TestRun_Users_InvalidRoleRecordsErrorAndContinues(t *testing.T) {
	app := setupApp(t)
	// PB's users auth collection requires email — supply it on both rows
	// so the failure mode under test is the role validation, not a save
	// error masking it.
	csvBody := strings.Join([]string{
		"code,name,email,role",
		"W001,Alex,alex@example.com,worker",
		"W002,Bad Role,bad@example.com,manager",
	}, "\n")
	result, err := Run(app, KindUsers, strings.NewReader(csvBody), false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsInserted != 1 {
		t.Errorf("want 1 inserted, got %d", result.RowsInserted)
	}
	if len(result.Errors) != 1 || result.Errors[0].Code != "INVALID_ROLE" {
		t.Fatalf("want one INVALID_ROLE error, got %+v", result.Errors)
	}
}

func TestRun_Groups_UpsertWithContactMetadata(t *testing.T) {
	app := setupApp(t)

	insertCSV := strings.Join([]string{
		"code,name,contact_email,contact_phone,notes,active",
		"CREW-A,Crew A,lead@example.com,+1-555-0100,Morning shift,true",
	}, "\n")
	r1, err := Run(app, KindGroups, strings.NewReader(insertCSV), false)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if r1.RowsInserted != 1 {
		t.Fatalf("want 1 inserted, got %+v", r1)
	}

	// Second pass updates the same row by code; contact_email should change.
	updateCSV := strings.Join([]string{
		"code,name,contact_email",
		"CREW-A,Crew A (Day),new-lead@example.com",
	}, "\n")
	r2, err := Run(app, KindGroups, strings.NewReader(updateCSV), false)
	if err != nil {
		t.Fatalf("update run: %v", err)
	}
	if r2.RowsUpdated != 1 {
		t.Fatalf("want 1 updated, got %+v", r2)
	}
	rec, err := app.FindFirstRecordByFilter("groups", "code = {:c}", dbx.Params{"c": "CREW-A"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rec.GetString("contact_email"); got != "new-lead@example.com" {
		t.Errorf("contact_email: got %q", got)
	}
}

func TestRun_RejectsEmptyAndHeaderOnlyCSV(t *testing.T) {
	app := setupApp(t)

	if _, err := Run(app, KindItems, strings.NewReader(""), false); err == nil {
		t.Error("empty CSV should error")
	}
	if _, err := Run(app, KindItems, strings.NewReader("code,name,type\n"), false); err == nil {
		t.Error("header-only CSV should error")
	}
	if _, err := Run(app, KindItems, strings.NewReader("name,type\nHammer,tool\n"), false); err == nil {
		t.Error("missing code header should error")
	}
}

func TestTemplates_RoundTripThroughRun(t *testing.T) {
	// Each template should validate cleanly through Run with dry_run=true.
	// This is the contract: an admin downloads a template, edits it, and
	// uploads it back without having to translate any column names.
	app := setupApp(t)
	for _, kind := range []Kind{KindItems, KindUsers, KindGroups} {
		var buf bytes.Buffer
		writer := TemplateFor(kind)
		if writer == nil {
			t.Fatalf("no template writer for %s", kind)
		}
		if err := writer(&buf); err != nil {
			t.Fatalf("%s template: %v", kind, err)
		}
		result, err := Run(app, kind, &buf, true)
		if err != nil {
			t.Fatalf("%s round-trip Run: %v", kind, err)
		}
		if len(result.Errors) > 0 {
			t.Errorf("%s template produced validation errors on dry-run: %+v",
				kind, result.Errors)
		}
		if result.RowsTotal == 0 {
			t.Errorf("%s template had no example rows", kind)
		}
	}
}
