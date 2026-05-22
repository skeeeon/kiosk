package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// openKey groups expected open_checkouts by (item, instance, user). The
// instance field is the empty string for quantity-tracked items, so the
// non-serialized path stays unchanged.
type openKey struct{ item, instance, user string }

// expectedOpenCheckouts replays the transaction_lines ledger to compute what
// open_checkouts SHOULD contain right now. Used by both Integrity (which
// diffs against actual) and RebuildOpenCheckouts (which overwrites actual).
//
// Method per (item, instance, user) tuple:
//   - checkout line: +qty
//   - return line:   -qty against the original_checkout_user (or the
//     transaction's user if unset)
//   - consume line:  ignored
//
// Negative balances are surfaced raw; callers decide whether to clamp.
func expectedOpenCheckouts(app core.App) (map[openKey]int, int, error) {
	lines, err := app.FindRecordsByFilter("transaction_lines", "", "", 0, 0)
	if err != nil {
		return nil, 0, err
	}

	// One bulk fetch instead of per-line FindRecordById: the prior version
	// did N round-trips through an in-process cache; this is a single SELECT
	// followed by map lookups.
	txs, err := app.FindRecordsByFilter("transactions", "", "", 0, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("load transactions: %w", err)
	}
	txByID := make(map[string]*core.Record, len(txs))
	for _, t := range txs {
		txByID[t.Id] = t
	}

	expected := map[openKey]int{}
	for _, line := range lines {
		tx, ok := txByID[line.GetString("transaction")]
		if !ok {
			// Orphan line (parent transaction missing) — skip rather than
			// fail the whole integrity check; the rebuild path doesn't need
			// to fabricate counts for a transaction that no longer exists.
			continue
		}
		if tx.GetString("status") != "completed" {
			continue
		}
		action := line.GetString("action")
		qty := line.GetInt("qty")
		item := line.GetString("item")
		instance := line.GetString("item_instance")

		switch action {
		case "checkout":
			expected[openKey{item, instance, tx.GetString("user")}] += qty
		case "return":
			target := line.GetString("original_checkout_user")
			if target == "" {
				target = tx.GetString("user")
			}
			expected[openKey{item, instance, target}] -= qty
		}
	}
	return expected, len(lines), nil
}

// Integrity rebuilds the expected open_checkouts state by replaying the
// transaction_lines ledger and diffs it against the actual table. Drift means
// a hook bug or a manual DB edit — either way, the ledger is authoritative
// and this endpoint is how an operator finds out.
//
// Expected counts below zero are clamped to zero (negative means uncorrelated
// returns over-subtracted; the ledger can't tell us by how much per line).
func (h *Handlers) Integrity(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	expected, checkedLines, err := expectedOpenCheckouts(h.App)
	if err != nil {
		return err
	}

	actual := map[openKey]int{}
	opens, err := h.App.FindRecordsByFilter("open_checkouts", "", "", 0, 0)
	if err != nil {
		return err
	}
	for _, o := range opens {
		actual[openKey{o.GetString("item"), o.GetString("item_instance"), o.GetString("user")}]++
	}

	type diff struct {
		Item     string `json:"item"`
		Instance string `json:"item_instance,omitempty"`
		User     string `json:"user"`
		Expected int    `json:"expected"`
		Actual   int    `json:"actual"`
	}
	var missing, extra []diff

	for k, exp := range expected {
		if exp < 0 {
			exp = 0
		}
		act := actual[k]
		if exp > act {
			missing = append(missing, diff{k.item, k.instance, k.user, exp, act})
		} else if act > exp {
			extra = append(extra, diff{k.item, k.instance, k.user, exp, act})
		}
	}
	for k, act := range actual {
		if _, seen := expected[k]; seen {
			continue
		}
		if act > 0 {
			extra = append(extra, diff{k.item, k.instance, k.user, 0, act})
		}
	}

	totalExpected := 0
	for _, v := range expected {
		if v > 0 {
			totalExpected += v
		}
	}

	return re.JSON(http.StatusOK, map[string]any{
		"checked_lines":    checkedLines,
		"expected_open":    totalExpected,
		"actual_open":      len(opens),
		"missing_in_table": missing,
		"extra_in_table":   extra,
	})
}

// replayedRow is one open_checkouts row produced by the ledger replay.
// Carries enough provenance that the rebuild can stamp the rebuilt row with
// the source checkout line's completed_at and FK back to its line, rather
// than losing both to a time.Now()-stamped synthetic row.
type replayedRow struct {
	Item            string
	ItemInstance    string
	User            string
	Serial          string
	CheckedOutAt    time.Time
	TransactionLine string
}

// replayOpenRows walks the transaction_lines ledger in chronological order
// (transactions sorted by completed_at) and reconstructs the open_checkouts
// rows that should be present right now. Mirrors the commit hook's
// closeCheckoutsForLine fallback policy: when a return targets a user with
// fewer open rows than qty, the deficit is taken from any other user's
// rows in FIFO order. This produces ground truth — running rebuild and
// then querying open_checkouts equals running every commit again.
//
// Note this differs from expectedOpenCheckouts (used by the Integrity
// diff), which charges returns strictly against the target user. The two
// can disagree in fallback scenarios; see
// integrity_divergence_test.go for the characterization.
func replayOpenRows(app core.App) ([]replayedRow, error) {
	txs, err := app.FindRecordsByFilter("transactions",
		"status = 'completed'", "completed_at", 0, 0)
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

	var open []replayedRow
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
					open = append(open, replayedRow{
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
				open = removeReplayedRows(open, item, instance, target, qty)
			}
		}
	}
	return open, nil
}

// removeReplayedRows removes up to qty rows mirroring commit's policy:
// serialized lines (instance != "") close the single matching instance row;
// non-serialized lines prefer the target user, falling back to any other
// user in FIFO order.
func removeReplayedRows(rows []replayedRow, item, instance, target string, qty int) []replayedRow {
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
	out := make([]replayedRow, 0, len(rows))
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
	out = make([]replayedRow, 0, len(rows))
	for _, r := range rows {
		if removed < qty && r.Item == item && r.ItemInstance == "" && r.User != target {
			removed++
			continue
		}
		out = append(out, r)
	}
	return out
}

// RebuildOpenCheckouts wipes open_checkouts and repopulates it from a
// ledger replay. Destructive but idempotent: running it twice yields the
// same state. Use when Integrity reports drift you can't otherwise explain.
//
// Each rebuilt row carries the source checkout line's `completed_at` as
// `checked_out_at` and a FK back to its `transaction_line`, so aging /
// audit reports stay meaningful after a rebuild.
func (h *Handlers) RebuildOpenCheckouts(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	var deleted, inserted int
	err := h.App.RunInTransaction(func(tx core.App) error {
		existing, err := tx.FindRecordsByFilter("open_checkouts", "", "", 0, 0)
		if err != nil {
			return fmt.Errorf("load open_checkouts: %w", err)
		}
		for _, r := range existing {
			if err := tx.Delete(r); err != nil {
				return fmt.Errorf("delete open_checkout %s: %w", r.Id, err)
			}
			deleted++
		}

		rows, err := replayOpenRows(tx)
		if err != nil {
			return err
		}

		col, err := tx.FindCollectionByNameOrId("open_checkouts")
		if err != nil {
			return fmt.Errorf("find open_checkouts collection: %w", err)
		}
		for _, r := range rows {
			rec := core.NewRecord(col)
			rec.Set("item", r.Item)
			rec.Set("user", r.User)
			if r.ItemInstance != "" {
				rec.Set("item_instance", r.ItemInstance)
			}
			if r.Serial != "" {
				rec.Set("serial", r.Serial)
			}
			rec.Set("checked_out_at", r.CheckedOutAt)
			rec.Set("transaction_line", r.TransactionLine)
			if err := tx.Save(rec); err != nil {
				return fmt.Errorf("insert open_checkout: %w", err)
			}
			inserted++
		}
		return nil
	})
	if err != nil {
		return re.InternalServerError("rebuild failed", err)
	}

	id := kioskctx.Get()
	events.Publish(events.IntegrityRebuildSubject(id.KioskCode), map[string]any{
		"kiosk_code":    id.KioskCode,
		"location_code": id.LocationCode,
		"admin_id":      re.Auth.Id,
		"deleted":       deleted,
		"inserted":      inserted,
		"completed_at":  time.Now().UTC(),
	})

	return re.JSON(http.StatusOK, map[string]any{
		"deleted":  deleted,
		"inserted": inserted,
	})
}
