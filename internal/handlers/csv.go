package handlers

import (
	"encoding/csv"
	"net/http"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
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
	if len(rows) < 2 {
		return re.JSON(http.StatusOK, importResult{DryRun: dryRun})
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
	serial := csvCol(headers, row, "serial")
	if tracking == "serialized" && serial == "" {
		errs = append(errs, importError{Code: "MISSING_SERIAL", Message: "serialized items require a serial"})
	}

	if len(errs) > 0 {
		return nil, errs
	}

	return map[string]any{
		"code":          code,
		"name":          name,
		"type":          typ,
		"unit":          csvCol(headers, row, "unit"),
		"tracking_mode": tracking,
		"serial":        serial,
		"category":      csvCol(headers, row, "category"),
		"rfid_epc":      csvCol(headers, row, "rfid_epc"),
		"active":        parseCSVActive(csvCol(headers, row, "active")),
		"notes":         csvCol(headers, row, "notes"),
	}, nil
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
