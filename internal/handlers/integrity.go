package handlers

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

// Integrity rebuilds the expected open_checkouts state by replaying the
// transaction_lines ledger and diffs it against the actual table. Drift means
// a hook bug or a manual DB edit — either way, the ledger is authoritative
// and this endpoint is how an operator finds out.
//
// Method per (item, user) pair:
//   - checkout line: +qty
//   - return line:   -qty against the original_checkout_user (or the
//     transaction's user if unset)
//   - consume line:  ignored
//
// Expected counts below zero are clamped to zero (negative means uncorrelated
// returns over-subtracted; the ledger can't tell us by how much per line).
func (h *Handlers) Integrity(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	type key struct{ item, user string }

	expected := map[key]int{}

	lines, err := h.App.FindRecordsByFilter("transaction_lines", "", "", 0, 0)
	if err != nil {
		return err
	}

	txCache := map[string]*core.Record{}
	getTx := func(id string) (*core.Record, error) {
		if r, ok := txCache[id]; ok {
			return r, nil
		}
		r, err := h.App.FindRecordById("transactions", id)
		if err != nil {
			return nil, err
		}
		txCache[id] = r
		return r, nil
	}

	for _, line := range lines {
		tx, err := getTx(line.GetString("transaction"))
		if err != nil {
			return err
		}
		if tx.GetString("status") != "completed" {
			continue
		}
		action := line.GetString("action")
		qty := line.GetInt("qty")
		item := line.GetString("item")

		switch action {
		case "checkout":
			expected[key{item, tx.GetString("user")}] += qty
		case "return":
			target := line.GetString("original_checkout_user")
			if target == "" {
				target = tx.GetString("user")
			}
			expected[key{item, target}] -= qty
		}
	}

	actual := map[key]int{}
	opens, err := h.App.FindRecordsByFilter("open_checkouts", "", "", 0, 0)
	if err != nil {
		return err
	}
	for _, o := range opens {
		actual[key{o.GetString("item"), o.GetString("user")}]++
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
		"checked_lines":    len(lines),
		"expected_open":    totalExpected,
		"actual_open":      len(opens),
		"missing_in_table": missing,
		"extra_in_table":   extra,
	})
}
