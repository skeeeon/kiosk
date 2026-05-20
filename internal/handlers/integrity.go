package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

type openKey struct{ item, user string }

// expectedOpenCheckouts replays the transaction_lines ledger to compute what
// open_checkouts SHOULD contain right now. Used by both Integrity (which
// diffs against actual) and RebuildOpenCheckouts (which overwrites actual).
//
// Method per (item, user) pair:
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

	txCache := map[string]*core.Record{}
	getTx := func(id string) (*core.Record, error) {
		if r, ok := txCache[id]; ok {
			return r, nil
		}
		r, err := app.FindRecordById("transactions", id)
		if err != nil {
			return nil, err
		}
		txCache[id] = r
		return r, nil
	}

	expected := map[openKey]int{}
	for _, line := range lines {
		tx, err := getTx(line.GetString("transaction"))
		if err != nil {
			return nil, 0, err
		}
		if tx.GetString("status") != "completed" {
			continue
		}
		action := line.GetString("action")
		qty := line.GetInt("qty")
		item := line.GetString("item")

		switch action {
		case "checkout":
			expected[openKey{item, tx.GetString("user")}] += qty
		case "return":
			target := line.GetString("original_checkout_user")
			if target == "" {
				target = tx.GetString("user")
			}
			expected[openKey{item, target}] -= qty
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
		actual[openKey{o.GetString("item"), o.GetString("user")}]++
	}

	type diff struct {
		Item     string `json:"item"`
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
			missing = append(missing, diff{k.item, k.user, exp, act})
		} else if act > exp {
			extra = append(extra, diff{k.item, k.user, exp, act})
		}
	}
	for k, act := range actual {
		if _, seen := expected[k]; seen {
			continue
		}
		if act > 0 {
			extra = append(extra, diff{k.item, k.user, 0, act})
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

// RebuildOpenCheckouts wipes open_checkouts and repopulates it from the
// ledger. Destructive but idempotent: running it twice yields the same state.
// Use when Integrity reports drift you can't otherwise explain.
//
// Limitation: the rebuild loses per-row `checked_out_at` (we don't know which
// of N units belongs to which checkout line, only the count). All rebuilt
// rows are stamped with the most recent matching checkout line's completed_at,
// which is the best signal available.
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

		expected, _, err := expectedOpenCheckouts(tx)
		if err != nil {
			return err
		}

		col, err := tx.FindCollectionByNameOrId("open_checkouts")
		if err != nil {
			return fmt.Errorf("find open_checkouts collection: %w", err)
		}

		now := time.Now().UTC()
		for k, count := range expected {
			if count <= 0 {
				continue
			}
			item, err := tx.FindRecordById("items", k.item)
			if err != nil {
				return fmt.Errorf("find item %s: %w", k.item, err)
			}
			serial := item.GetString("serial")
			for i := 0; i < count; i++ {
				rec := core.NewRecord(col)
				rec.Set("item", k.item)
				rec.Set("user", k.user)
				if serial != "" {
					rec.Set("serial", serial)
				}
				rec.Set("checked_out_at", now)
				if err := tx.Save(rec); err != nil {
					return fmt.Errorf("insert open_checkout: %w", err)
				}
				inserted++
			}
		}
		return nil
	})
	if err != nil {
		return re.InternalServerError("rebuild failed", err)
	}

	return re.JSON(http.StatusOK, map[string]any{
		"deleted":  deleted,
		"inserted": inserted,
	})
}
