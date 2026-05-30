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

// countActions tallies a Result's Rows by Action, so tests can assert on
// the per-row breakdown without indexing into the slice.
func countActions(rows []RowResult) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		out[r.Action]++
	}
	return out
}

func TestRun_Items_DryRunClassifiesInsertVsUpdate(t *testing.T) {
	app := setupApp(t)

	// Seed one existing item so we have a known update target.
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	rec := core.NewRecord(items)
	rec.Set("code", "HAMMER")
	rec.Set("name", "Hammer")
	rec.Set("type", "tool")
	rec.Set("tracking_mode", "quantity")
	rec.Set("active", true)
	if err := app.Save(rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode,active",
		"HAMMER,Hammer (Renamed),tool,quantity,true",  // would update
		"WRENCH,Wrench,tool,quantity,true",            // would insert
	}, "\n")

	result, err := Run(app, KindItems, strings.NewReader(csvBody), true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.DryRun {
		t.Error("DryRun flag not set on response")
	}
	if got := countActions(result.Rows); got[ActionInsert] != 1 || got[ActionUpdate] != 1 {
		t.Errorf("dry-run actions: want 1 insert + 1 update, got %v", got)
	}
	if result.RowsInserted != 1 || result.RowsUpdated != 1 {
		t.Errorf("summary counts: want 1/1, got %d/%d",
			result.RowsInserted, result.RowsUpdated)
	}

	// Dry-run must NOT touch the DB — the existing row's name still reads
	// "Hammer", not "Hammer (Renamed)".
	reload, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "HAMMER"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reload.GetString("name"); got != "Hammer" {
		t.Errorf("dry-run wrote: name = %q (expected unchanged)", got)
	}
	if _, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "WRENCH"}); err == nil {
		t.Error("dry-run inserted a row")
	}
}

func TestRun_Items_RealRunInsertsThenUpdates(t *testing.T) {
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
		t.Fatalf("insert pass: want 1/0, got %d/%d", r1.RowsInserted, r1.RowsUpdated)
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
		t.Fatalf("update pass: want 0/1, got %d/%d", r2.RowsInserted, r2.RowsUpdated)
	}
	rec, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "HAMMER"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rec.GetString("name"); got != "Reframing Hammer" {
		t.Errorf("name not updated: got %q", got)
	}
}

// TestRun_Items_DuplicateCodeInSameCSVTreatedAsUpdate exercises a subtle
// edge case of the bulk-snapshot approach: when the same code appears twice
// in the input, the second occurrence must see the first as an existing
// row so it classifies as update rather than colliding on insert.
func TestRun_Items_DuplicateCodeInSameCSVTreatedAsUpdate(t *testing.T) {
	app := setupApp(t)

	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode,active",
		"HAMMER,Hammer,tool,quantity,true",          // insert
		"HAMMER,Hammer (v2),tool,quantity,true",     // update of the line above
	}, "\n")
	result, err := Run(app, KindItems, strings.NewReader(csvBody), false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsInserted != 1 || result.RowsUpdated != 1 {
		t.Errorf("intra-CSV dup: want 1/1, got %d/%d",
			result.RowsInserted, result.RowsUpdated)
	}
	rec, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "HAMMER"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rec.GetString("name"); got != "Hammer (v2)" {
		t.Errorf("name: want last value, got %q", got)
	}
}

// TestRun_Items_OmittedQuantityColumnsPreserveExistingValues pins the
// kiosk-local stock invariant: omitting quantity_on_hand/reorder_threshold
// headers must NOT overwrite existing values with zero. The watcher leaves
// these fields untouched on catalog sync; the importer must respect the
// same contract — otherwise a "refresh names" re-import would silently
// zero out every kiosk's stock.
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

// TestRun_Items_SerializedIgnoresQuantityColumn verifies that a serialized
// row's quantity_on_hand column is ignored (the value is derived from active
// instances), without erroring the row. reorder_threshold still applies.
func TestRun_Items_SerializedIgnoresQuantityColumn(t *testing.T) {
	app := setupApp(t)

	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode,active,quantity_on_hand,reorder_threshold",
		"DRILL-S,Serialized Drill,tool,serialized,true,99,2",
	}, "\n")
	result, err := Run(app, KindItems, strings.NewReader(csvBody), false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsInserted != 1 {
		t.Fatalf("rows inserted: want 1, got %d (errors=%d)", result.RowsInserted, result.RowsErrored)
	}

	rec, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": "DRILL-S"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// quantity_on_hand column ignored → PB zero-default, not the CSV's 99.
	if got := rec.GetInt("quantity_on_hand"); got != 0 {
		t.Errorf("quantity_on_hand: want 0 (CSV value ignored for serialized), got %d", got)
	}
	// reorder_threshold still honored.
	if got := rec.GetInt("reorder_threshold"); got != 2 {
		t.Errorf("reorder_threshold: want 2, got %d", got)
	}
}

func TestRun_Items_PerRowErrorsDontAbortRun(t *testing.T) {
	app := setupApp(t)
	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode",
		",MissingCode,tool,quantity",        // row 2: MISSING_CODE
		"BAD,Bad Type,widget,quantity",       // row 3: INVALID_TYPE
		"GOOD,Good,tool,quantity",            // row 4: ok
	}, "\n")

	result, err := Run(app, KindItems, strings.NewReader(csvBody), false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsInserted != 1 {
		t.Errorf("want 1 inserted (GOOD), got %d", result.RowsInserted)
	}
	if result.RowsErrored != 2 {
		t.Errorf("want 2 errored, got %d", result.RowsErrored)
	}

	// Errors are addressable per-row, with original 1-based row numbers.
	codesByRow := map[int]string{}
	for _, r := range result.Rows {
		if r.Action == ActionError && len(r.Errors) > 0 {
			codesByRow[r.Row] = r.Errors[0].Code
		}
	}
	if codesByRow[2] != "MISSING_CODE" {
		t.Errorf("row 2: want MISSING_CODE, got %q", codesByRow[2])
	}
	if codesByRow[3] != "INVALID_TYPE" {
		t.Errorf("row 3: want INVALID_TYPE, got %q", codesByRow[3])
	}
}

// TestRun_Items_MultipleValidationErrorsRideOnSameRow confirms the
// importer surfaces *all* validation problems on a row rather than
// short-circuiting at the first one. A row missing both code and name
// should carry two Errors entries.
func TestRun_Items_MultipleValidationErrorsRideOnSameRow(t *testing.T) {
	app := setupApp(t)
	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode",
		",,widget,quantity", // missing code, missing name, AND invalid type
	}, "\n")
	result, err := Run(app, KindItems, strings.NewReader(csvBody), false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(result.Rows))
	}
	row := result.Rows[0]
	if row.Action != ActionError {
		t.Errorf("action: want error, got %q", row.Action)
	}
	if len(row.Errors) < 3 {
		t.Errorf("want >= 3 errors on this row, got %d (%+v)", len(row.Errors), row.Errors)
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
	if result.RowsInserted != 1 || result.RowsErrored != 0 {
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

// TestRun_Users_DryRunSkipsGroupAutoCreate ensures a validate pass is
// strictly read-only, even when a row references a group code that
// doesn't exist yet. The auto-create kicks in only on real-run.
func TestRun_Users_DryRunSkipsGroupAutoCreate(t *testing.T) {
	app := setupApp(t)
	csvBody := strings.Join([]string{
		"code,name,email,role,group",
		"W001,Alex,alex@example.com,worker,GHOST-CREW",
	}, "\n")
	result, err := Run(app, KindUsers, strings.NewReader(csvBody), true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsInserted != 1 {
		t.Errorf("want 1 would-insert, got %d", result.RowsInserted)
	}
	if _, err := app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": "GHOST-CREW"}); err == nil {
		t.Error("dry-run created a group")
	}
}

func TestRun_Users_InvalidRoleRecordsErrorAndContinues(t *testing.T) {
	app := setupApp(t)
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
	if result.RowsErrored != 1 {
		t.Fatalf("want 1 errored, got %d", result.RowsErrored)
	}
	// Find the errored row and check its code.
	var errored *RowResult
	for i := range result.Rows {
		if result.Rows[i].Action == ActionError {
			errored = &result.Rows[i]
			break
		}
	}
	if errored == nil || len(errored.Errors) == 0 || errored.Errors[0].Code != "INVALID_ROLE" {
		t.Errorf("want INVALID_ROLE, got %+v", errored)
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

// TestRun_RowsIsNeverNil pins the JSON contract: even when nothing happens
// the Rows slice must be non-nil so it marshals as `[]` rather than `null`.
// The SPA reads `.length` directly.
func TestRun_RowsIsNeverNil(t *testing.T) {
	app := setupApp(t)
	csvBody := strings.Join([]string{
		"code,name,type,tracking_mode,active",
		"HAMMER,Hammer,tool,quantity,true",
	}, "\n")
	result, err := Run(app, KindItems, strings.NewReader(csvBody), true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Rows == nil {
		t.Fatal("Rows must be non-nil so it marshals as [] not null")
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
		if result.RowsErrored > 0 {
			t.Errorf("%s template produced %d errored rows on dry-run", kind, result.RowsErrored)
		}
		if result.RowsTotal == 0 {
			t.Errorf("%s template had no example rows", kind)
		}
	}
}
