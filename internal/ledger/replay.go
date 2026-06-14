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
	"log/slog"
	"strconv"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// replayWarnRows is the line-count threshold above which ReplayOpenRows logs a
// warning. It's the data-driven signal that one kiosk's ledger has grown large
// enough to consider the deferred per-kiosk checkpoint accelerator (replaying
// from a saved open-set baseline + only recent lines) rather than walking the
// full history on every read. Until that fires, the bounded full replay below
// stays comfortably small.
const replayWarnRows = 250_000

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
// now. Mirrors the kiosk commit hook exactly (commit.candidateOpenRows):
// checkout adds qty rows; return and admin_close each subtract qty from the
// holder's rows only (no cross-user borrowing — see removeRows). A return
// that over-subtracts leaves other users' rows intact.
//
// kioskCodeFilter (optional, empty = no filter) scopes the replay to one
// kiosk's transactions. Used by the controller's reports view to slice the
// cross-fleet ledger by originating kiosk. (admin_close lines exist only in
// a kiosk's own ledger — the controller never projects them as lines — so
// the admin_close case below is live on the kiosk and inert on the
// controller, which closes those rows via its live event projection.)
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
	// Bound the lines load to the same kiosk as the transactions above.
	// Loading the entire transaction_lines table (the previous behaviour)
	// OOMs the controller at fleet scale — even a single-kiosk view pulled
	// every kiosk's lines. Filtering indirectly through the parent
	// transaction relation scopes the load to one kiosk's history; it's the
	// same trick used in internal/exports/reports.go and
	// internal/scheduler/daily_activity.go. An empty filter (the kiosk
	// integrity-rebuild / kiosk-local path, where the DB only holds one
	// kiosk's data anyway) keeps the unbounded load.
	lineFilter := ""
	lineParams := dbx.Params{}
	if kioskCodeFilter != "" {
		lineFilter = "transaction.status = 'completed' && transaction.kiosk_code = {:kc}"
		lineParams["kc"] = kioskCodeFilter
	}
	lines, err := app.FindRecordsByFilter("transaction_lines", lineFilter, "id", 0, 0, lineParams)
	if err != nil {
		return nil, fmt.Errorf("load lines: %w", err)
	}
	if len(lines) > replayWarnRows {
		slog.Warn("ledger replay loaded a large line set; consider per-kiosk checkpoints",
			"kiosk_code", kioskCodeFilter, "lines", len(lines))
	}
	linesByTx := make(map[string][]*core.Record, len(txs))
	for _, l := range lines {
		txID := l.GetString("transaction")
		linesByTx[txID] = append(linesByTx[txID], l)
	}

	return replay(txs, linesByTx), nil
}

// replay is the shared reconstruction loop: walk the supplied transactions
// (which MUST already be ordered chronologically by completed_at) and their
// lines, adding qty rows per checkout and subtracting per return/admin_close.
// Both ReplayOpenRows (kiosk/whole-ledger) and ReplayOpenRowsForUser
// (single-user slice) build the txs + linesByTx inputs their own way and then
// delegate here so the open/close arithmetic lives in exactly one place.
func replay(txs []*core.Record, linesByTx map[string][]*core.Record) []OpenRow {
	var open []OpenRow
	for _, tx := range txs {
		txUser := tx.GetString("user")
		txCompletedAt := tx.GetDateTime("completed_at").Time()
		for _, line := range linesByTx[tx.Id] {
			action := line.GetString("action")
			qty := line.GetInt("qty")
			item := line.GetString("item")
			// Prefer the controller's cross-binary text column (kiosk-local
			// instance ids that can't live on the item_instance RelationField);
			// fall back to the real item_instance relation on the kiosk, where
			// instances are local. A line on a kiosk has no source column, so
			// GetString returns "" and we use the relation.
			instance := line.GetString("source_item_instance_id")
			if instance == "" {
				instance = line.GetString("item_instance")
			}
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
			case "return", "admin_close":
				// admin_close removes the holder's row exactly like a return.
				// Its line always carries original_checkout_user (the holder);
				// the txUser fallback is belt-and-suspenders.
				target := line.GetString("original_checkout_user")
				if target == "" {
					target = txUser
				}
				open = removeRows(open, item, instance, target, qty)
			}
		}
	}
	return open
}

// ReplayOpenRowsForUser reconstructs ONE user's open_checkouts rows from the
// ledger without scanning every kiosk's transactions. It loads only the
// transactions that can affect this user's open set:
//
//   - transactions the user performed (their own checkouts and self-returns), and
//   - transactions whose return/admin_close lines name this user as the
//     original checkout holder — a foreman returning the user's tool, or an
//     admin force-close, which live in SOMEONE ELSE'S transaction.
//
// The union is required for correctness: a foreman's return of the user's tool
// carries original_checkout_user=user but transaction.user=foreman, so a
// transaction.user=user scope alone would never close it and the row would
// stay open forever. Both sets are replayed together in chronological order
// (the shared replay() helper) and the result is filtered to this user's rows.
//
// The load is bounded to one worker's history — this is the per-user path the
// fleet-wide clock-out gate calls, deliberately avoiding the whole-table scan
// ReplayOpenRows warns about (replayWarnRows) at fleet scale.
func ReplayOpenRowsForUser(app core.App, userID string) ([]OpenRow, error) {
	if userID == "" {
		return nil, nil
	}

	txIDs := map[string]struct{}{}

	// (1) the user's own completed transactions.
	ownTxs, err := app.FindRecordsByFilter("transactions",
		"status = 'completed' && user = {:u}", "", 0, 0, dbx.Params{"u": userID})
	if err != nil {
		return nil, fmt.Errorf("load user transactions: %w", err)
	}
	for _, t := range ownTxs {
		txIDs[t.Id] = struct{}{}
	}

	// (2) transactions whose returns/closes target this user as the holder.
	closingLines, err := app.FindRecordsByFilter("transaction_lines",
		"original_checkout_user = {:u} && (action = 'return' || action = 'admin_close')",
		"", 0, 0, dbx.Params{"u": userID})
	if err != nil {
		return nil, fmt.Errorf("load closing lines: %w", err)
	}
	for _, l := range closingLines {
		if t := l.GetString("transaction"); t != "" {
			txIDs[t] = struct{}{}
		}
	}
	if len(txIDs) == 0 {
		return nil, nil
	}

	// Load the union in chronological order, plus all their lines, replay, and
	// keep only this user's open rows.
	txExpr, txParams := orIDExpr("id", txIDs)
	txs, err := app.FindRecordsByFilter("transactions",
		"status = 'completed' && ("+txExpr+")", "completed_at", 0, 0, txParams)
	if err != nil {
		return nil, fmt.Errorf("load union transactions: %w", err)
	}
	lineExpr, lineParams := orIDExpr("transaction", txIDs)
	lines, err := app.FindRecordsByFilter("transaction_lines", lineExpr, "id", 0, 0, lineParams)
	if err != nil {
		return nil, fmt.Errorf("load union lines: %w", err)
	}
	linesByTx := make(map[string][]*core.Record, len(txs))
	for _, l := range lines {
		txID := l.GetString("transaction")
		linesByTx[txID] = append(linesByTx[txID], l)
	}

	all := replay(txs, linesByTx)
	out := make([]OpenRow, 0, len(all))
	for _, r := range all {
		if r.User == userID {
			out = append(out, r)
		}
	}
	return out, nil
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
	expr, params := orIDExpr("id", ids)
	rows, err := app.FindRecordsByFilter(collection, expr, "", 0, 0, params)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.Id] = r
	}
	return out, nil
}

// orIDExpr builds a `field = {:k} || field = {:k} ...` filter over a set of
// ids plus its dbx.Params. Used to load a bounded id set in one query. Callers
// guarantee a non-empty set (an empty set yields an empty expression, which
// FindRecordsByFilter rejects).
func orIDExpr(field string, ids map[string]struct{}) (string, dbx.Params) {
	var expr string
	params := dbx.Params{}
	i := 0
	for id := range ids {
		key := "x" + strconv.Itoa(i)
		if expr != "" {
			expr += " || "
		}
		expr += field + " = {:" + key + "}"
		params[key] = id
		i++
	}
	return expr, params
}

// removeRows removes up to qty rows mirroring commit.candidateOpenRows:
// serialized lines (instance != "") close the single matching instance row;
// non-serialized lines remove rows belonging to the target user ONLY, FIFO.
//
// A shortfall (returning more than the target has out) leaves every other
// user's rows untouched. Commit deliberately does NOT borrow from another
// worker here — it stamps the offending line uncorrelated and moves on — so
// replay must not borrow either. An earlier version fell back to "any other
// user in FIFO order"; that silently diverged from commit, so the integrity
// rebuild (which replays through here) could delete an innocent worker's
// open checkout while the integrity check (which agrees with commit) still
// reported the table healthy. Keep this target-user-only to stay convergent.
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
	return out
}
