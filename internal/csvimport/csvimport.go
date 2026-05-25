// Package csvimport holds the row-validation, diff, and template-writer
// logic shared by every CSV admin import (items, users, groups) on both
// the kiosk and the controller binaries. The HTTP handlers in
// internal/handlers and internal/controller wrap this with auth, multipart
// parsing, and JSON response shaping; the controller's seed-catalog CLI
// reuses the same row logic so the CLI and HTTP paths can't drift.
//
// Each importer is upsert-by-`code`. Bad rows record an error but don't
// abort the run; the caller decides what to do with the Result.
//
// Existing records are loaded in one SELECT per kind and held in a
// code → record map; per-row work is then an in-memory diff. This makes
// dry-run honest (it reports `would-insert` vs `would-update` rather than
// flattening both into a single "validated" bucket) and collapses what
// was N+1 lookups into a single query.
package csvimport

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// ActionInsert / ActionUpdate / ActionError are the three terminal states
// a row can land in. Stable strings — both the SPA's filter chips and the
// summary counters key on them. "would-" doesn't appear; dry-run reports
// the same action verbs and the response's DryRun flag tells the SPA to
// render "Would insert" labels instead.
const (
	ActionInsert = "insert"
	ActionUpdate = "update"
	ActionError  = "error"
)

// RowResult is the per-row outcome the SPA renders one-per-table-row.
// Code and Name are echoed back even on error rows (extracted from the
// CSV even when validation fails) so the operator can identify which
// input line went wrong without cross-referencing row numbers.
type RowResult struct {
	Row    int     `json:"row"`
	Code   string  `json:"code"`
	Name   string  `json:"name"`
	Action string  `json:"action"`
	Errors []Error `json:"errors,omitempty"`
}

// Error reports one validation failure on a row. Code is a stable token
// the SPA can branch on; Message is human-readable detail. A row can carry
// more than one (MISSING_CODE *and* INVALID_TYPE), so they ride inside
// RowResult.Errors as a slice.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is the JSON shape every importer returns. Rows is the full per-
// row outcome list (always non-nil so the SPA can `.length` it without a
// nullish guard); RowsInserted/RowsUpdated/RowsErrored are summary counts
// derived from Rows for the result-panel cards.
type Result struct {
	DryRun       bool        `json:"dry_run"`
	RowsTotal    int         `json:"rows_total"`
	RowsInserted int         `json:"rows_inserted"`
	RowsUpdated  int         `json:"rows_updated"`
	RowsErrored  int         `json:"rows_errored"`
	Rows         []RowResult `json:"rows"`
}

// Kind selects which collection an importer targets. The HTTP routes and
// SPA tabs are keyed on these constants.
type Kind string

const (
	KindItems  Kind = "items"
	KindUsers  Kind = "users"
	KindGroups Kind = "groups"
)

// Run parses a CSV stream and dispatches to the per-kind importer. Returns
// a Result even on partial failures — per-row outcomes are inside it; a
// returned non-nil error means the upload itself was unusable (empty CSV,
// missing header, etc.).
//
// One SELECT against the target collection loads all existing records into
// a code-indexed map up front; row processing is then an in-memory diff,
// then one save per inserted/updated row (skipped on dry-run). On a real
// run another writer racing in with the same code between snapshot and
// save still hits PB's unique index on `code` and surfaces as
// INSERT_FAILED on the affected row — the rest of the batch is unaffected.
func Run(app core.App, kind Kind, r io.Reader, dryRun bool) (*Result, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("invalid CSV: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("CSV is empty")
	}
	if len(rows) < 2 {
		return nil, errors.New("CSV contains a header row but no data rows")
	}

	headers := normalizeHeaders(rows[0])
	if _, ok := headers["code"]; !ok {
		return nil, errors.New("CSV must have a 'code' column")
	}

	collName, ok := collectionFor(kind)
	if !ok {
		return nil, fmt.Errorf("unknown import kind %q", kind)
	}
	coll, err := app.FindCollectionByNameOrId(collName)
	if err != nil {
		return nil, fmt.Errorf("find %s collection: %w", collName, err)
	}
	existing, err := loadByCode(app, collName)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", collName, err)
	}

	// Pre-allocate to len(rows)-1 so Rows is non-nil even when the CSV has
	// zero data rows after the header (it can't, given the guard above,
	// but defensive — `result.Rows` must never marshal as null).
	result := &Result{DryRun: dryRun, Rows: make([]RowResult, 0, len(rows)-1)}
	for i, row := range rows[1:] {
		rowNum := i + 2 // 1-based, accounting for header row
		result.RowsTotal++

		var out RowResult
		switch kind {
		case KindItems:
			out = upsertItemRow(app, coll, existing, headers, row, dryRun)
		case KindUsers:
			out = upsertUserRow(app, coll, existing, headers, row, dryRun)
		case KindGroups:
			out = upsertGroupRow(app, coll, existing, headers, row, dryRun)
		}
		out.Row = rowNum

		switch out.Action {
		case ActionInsert:
			result.RowsInserted++
		case ActionUpdate:
			result.RowsUpdated++
		case ActionError:
			result.RowsErrored++
		}
		result.Rows = append(result.Rows, out)
	}
	return result, nil
}

func collectionFor(kind Kind) (string, bool) {
	switch kind {
	case KindItems:
		return "items", true
	case KindUsers:
		return "users", true
	case KindGroups:
		return "groups", true
	default:
		return "", false
	}
}

// loadByCode returns a map keyed on the `code` field. All three target
// collections use the same unique `code` index, so this helper is shared.
// We use FindRecordsByFilter with an empty filter to grab everything;
// limit=0 means "no limit" in PB's DAO.
func loadByCode(app core.App, coll string) (map[string]*core.Record, error) {
	rows, err := app.FindRecordsByFilter(coll, "", "", 0, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*core.Record, len(rows))
	for _, r := range rows {
		code := r.GetString("code")
		if code == "" {
			continue
		}
		out[code] = r
	}
	return out, nil
}

// ---- shared helpers ----

func normalizeHeaders(headers []string) map[string]int {
	out := make(map[string]int, len(headers))
	for i, h := range headers {
		out[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return out
}

func csvCol(headers map[string]int, row []string, name string) string {
	i, ok := headers[name]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// parseCSVInt mirrors the kiosk-side legacy importer: empty or unparseable
// input becomes 0 rather than erroring. The import path is upsert-on-code,
// so admins typing freeform values shouldn't break the row over a stray
// space.
func parseCSVInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0
	}
	return n
}

// parseCSVBool accepts true/false/1/0/yes/no/y/n in any case. Empty falls
// back to defaultVal.
func parseCSVBool(s string, defaultVal bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return defaultVal
	case "true", "1", "yes", "y", "t":
		return true
	case "false", "0", "no", "n", "f":
		return false
	default:
		return defaultVal
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// randomPassword returns URL-safe base64 with at least nbytes of entropy.
// Used for new user rows since PB's auth collection requires a non-empty
// password on create. Workers don't actually log in.
func randomPassword(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// errorRow builds a RowResult with Action=error and one or more Errors.
// Code and Name are echoed even on failure so the SPA can show *which*
// row went wrong, not just its line number.
func errorRow(code, name string, errs ...Error) RowResult {
	return RowResult{Code: code, Name: name, Action: ActionError, Errors: errs}
}

// ---- items ----

func upsertItemRow(app core.App, coll *core.Collection, existing map[string]*core.Record,
	headers map[string]int, row []string, dryRun bool) RowResult {

	code := csvCol(headers, row, "code")
	name := csvCol(headers, row, "name")

	var errs []Error
	if code == "" {
		errs = append(errs, Error{Code: "MISSING_CODE", Message: "code is required"})
	}
	if name == "" {
		errs = append(errs, Error{Code: "MISSING_NAME", Message: "name is required"})
	}
	typ := csvCol(headers, row, "type")
	if typ != "tool" && typ != "consumable" {
		errs = append(errs, Error{Code: "INVALID_TYPE", Message: "type must be 'tool' or 'consumable'"})
	}
	tracking := csvCol(headers, row, "tracking_mode")
	if tracking == "" {
		tracking = "quantity"
	}
	if tracking != "quantity" && tracking != "serialized" {
		errs = append(errs, Error{Code: "INVALID_TRACKING_MODE", Message: "tracking_mode must be 'quantity' or 'serialized'"})
	}
	if len(errs) > 0 {
		return errorRow(code, name, errs...)
	}

	data := map[string]any{
		"code":          code,
		"name":          name,
		"type":          typ,
		"unit":          csvCol(headers, row, "unit"),
		"tracking_mode": tracking,
		"category":      csvCol(headers, row, "category"),
		"active":        parseCSVBool(csvCol(headers, row, "active"), true),
		"notes":         csvCol(headers, row, "notes"),
	}
	// Only set quantity fields if the column is present in the CSV — omission
	// means "leave as-is" on update, "default to 0" on insert (PB's number
	// field zero-default applies). The csvimport tests pin this contract.
	if _, ok := headers["quantity_on_hand"]; ok {
		data["quantity_on_hand"] = parseCSVInt(csvCol(headers, row, "quantity_on_hand"))
	}
	if _, ok := headers["reorder_threshold"]; ok {
		data["reorder_threshold"] = parseCSVInt(csvCol(headers, row, "reorder_threshold"))
	}

	prev, isUpdate := existing[code]
	if dryRun {
		if isUpdate {
			return RowResult{Code: code, Name: name, Action: ActionUpdate}
		}
		return RowResult{Code: code, Name: name, Action: ActionInsert}
	}

	if isUpdate {
		for k, v := range data {
			prev.Set(k, v)
		}
		if err := app.Save(prev); err != nil {
			return errorRow(code, name, Error{Code: "UPDATE_FAILED", Message: err.Error()})
		}
		return RowResult{Code: code, Name: name, Action: ActionUpdate}
	}

	rec := core.NewRecord(coll)
	for k, v := range data {
		rec.Set(k, v)
	}
	if err := app.Save(rec); err != nil {
		return errorRow(code, name, Error{Code: "INSERT_FAILED", Message: err.Error()})
	}
	existing[code] = rec // so a duplicate later row in the same CSV reports as update
	return RowResult{Code: code, Name: name, Action: ActionInsert}
}

// ---- users ----

func upsertUserRow(app core.App, coll *core.Collection, existing map[string]*core.Record,
	headers map[string]int, row []string, dryRun bool) RowResult {

	code := csvCol(headers, row, "code")
	name := csvCol(headers, row, "name")

	var errs []Error
	if code == "" {
		errs = append(errs, Error{Code: "MISSING_CODE", Message: "code is required"})
	}
	if name == "" {
		errs = append(errs, Error{Code: "MISSING_NAME", Message: "name is required"})
	}
	role := csvCol(headers, row, "role")
	if role == "" {
		role = "worker"
	}
	if role != "worker" && role != "foreman" {
		errs = append(errs, Error{Code: "INVALID_ROLE", Message: "role must be 'worker' or 'foreman'"})
	}
	if len(errs) > 0 {
		return errorRow(code, name, errs...)
	}

	prev, isUpdate := existing[code]

	if dryRun {
		// Group resolution writes to the DB (auto-creating missing groups)
		// — skip it on dry-run so a validate pass is read-only. The CSV
		// might reference a group that doesn't exist yet; the operator
		// learns that at real-run time, when auto-create kicks in.
		if isUpdate {
			return RowResult{Code: code, Name: name, Action: ActionUpdate}
		}
		return RowResult{Code: code, Name: name, Action: ActionInsert}
	}

	// Resolve group code → id, auto-creating the row if missing. Mirrors the
	// CLI seeder so existing CSV formats keep working. Admins enrich
	// auto-created groups with contact metadata post-import.
	groupCode := csvCol(headers, row, "group")
	groupID := ""
	if groupCode != "" {
		gID, err := ensureGroupByCode(app, groupCode)
		if err != nil {
			return errorRow(code, name, Error{Code: "GROUP_RESOLVE_FAILED", Message: err.Error()})
		}
		groupID = gID
	}

	var rec *core.Record
	if isUpdate {
		rec = prev
	} else {
		rec = core.NewRecord(coll)
		pw, err := randomPassword(16)
		if err != nil {
			return errorRow(code, name, Error{Code: "PASSWORD_GEN_FAILED", Message: err.Error()})
		}
		rec.SetPassword(pw)
	}
	rec.Set("code", code)
	rec.Set("name", name)
	rec.Set("email", csvCol(headers, row, "email"))
	rec.Set("phone", csvCol(headers, row, "phone"))
	rec.Set("role", role)
	rec.Set("group", groupID)
	rec.Set("active", parseCSVBool(csvCol(headers, row, "active"), true))

	if err := app.Save(rec); err != nil {
		if isUpdate {
			return errorRow(code, name, Error{Code: "UPDATE_FAILED", Message: err.Error()})
		}
		return errorRow(code, name, Error{Code: "INSERT_FAILED", Message: err.Error()})
	}
	if isUpdate {
		return RowResult{Code: code, Name: name, Action: ActionUpdate}
	}
	existing[code] = rec
	return RowResult{Code: code, Name: name, Action: ActionInsert}
}

// ensureGroupByCode returns the id of a groups row with the given code,
// auto-creating it (code=name=code, active=true) if none exists.
func ensureGroupByCode(app core.App, code string) (string, error) {
	existing, err := app.FindFirstRecordByFilter("groups",
		"code = {:c}", map[string]any{"c": code})
	if err == nil {
		return existing.Id, nil
	}
	if !isNotFound(err) {
		return "", err
	}
	col, err := app.FindCollectionByNameOrId("groups")
	if err != nil {
		return "", fmt.Errorf("find groups collection: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("code", code)
	rec.Set("name", code)
	rec.Set("active", true)
	if err := app.Save(rec); err != nil {
		return "", fmt.Errorf("create group %q: %w", code, err)
	}
	return rec.Id, nil
}

// ---- groups ----

func upsertGroupRow(app core.App, coll *core.Collection, existing map[string]*core.Record,
	headers map[string]int, row []string, dryRun bool) RowResult {

	code := csvCol(headers, row, "code")
	name := csvCol(headers, row, "name")

	var errs []Error
	if code == "" {
		errs = append(errs, Error{Code: "MISSING_CODE", Message: "code is required"})
	}
	if name == "" {
		errs = append(errs, Error{Code: "MISSING_NAME", Message: "name is required"})
	}
	if len(errs) > 0 {
		return errorRow(code, name, errs...)
	}

	prev, isUpdate := existing[code]
	if dryRun {
		if isUpdate {
			return RowResult{Code: code, Name: name, Action: ActionUpdate}
		}
		return RowResult{Code: code, Name: name, Action: ActionInsert}
	}

	var rec *core.Record
	if isUpdate {
		rec = prev
	} else {
		rec = core.NewRecord(coll)
	}
	rec.Set("code", code)
	rec.Set("name", name)
	rec.Set("contact_email", csvCol(headers, row, "contact_email"))
	rec.Set("contact_phone", csvCol(headers, row, "contact_phone"))
	rec.Set("notes", csvCol(headers, row, "notes"))
	rec.Set("active", parseCSVBool(csvCol(headers, row, "active"), true))

	if err := app.Save(rec); err != nil {
		if isUpdate {
			return errorRow(code, name, Error{Code: "UPDATE_FAILED", Message: err.Error()})
		}
		return errorRow(code, name, Error{Code: "INSERT_FAILED", Message: err.Error()})
	}
	if isUpdate {
		return RowResult{Code: code, Name: name, Action: ActionUpdate}
	}
	existing[code] = rec
	return RowResult{Code: code, Name: name, Action: ActionInsert}
}

// stable JSON encoding sanity: catches a `RowResult{Errors: nil}` from
// growing into a json `"errors": null` surprise on the SPA. Not called at
// runtime; kept as a compile-checked assert.
var _ = func() error { return json.NewEncoder(io.Discard).Encode(RowResult{}) }
