package handlers

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/exports"
)

const maxCSVUploadBytes = 10 << 20 // 10 MB — enough for tens of thousands of items

type importError struct {
	Row     int    `json:"row"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type importResult struct {
	DryRun       bool          `json:"dry_run"`
	RowsTotal    int           `json:"rows_total"`
	RowsInserted int           `json:"rows_inserted"`
	RowsUpdated  int           `json:"rows_updated"`
	Errors       []importError `json:"errors"`
}

// CSVImport upserts items from a CSV upload. Rows match existing records by
// `code`. Each row is processed independently: a bad row records an error
// but doesn't stop the rest. `dry_run=true` validates without writing.
//
// Items not present in the CSV are left alone (no auto-deactivation, per
// the plan).
func (h *Handlers) CSVImport(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	if err := re.Request.ParseMultipartForm(maxCSVUploadBytes); err != nil {
		return re.BadRequestError("could not parse upload (max 10 MB)", err)
	}

	file, _, err := re.Request.FormFile("file")
	if err != nil {
		return re.BadRequestError("file field is required", err)
	}
	defer file.Close()

	dryRun := strings.EqualFold(re.Request.FormValue("dry_run"), "true")

	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		return re.BadRequestError("invalid CSV", err)
	}
	if len(rows) == 0 {
		return re.BadRequestError("CSV is empty", nil)
	}
	if len(rows) < 2 {
		return re.BadRequestError("CSV contains a header row but no data rows", nil)
	}

	headers := normalizeHeaders(rows[0])
	if _, ok := headers["code"]; !ok {
		return re.BadRequestError("CSV must have a 'code' column", nil)
	}

	itemsCol, err := h.App.FindCollectionByNameOrId("items")
	if err != nil {
		return err
	}

	result := importResult{DryRun: dryRun}
	for i, row := range rows[1:] {
		rowNum := i + 2 // 1-based, accounting for header row
		result.RowsTotal++

		data, rowErrs := validateImportRow(headers, row)
		if len(rowErrs) > 0 {
			for _, e := range rowErrs {
				e.Row = rowNum
				result.Errors = append(result.Errors, e)
			}
			continue
		}

		if dryRun {
			continue
		}

		existing, err := h.App.FindFirstRecordByFilter(
			"items", "code = {:c}", dbx.Params{"c": data["code"]},
		)
		if err != nil && !isNotFound(err) {
			result.Errors = append(result.Errors, importError{
				Row: rowNum, Code: "DB_ERROR", Message: err.Error(),
			})
			continue
		}

		if existing != nil {
			for k, v := range data {
				existing.Set(k, v)
			}
			if err := h.App.Save(existing); err != nil {
				result.Errors = append(result.Errors, importError{
					Row: rowNum, Code: "UPDATE_FAILED", Message: err.Error(),
				})
				continue
			}
			result.RowsUpdated++
		} else {
			rec := core.NewRecord(itemsCol)
			for k, v := range data {
				rec.Set(k, v)
			}
			if err := h.App.Save(rec); err != nil {
				result.Errors = append(result.Errors, importError{
					Row: rowNum, Code: "INSERT_FAILED", Message: err.Error(),
				})
				continue
			}
			result.RowsInserted++
		}
	}

	return re.JSON(http.StatusOK, result)
}

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

// validateImportRow checks required fields and enum values. It does not check
// uniqueness across the file or against the database — duplicate codes within
// the same CSV will simply upsert in order; DB-level uniqueness conflicts
// surface as INSERT_FAILED on save.
func validateImportRow(headers map[string]int, row []string) (map[string]any, []importError) {
	var errs []importError

	code := csvCol(headers, row, "code")
	if code == "" {
		errs = append(errs, importError{Code: "MISSING_CODE", Message: "code is required"})
	}
	name := csvCol(headers, row, "name")
	if name == "" {
		errs = append(errs, importError{Code: "MISSING_NAME", Message: "name is required"})
	}
	typ := csvCol(headers, row, "type")
	if typ != "tool" && typ != "consumable" {
		errs = append(errs, importError{Code: "INVALID_TYPE", Message: "type must be 'tool' or 'consumable'"})
	}
	tracking := csvCol(headers, row, "tracking_mode")
	if tracking == "" {
		tracking = "quantity"
	}
	if tracking != "quantity" && tracking != "serialized" {
		errs = append(errs, importError{Code: "INVALID_TRACKING_MODE", Message: "tracking_mode must be 'quantity' or 'serialized'"})
	}

	if len(errs) > 0 {
		return nil, errs
	}

	out := map[string]any{
		"code":          code,
		"name":          name,
		"type":          typ,
		"unit":          csvCol(headers, row, "unit"),
		"tracking_mode": tracking,
		"category":      csvCol(headers, row, "category"),
		"active":        parseCSVActive(csvCol(headers, row, "active")),
		"notes":         csvCol(headers, row, "notes"),
	}
	// Only set quantity fields if the column is present in the CSV — omission
	// means "leave as-is" on update, "default to 0" on insert (PB's number
	// field zero-default applies).
	if _, ok := headers["quantity_on_hand"]; ok {
		out["quantity_on_hand"] = parseCSVInt(csvCol(headers, row, "quantity_on_hand"))
	}
	if _, ok := headers["reorder_threshold"]; ok {
		out["reorder_threshold"] = parseCSVInt(csvCol(headers, row, "reorder_threshold"))
	}
	return out, nil
}

// parseCSVInt parses a quantity column. Empty or unparseable input becomes 0
// — the import path is upsert-on-code, so admins typing freeform values
// shouldn't break the row over a stray space.
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

// TransactionsExportCSV streams completed transactions as CSV. Optional
// ?from= and ?to= ISO timestamps narrow the window; both are inclusive on
// completed_at. Line counts are read from transactions.lines_count
// (denormalized at commit time), so the export is a single SELECT.
func (h *Handlers) TransactionsExportCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	from := re.Request.URL.Query().Get("from")
	to := re.Request.URL.Query().Get("to")
	if from != "" {
		if _, err := time.Parse(time.RFC3339, from); err != nil {
			return re.BadRequestError("from must be RFC3339 (e.g. 2026-05-01T00:00:00Z)", err)
		}
	}
	if to != "" {
		if _, err := time.Parse(time.RFC3339, to); err != nil {
			return re.BadRequestError("to must be RFC3339", err)
		}
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"transactions-%s.csv\"", time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteTransactionsCSV(h.App, w, exports.TransactionsOptions{
		From: from,
		To:   to,
		// IncludeSourceKiosk left false — on a standalone kiosk those fields
		// are always blank.
	})
}

// ItemsExportCSV streams the items collection as CSV with the same column
// shape the importer accepts, so an export can round-trip back through
// /items/import. Instances are intentionally not exported here — they live
// in their own collection and aren't part of the items round-trip.
func (h *Handlers) ItemsExportCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"items-%s.csv\"", time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteItemsCSV(h.App, w)
}

// parseCSVActive treats empty as active=true; only explicit falsy values
// disable. Accepts true/false/1/0/yes/no/y/n in any case.
func parseCSVActive(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "0", "no", "n", "f":
		return false
	default:
		return true
	}
}
