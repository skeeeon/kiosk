// Package csvimport holds the row-validation, upsert, and template-writer
// logic shared by every CSV admin import (items, users, groups) on both the
// kiosk and the controller binaries. The HTTP handlers in
// internal/handlers and internal/controller wrap this with auth, multipart
// parsing, and JSON response shaping; the controller's seed-catalog CLI
// reuses the same row logic so the CLI and the HTTP paths can't drift.
//
// Each importer is upsert-by-`code`. Bad rows record an error but don't
// abort the run; the caller decides what to do with the Result.
package csvimport

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// Error reports one bad row. Code is a stable token the SPA can branch on;
// Message is human-readable detail.
type Error struct {
	Row     int    `json:"row"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is the JSON shape every importer returns. DryRun=true means no
// rows were written; counters apply to validated rows only.
type Result struct {
	DryRun       bool    `json:"dry_run"`
	RowsTotal    int     `json:"rows_total"`
	RowsInserted int     `json:"rows_inserted"`
	RowsUpdated  int     `json:"rows_updated"`
	Errors       []Error `json:"errors"`
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
// a Result even on partial failures — errors per row are inside it; a
// returned non-nil error means the upload itself was unusable (empty CSV,
// missing header, etc.).
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

	result := &Result{DryRun: dryRun}
	for i, row := range rows[1:] {
		rowNum := i + 2 // 1-based, accounting for header row
		result.RowsTotal++

		var (
			inserted bool
			updated  bool
			rowErrs  []Error
		)
		switch kind {
		case KindItems:
			inserted, updated, rowErrs = upsertItemRow(app, headers, row, dryRun)
		case KindUsers:
			inserted, updated, rowErrs = upsertUserRow(app, headers, row, dryRun)
		case KindGroups:
			inserted, updated, rowErrs = upsertGroupRow(app, headers, row, dryRun)
		default:
			return nil, fmt.Errorf("unknown import kind %q", kind)
		}

		if len(rowErrs) > 0 {
			for _, e := range rowErrs {
				e.Row = rowNum
				result.Errors = append(result.Errors, e)
			}
			continue
		}
		if inserted {
			result.RowsInserted++
		}
		if updated {
			result.RowsUpdated++
		}
	}
	return result, nil
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

// ---- items ----

func upsertItemRow(app core.App, headers map[string]int, row []string, dryRun bool) (bool, bool, []Error) {
	var errs []Error

	code := csvCol(headers, row, "code")
	if code == "" {
		errs = append(errs, Error{Code: "MISSING_CODE", Message: "code is required"})
	}
	name := csvCol(headers, row, "name")
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
		return false, false, errs
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
	// field zero-default applies). This is what the kiosk's
	// csv_omitted_columns_test pins.
	if _, ok := headers["quantity_on_hand"]; ok {
		data["quantity_on_hand"] = parseCSVInt(csvCol(headers, row, "quantity_on_hand"))
	}
	if _, ok := headers["reorder_threshold"]; ok {
		data["reorder_threshold"] = parseCSVInt(csvCol(headers, row, "reorder_threshold"))
	}

	if dryRun {
		return false, false, nil
	}

	itemsCol, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return false, false, []Error{{Code: "DB_ERROR", Message: err.Error()}}
	}

	existing, err := app.FindFirstRecordByFilter("items",
		"code = {:c}", dbx.Params{"c": code})
	if err != nil && !isNotFound(err) {
		return false, false, []Error{{Code: "DB_ERROR", Message: err.Error()}}
	}

	if existing != nil {
		for k, v := range data {
			existing.Set(k, v)
		}
		if err := app.Save(existing); err != nil {
			return false, false, []Error{{Code: "UPDATE_FAILED", Message: err.Error()}}
		}
		return false, true, nil
	}

	rec := core.NewRecord(itemsCol)
	for k, v := range data {
		rec.Set(k, v)
	}
	if err := app.Save(rec); err != nil {
		return false, false, []Error{{Code: "INSERT_FAILED", Message: err.Error()}}
	}
	return true, false, nil
}

// ---- users ----

func upsertUserRow(app core.App, headers map[string]int, row []string, dryRun bool) (bool, bool, []Error) {
	var errs []Error

	code := csvCol(headers, row, "code")
	if code == "" {
		errs = append(errs, Error{Code: "MISSING_CODE", Message: "code is required"})
	}
	name := csvCol(headers, row, "name")
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
		return false, false, errs
	}

	if dryRun {
		return false, false, nil
	}

	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return false, false, []Error{{Code: "DB_ERROR", Message: err.Error()}}
	}

	// Resolve group code → id, auto-creating the row if missing. Mirrors the
	// CLI seeder so existing CSV formats keep working. Admins enrich
	// auto-created groups with contact metadata post-import.
	groupCode := csvCol(headers, row, "group")
	groupID := ""
	if groupCode != "" {
		gID, err := ensureGroupByCode(app, groupCode)
		if err != nil {
			return false, false, []Error{{Code: "GROUP_RESOLVE_FAILED", Message: err.Error()}}
		}
		groupID = gID
	}

	existing, err := app.FindFirstRecordByFilter("users",
		"code = {:c}", dbx.Params{"c": code})
	if err != nil && !isNotFound(err) {
		return false, false, []Error{{Code: "DB_ERROR", Message: err.Error()}}
	}

	var rec *core.Record
	if existing != nil {
		rec = existing
	} else {
		rec = core.NewRecord(usersCol)
		pw, err := randomPassword(16)
		if err != nil {
			return false, false, []Error{{Code: "PASSWORD_GEN_FAILED", Message: err.Error()}}
		}
		rec.SetPassword(pw)
	}
	rec.Set("code", code)
	rec.Set("name", name)
	rec.Set("email", csvCol(headers, row, "email"))
	rec.Set("role", role)
	rec.Set("group", groupID)
	rec.Set("active", parseCSVBool(csvCol(headers, row, "active"), true))

	if err := app.Save(rec); err != nil {
		if existing != nil {
			return false, false, []Error{{Code: "UPDATE_FAILED", Message: err.Error()}}
		}
		return false, false, []Error{{Code: "INSERT_FAILED", Message: err.Error()}}
	}
	if existing != nil {
		return false, true, nil
	}
	return true, false, nil
}

// ensureGroupByCode returns the id of a groups row with the given code,
// auto-creating it (code=name=code, active=true) if none exists. Same
// behavior as the legacy seed.go helper — the only writer of "minimal"
// auto-created groups; the user-import row above is the only call site
// that needs auto-create, but the helper is shared so a future caller
// can't accidentally diverge from the contract.
func ensureGroupByCode(app core.App, code string) (string, error) {
	existing, err := app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": code})
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

func upsertGroupRow(app core.App, headers map[string]int, row []string, dryRun bool) (bool, bool, []Error) {
	var errs []Error

	code := csvCol(headers, row, "code")
	if code == "" {
		errs = append(errs, Error{Code: "MISSING_CODE", Message: "code is required"})
	}
	name := csvCol(headers, row, "name")
	if name == "" {
		errs = append(errs, Error{Code: "MISSING_NAME", Message: "name is required"})
	}
	if len(errs) > 0 {
		return false, false, errs
	}

	if dryRun {
		return false, false, nil
	}

	groupsCol, err := app.FindCollectionByNameOrId("groups")
	if err != nil {
		return false, false, []Error{{Code: "DB_ERROR", Message: err.Error()}}
	}

	existing, err := app.FindFirstRecordByFilter("groups",
		"code = {:c}", dbx.Params{"c": code})
	if err != nil && !isNotFound(err) {
		return false, false, []Error{{Code: "DB_ERROR", Message: err.Error()}}
	}

	var rec *core.Record
	if existing != nil {
		rec = existing
	} else {
		rec = core.NewRecord(groupsCol)
	}
	rec.Set("code", code)
	rec.Set("name", name)
	rec.Set("contact_email", csvCol(headers, row, "contact_email"))
	rec.Set("contact_phone", csvCol(headers, row, "contact_phone"))
	rec.Set("notes", csvCol(headers, row, "notes"))
	rec.Set("active", parseCSVBool(csvCol(headers, row, "active"), true))

	if err := app.Save(rec); err != nil {
		if existing != nil {
			return false, false, []Error{{Code: "UPDATE_FAILED", Message: err.Error()}}
		}
		return false, false, []Error{{Code: "INSERT_FAILED", Message: err.Error()}}
	}
	if existing != nil {
		return false, true, nil
	}
	return true, false, nil
}
