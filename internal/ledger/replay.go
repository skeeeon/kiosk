// Package ledger holds ledger-replay computations that need to run on
// both the kiosk binary (where they back the integrity/rebuild endpoints
// against the local commit-maintained open_checkouts table) and the
// controller binary (where they compute the same view on demand from the
// projected ledger, since the controller doesn't materialize
// open_checkouts).
//
// Functions take a core.App and return plain Go values; no HTTP or PB
// hook coupling.
package ledger

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// OpenRow is one unit currently checked out, reconstructed from the ledger.
// The TransactionLine FK lets a caller resolve back to the originating
// line if it needs serial / instance details that aren't denormalized here.
type OpenRow struct {
	Item            string // item id
	ItemInstance    string // item_instance id (empty for non-serialized)
	User            string // user id
	Serial          string // line.serial (empty when none)
	CheckedOutAt    time.Time
	TransactionLine string // transaction_lines.id
}

// ReplayOpenRows walks the transaction_lines ledger in chronological order
// and reconstructs the open_checkouts rows that should be present right
// now. Mirrors the kiosk commit hook's closeCheckoutsForLine fallback
// policy: when a return targets a user with fewer open rows than qty,
// the deficit is taken from any other user's rows in FIFO order.
//
// kioskCodeFilter (optional, empty = no filter) scopes the replay to one
// kiosk's transactions. Used by the controller's reports view to slice the
// cross-fleet ledger by originating kiosk.
func ReplayOpenRows(app core.App, kioskCodeFilter string) ([]OpenRow, error) {
	filter := "status = 'completed'"
	params := dbx.Params{}
	if kioskCodeFilter != "" {
		filter += " && kiosk_code = {:kc}"
		params["kc"] = kioskCodeFilter
	}
	txs, err := app.FindRecordsByFilter("transactions",
		filter, "completed_at", 0, 0, params)
	if err != nil {
		return nil, fmt.Errorf("load transactions: %w", err)
	}
	lines, err := app.FindRecordsByFilter("transaction_lines", "", "id", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load lines: %w", err)
	}
	linesByTx := make(map[string][]*core.Record, len(txs))
	for _, l := range lines {
		txID := l.GetString("transaction")
		linesByTx[txID] = append(linesByTx[txID], l)
	}

	var open []OpenRow
	for _, tx := range txs {
		txUser := tx.GetString("user")
		txCompletedAt := tx.GetDateTime("completed_at").Time()
		for _, line := range linesByTx[tx.Id] {
			action := line.GetString("action")
			qty := line.GetInt("qty")
			item := line.GetString("item")
			instance := line.GetString("item_instance")
			serial := line.GetString("serial")
			switch action {
			case "checkout":
				for i := 0; i < qty; i++ {
					open = append(open, OpenRow{
						Item:            item,
						ItemInstance:    instance,
						User:            txUser,
						Serial:          serial,
						CheckedOutAt:    txCompletedAt,
						TransactionLine: line.Id,
					})
				}
			case "return":
				target := line.GetString("original_checkout_user")
				if target == "" {
					target = txUser
				}
				open = removeRows(open, item, instance, target, qty)
			}
		}
	}
	return open, nil
}

// OpenCheckoutDTO is the hydrated wire shape returned by the reports
// endpoint. Mirrors PB's expand-shape so the SPA can render rows from
// either source (live PB read or replay) with one template.
type OpenCheckoutDTO struct {
	ID           string             `json:"id"`
	Serial       string             `json:"serial"`
	CheckedOutAt time.Time          `json:"checked_out_at"`
	KioskCode    string             `json:"kiosk_code"`
	Expand       OpenCheckoutExpand `json:"expand"`
}

type OpenCheckoutExpand struct {
	Item *OpenCheckoutItem `json:"item,omitempty"`
	User *OpenCheckoutUser `json:"user,omitempty"`
}

type OpenCheckoutItem struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type OpenCheckoutUser struct {
	ID    string `json:"id"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	Group string `json:"group"`
}

// Hydrate fills item/user/kiosk metadata for a slice of OpenRow values via
// bulk lookups. Returns an empty slice (not nil) when rows is empty so the
// SPA sees [] in JSON.
func Hydrate(app core.App, rows []OpenRow) ([]OpenCheckoutDTO, error) {
	out := make([]OpenCheckoutDTO, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	itemIDs := map[string]struct{}{}
	userIDs := map[string]struct{}{}
	lineIDs := map[string]struct{}{}
	for _, r := range rows {
		if r.Item != "" {
			itemIDs[r.Item] = struct{}{}
		}
		if r.User != "" {
			userIDs[r.User] = struct{}{}
		}
		if r.TransactionLine != "" {
			lineIDs[r.TransactionLine] = struct{}{}
		}
	}
	itemByID, err := bulkFetchByIDs(app, "items", itemIDs)
	if err != nil {
		return nil, fmt.Errorf("load items: %w", err)
	}
	userByID, err := bulkFetchByIDs(app, "users", userIDs)
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	// Resolve each line's parent transaction so we can surface kiosk_code
	// per row — useful on the controller's fleet-wide view.
	lineByID, err := bulkFetchByIDs(app, "transaction_lines", lineIDs)
	if err != nil {
		return nil, fmt.Errorf("load transaction_lines: %w", err)
	}
	txIDs := map[string]struct{}{}
	for _, l := range lineByID {
		if t := l.GetString("transaction"); t != "" {
			txIDs[t] = struct{}{}
		}
	}
	txByID, err := bulkFetchByIDs(app, "transactions", txIDs)
	if err != nil {
		return nil, fmt.Errorf("load transactions: %w", err)
	}

	// Generate a stable per-DTO id. A qty=N checkout of a non-serialized
	// item produces N rows sharing the same TransactionLine; the SPA needs
	// unique keys for v-for, so we suffix `-<index-within-line>`.
	perLineCount := map[string]int{}
	for _, r := range rows {
		idx := perLineCount[r.TransactionLine]
		perLineCount[r.TransactionLine] = idx + 1
		id := r.TransactionLine
		if idx > 0 {
			id = r.TransactionLine + "-" + strconv.Itoa(idx)
		}
		dto := OpenCheckoutDTO{
			ID:           id,
			Serial:       r.Serial,
			CheckedOutAt: r.CheckedOutAt,
		}
		if line := lineByID[r.TransactionLine]; line != nil {
			if tx := txByID[line.GetString("transaction")]; tx != nil {
				dto.KioskCode = tx.GetString("kiosk_code")
			}
		}
		if it := itemByID[r.Item]; it != nil {
			dto.Expand.Item = &OpenCheckoutItem{
				ID: it.Id, Code: it.GetString("code"),
				Name: it.GetString("name"), Type: it.GetString("type"),
			}
		}
		if u := userByID[r.User]; u != nil {
			dto.Expand.User = &OpenCheckoutUser{
				ID: u.Id, Code: u.GetString("code"),
				Name: u.GetString("name"), Group: u.GetString("group"),
			}
		}
		out = append(out, dto)
	}
	return out, nil
}

// bulkFetchByIDs returns id → *Record in a single query. Empty input
// short-circuits to an empty map.
func bulkFetchByIDs(app core.App, collection string, ids map[string]struct{}) (map[string]*core.Record, error) {
	out := map[string]*core.Record{}
	if len(ids) == 0 {
		return out, nil
	}
	var expr string
	params := dbx.Params{}
	i := 0
	for id := range ids {
		key := "i" + strconv.Itoa(i)
		if expr != "" {
			expr += " || "
		}
		expr += "id = {:" + key + "}"
		params[key] = id
		i++
	}
	rows, err := app.FindRecordsByFilter(collection, expr, "", 0, 0, params)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.Id] = r
	}
	return out, nil
}

// removeRows removes up to qty rows mirroring commit's policy: serialized
// lines (instance != "") close the single matching instance row;
// non-serialized lines prefer the target user, falling back to any other
// user in FIFO order.
func removeRows(rows []OpenRow, item, instance, target string, qty int) []OpenRow {
	if qty <= 0 {
		return rows
	}
	if instance != "" {
		for i, r := range rows {
			if r.ItemInstance == instance {
				return append(rows[:i], rows[i+1:]...)
			}
		}
		return rows
	}
	removed := 0
	out := make([]OpenRow, 0, len(rows))
	for _, r := range rows {
		if removed < qty && r.Item == item && r.ItemInstance == "" && r.User == target {
			removed++
			continue
		}
		out = append(out, r)
	}
	if removed >= qty {
		return out
	}
	rows = out
	out = make([]OpenRow, 0, len(rows))
	for _, r := range rows {
		if removed < qty && r.Item == item && r.ItemInstance == "" && r.User != target {
			removed++
			continue
		}
		out = append(out, r)
	}
	return out
}
