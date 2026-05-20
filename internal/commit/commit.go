// Package commit promotes an in-memory cart into a persisted transaction,
// updates open_checkouts to reflect the world, and emits events. All
// database writes happen inside a single PocketBase transaction; events
// are emitted only after that transaction commits successfully.
//
// This is the heart of the system: every state-changing action a worker
// takes (checkout, return, consume) flows through Commit. Tests for the
// state machine live alongside this file.
package commit

import (
	"errors"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// Result is the JSON returned from the commit endpoint and is also useful
// for tests. Action counters are line counts (not qty totals) to match the
// API spec in §7 of the plan.
type Result struct {
	TransactionID string   `json:"transaction_id"`
	LinesCount    int      `json:"lines_count"`
	CheckedOut    int      `json:"checked_out"`
	Returned      int      `json:"returned"`
	Consumed      int      `json:"consumed"`
	Warnings      []string `json:"warnings,omitempty"`
}

// Policy is the kiosk's return-acceptance configuration. Defaults are
// permissive (matches the v1 behavior where these flags weren't enforced);
// flip a field to false to reject the corresponding case at commit time
// and roll the whole transaction back.
type Policy struct {
	AllowCrossUser    bool
	AllowUncorrelated bool
}

// DefaultPolicy returns the permissive policy used by tests and as the
// safe baseline when no config has been loaded.
func DefaultPolicy() Policy {
	return Policy{AllowCrossUser: true, AllowUncorrelated: true}
}

// PublishFunc is injected so tests can verify events without setting up a
// real publisher. Production passes events.Publish.
type PublishFunc func(subject string, payload any)

// Commit is the entry point. It validates the cart, opens a DB transaction,
// writes the transactions + transaction_lines records, applies the per-line
// open_checkouts side effects, and finally — outside the DB transaction —
// emits one transaction.complete event plus one item.{action} event per line.
func Commit(app core.App, c *cart.Cart, id kioskctx.Identity, policy Policy, publish PublishFunc) (*Result, error) {
	if c == nil {
		return nil, errors.New("cart is nil")
	}
	if len(c.Lines) == 0 {
		return nil, errors.New("cart has no lines")
	}
	if id.KioskCode == "" || id.LocationCode == "" {
		return nil, errors.New("kiosk identity is not set")
	}

	if !policy.AllowCrossUser {
		for _, l := range c.Lines {
			if l.Action == "return" && l.OriginalCheckoutUserID != "" && l.OriginalCheckoutUserID != c.UserID {
				return nil, fmt.Errorf("item %s is checked out to another worker; cross-user returns are not allowed", l.ItemCode)
			}
		}
	}

	completedAt := time.Now().UTC()
	var (
		result      Result
		lineEvents  []lineEvent
		txCompleted lineEvent // payload for the transaction.complete event
	)

	err := app.RunInTransaction(func(tx core.App) error {
		userRec, err := tx.FindRecordById("users", c.UserID)
		if err != nil {
			return fmt.Errorf("find user %s: %w", c.UserID, err)
		}

		txRec, err := createTransaction(tx, c, id, completedAt)
		if err != nil {
			return err
		}
		result.TransactionID = txRec.Id

		openCol, err := tx.FindCollectionByNameOrId("open_checkouts")
		if err != nil {
			return fmt.Errorf("find open_checkouts collection: %w", err)
		}
		linesCol, err := tx.FindCollectionByNameOrId("transaction_lines")
		if err != nil {
			return fmt.Errorf("find transaction_lines collection: %w", err)
		}

		for _, l := range c.Lines {
			itemRec, err := tx.FindRecordById("items", l.ItemID)
			if err != nil {
				return fmt.Errorf("find item %s: %w", l.ItemID, err)
			}
			itemType := itemRec.GetString("type")
			if !cart.ValidActionForType(l.Action, itemType) {
				return fmt.Errorf("item %s (%s) cannot %s", itemRec.GetString("code"), itemType, l.Action)
			}
			if l.Qty < 1 || l.Qty > cart.MaxQty {
				return fmt.Errorf("item %s qty=%d out of range (1..%d)", itemRec.GetString("code"), l.Qty, cart.MaxQty)
			}
			if itemRec.GetString("tracking_mode") == "serialized" && l.Qty != 1 {
				return fmt.Errorf("serialized item %s must have qty=1, got %d", itemRec.GetString("code"), l.Qty)
			}

			lineRec, err := createLine(tx, linesCol, txRec, l)
			if err != nil {
				return err
			}

			switch l.Action {
			case "checkout":
				if err := openCheckoutsForLine(tx, openCol, lineRec, itemRec, c.UserID, completedAt, l.Qty); err != nil {
					return err
				}
				result.CheckedOut++

			case "return":
				uncorrelated, err := closeCheckoutsForLine(tx, lineRec, itemRec, c.UserID, l.Qty)
				if err != nil {
					return err
				}
				if uncorrelated {
					if !policy.AllowUncorrelated {
						return fmt.Errorf("return of %s does not match any open checkout; uncorrelated returns are not allowed", itemRec.GetString("code"))
					}
					lineRec.Set("uncorrelated", true)
					if err := tx.Save(lineRec); err != nil {
						return fmt.Errorf("mark line uncorrelated: %w", err)
					}
				}
				result.Returned++

			case "consume":
				// Decrement stock. Allowed to go negative — the ledger is the
				// source of truth; if a worker takes more than was recorded,
				// the low-stock report surfaces the discrepancy rather than
				// blocking the take.
				itemRec.Set("quantity_on_hand", itemRec.GetInt("quantity_on_hand")-l.Qty)
				if err := tx.Save(itemRec); err != nil {
					return fmt.Errorf("decrement quantity_on_hand for %s: %w", itemRec.GetString("code"), err)
				}
				result.Consumed++
			}

			lineEvents = append(lineEvents, lineEvent{
				Subject: fmt.Sprintf("kiosk.%s.item.%s", id.KioskCode, l.Action),
				Payload: map[string]any{
					"transaction_id": txRec.Id,
					"line_id":        lineRec.Id,
					"kiosk_code":     id.KioskCode,
					"location_code":  id.LocationCode,
					"user_id":        c.UserID,
					"user_code":      c.UserCode,
					"item_id":        itemRec.Id,
					"item_code":      itemRec.GetString("code"),
					"item_name":      itemRec.GetString("name"),
					"action":         l.Action,
					"qty":            l.Qty,
					"serial":         l.Serial,
					"uncorrelated":   lineRec.GetBool("uncorrelated"),
					"completed_at":   completedAt,
				},
			})
		}

		result.LinesCount = len(c.Lines)
		txCompleted = lineEvent{
			Subject: fmt.Sprintf("kiosk.%s.transaction.complete", id.KioskCode),
			Payload: map[string]any{
				"transaction_id": txRec.Id,
				"kiosk_code":     id.KioskCode,
				"location_code":  id.LocationCode,
				"user_id":        c.UserID,
				"user_code":      c.UserCode,
				"user_name":      userRec.GetString("name"),
				"started_at":     c.StartedAt,
				"completed_at":   completedAt,
				"lines_count":    result.LinesCount,
				"checked_out":    result.CheckedOut,
				"returned":       result.Returned,
				"consumed":       result.Consumed,
			},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	publish(txCompleted.Subject, txCompleted.Payload)
	for _, ev := range lineEvents {
		publish(ev.Subject, ev.Payload)
	}
	return &result, nil
}

type lineEvent struct {
	Subject string
	Payload map[string]any
}

func createTransaction(tx core.App, c *cart.Cart, id kioskctx.Identity, completedAt time.Time) (*core.Record, error) {
	col, err := tx.FindCollectionByNameOrId("transactions")
	if err != nil {
		return nil, fmt.Errorf("find transactions collection: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("kiosk_code", id.KioskCode)
	rec.Set("location_code", id.LocationCode)
	rec.Set("user", c.UserID)
	rec.Set("started_at", c.StartedAt)
	rec.Set("completed_at", completedAt)
	rec.Set("status", "completed")
	if err := tx.Save(rec); err != nil {
		return nil, fmt.Errorf("save transaction: %w", err)
	}
	return rec, nil
}

func createLine(tx core.App, col *core.Collection, txRec *core.Record, l *cart.Line) (*core.Record, error) {
	rec := core.NewRecord(col)
	rec.Set("transaction", txRec.Id)
	rec.Set("item", l.ItemID)
	rec.Set("action", l.Action)
	rec.Set("qty", l.Qty)
	if l.Serial != "" {
		rec.Set("serial", l.Serial)
	}
	if l.OriginalCheckoutUserID != "" {
		rec.Set("original_checkout_user", l.OriginalCheckoutUserID)
	}
	if err := tx.Save(rec); err != nil {
		return nil, fmt.Errorf("save transaction_line: %w", err)
	}
	return rec, nil
}

// openCheckoutsForLine inserts one open_checkouts row per unit. Serialized
// tools always have qty=1; non-serialized may have qty>1.
func openCheckoutsForLine(tx core.App, col *core.Collection, lineRec, itemRec *core.Record, userID string, completedAt time.Time, qty int) error {
	serial := itemRec.GetString("serial")
	for i := 0; i < qty; i++ {
		rec := core.NewRecord(col)
		rec.Set("item", itemRec.Id)
		rec.Set("user", userID)
		if serial != "" {
			rec.Set("serial", serial)
		}
		rec.Set("checked_out_at", completedAt)
		rec.Set("transaction_line", lineRec.Id)
		if err := tx.Save(rec); err != nil {
			return fmt.Errorf("insert open_checkouts: %w", err)
		}
	}
	return nil
}

// closeCheckoutsForLine deletes up to qty rows. For serialized tools, the
// row is uniquely identified by item; for non-serialized, we prefer rows
// belonging to the line's original_checkout_user (or the cart user if unset)
// and fall back to anyone else's. Returns uncorrelated=true if we couldn't
// match enough rows to cover qty.
func closeCheckoutsForLine(tx core.App, lineRec, itemRec *core.Record, cartUserID string, qty int) (bool, error) {
	rows, err := candidateOpenRows(tx, lineRec, itemRec, cartUserID, qty)
	if err != nil {
		return false, err
	}

	deleted := 0
	for _, r := range rows {
		if deleted >= qty {
			break
		}
		if err := tx.Delete(r); err != nil {
			return false, fmt.Errorf("delete open_checkout %s: %w", r.Id, err)
		}
		deleted++
	}
	return deleted < qty, nil
}

func candidateOpenRows(tx core.App, lineRec, itemRec *core.Record, cartUserID string, qty int) ([]*core.Record, error) {
	if itemRec.GetString("tracking_mode") == "serialized" {
		// At most one row possible.
		rows, err := tx.FindRecordsByFilter("open_checkouts",
			"item = {:item}", "", 1, 0,
			dbx.Params{"item": itemRec.Id})
		if err != nil {
			return nil, fmt.Errorf("find serialized open row: %w", err)
		}
		return rows, nil
	}

	target := lineRec.GetString("original_checkout_user")
	if target == "" {
		target = cartUserID
	}

	rows, err := tx.FindRecordsByFilter("open_checkouts",
		"item = {:item} && user = {:user}",
		"checked_out_at", qty, 0,
		dbx.Params{"item": itemRec.Id, "user": target})
	if err != nil {
		return nil, fmt.Errorf("find open rows for target user: %w", err)
	}
	if len(rows) >= qty {
		return rows, nil
	}

	// Fallback: anyone else with that item out.
	need := qty - len(rows)
	more, err := tx.FindRecordsByFilter("open_checkouts",
		"item = {:item} && user != {:user}",
		"checked_out_at", need, 0,
		dbx.Params{"item": itemRec.Id, "user": target})
	if err != nil {
		return nil, fmt.Errorf("find open rows fallback: %w", err)
	}
	return append(rows, more...), nil
}
