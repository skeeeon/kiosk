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
	"github.com/skeeeon/kiosk/internal/events"
	inststatus "github.com/skeeeon/kiosk/internal/instances/status"
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
	// MaintenanceEntered lists the serialized units this transaction routed
	// into maintenance status on return (per-SKU opt-in or a per-line flag).
	// The post-commit handler fires one batched maintenance notification from
	// it; the SPA can also surface "N units sent to maintenance".
	MaintenanceEntered []MaintenanceEntry `json:"maintenance_entered,omitempty"`
}

// MaintenanceEntry is one serialized unit that entered maintenance as part of
// a return, carrying the fields the maintenance notification + SPA toast need.
type MaintenanceEntry struct {
	InstanceID   string `json:"instance_id"`
	InstanceCode string `json:"instance_code"`
	ItemCode     string `json:"item_code"`
	ItemName     string `json:"item_name"`
	Serial       string `json:"serial,omitempty"`
	Reason       string `json:"reason"`
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
		maintEvents []lineEvent // instance.lifecycle events for maintenance-on-return
		txCompleted lineEvent   // payload for the transaction.complete event
	)

	err := app.RunInTransaction(func(tx core.App) error {
		userRec, err := tx.FindRecordById("users", c.UserID)
		if err != nil {
			return fmt.Errorf("find user %s: %w", c.UserID, err)
		}
		cartUserRole := userRec.GetString("role")
		cartUserGroupID := userRec.GetString("group")
		cartUserGroupCode := ""
		if cartUserGroupID != "" {
			g, err := tx.FindRecordById("groups", cartUserGroupID)
			if err == nil {
				cartUserGroupCode = g.GetString("code")
			}
		}

		txRec, err := createTransaction(tx, c, id, cartUserGroupCode, completedAt)
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
			if itemRec.GetString("tracking_mode") == "serialized" {
				if l.Qty != 1 {
					return fmt.Errorf("serialized item %s must have qty=1, got %d", itemRec.GetString("code"), l.Qty)
				}
				if l.ItemInstanceID == "" {
					return fmt.Errorf("serialized item %s requires an instance", itemRec.GetString("code"))
				}
			}
			// Any line carrying an instance must point at an instance whose
			// parent item matches the line. Catches client bugs / tampering.
			if l.ItemInstanceID != "" {
				inst, err := tx.FindRecordById("item_instances", l.ItemInstanceID)
				if err != nil {
					return fmt.Errorf("find instance %s: %w", l.ItemInstanceID, err)
				}
				if inst.GetString("item") != l.ItemID {
					return fmt.Errorf("instance %s does not belong to item %s", l.ItemInstanceID, itemRec.GetString("code"))
				}
			}

			// Serialized returns target a specific instance whose open
			// checkout may belong to another worker. The cross-user gate
			// below keys on OriginalCheckoutUserID, but the ordinary
			// cart-write paths never set it for a plain serialized scan (a
			// worker scanning someone else's tool defaults to checkout, then
			// may flip the line to return). Resolve the true holder here,
			// server-side, and overwrite whatever the client supplied — this
			// field must never be client-trusted. Without this, a non-foreman
			// could silently close another worker's serialized checkout and
			// the ledger would misattribute the return. commit is the trust
			// boundary, so the resolution lives here, before createLine stamps
			// the line and before the gate fires.
			if l.Action == "return" && itemRec.GetString("tracking_mode") == "serialized" && l.ItemInstanceID != "" {
				holder, err := serializedOpenRowHolder(tx, l.ItemInstanceID)
				if err != nil {
					return err
				}
				if holder != "" && holder != c.UserID {
					l.OriginalCheckoutUserID = holder
				} else {
					// Self-return or no open row — never carry a cross-user
					// marker the holder doesn't justify.
					l.OriginalCheckoutUserID = ""
				}
			}

			lineRec, err := createLine(tx, linesCol, txRec, l)
			if err != nil {
				return err
			}

			switch l.Action {
			case "checkout":
				if err := openCheckoutsForLine(tx, openCol, l, lineRec, itemRec, c.UserID, completedAt, l.Qty); err != nil {
					return err
				}
				result.CheckedOut++

			case "return":
				// Cross-user returns: only a foreman acting within their own
				// non-empty group can do this. Pre-flight AllowCrossUser kill
				// switch above already short-circuited the strict-policy case.
				// OriginalCheckoutUserID is server-resolved: by CartForemanReturn
				// for non-serialized, and just above (serializedOpenRowHolder)
				// for serialized — so this gate fires for every cross-user
				// return regardless of what the client sent.
				if l.OriginalCheckoutUserID != "" && l.OriginalCheckoutUserID != c.UserID {
					if !policy.AllowCrossUser {
						// Serialized cross-user returns resolve their holder
						// inside this loop, after the pre-flight kill switch ran
						// on the (empty) client value — so re-check the policy
						// here to keep a strict deny authoritative for them too.
						return fmt.Errorf("item %s is checked out to another worker; cross-user returns are not allowed", itemRec.GetString("code"))
					}
					if cartUserRole != "foreman" {
						return fmt.Errorf("item %s is checked out to another worker; only a foreman can return it", itemRec.GetString("code"))
					}
					if cartUserGroupID == "" {
						return fmt.Errorf("foreman %s has no group set; cross-user returns require a group", c.UserCode)
					}
					origUser, err := tx.FindRecordById("users", l.OriginalCheckoutUserID)
					if err != nil {
						return fmt.Errorf("find original checkout user %s: %w", l.OriginalCheckoutUserID, err)
					}
					if origUser.GetString("group") != cartUserGroupID {
						return fmt.Errorf("item %s is checked out to a worker in a different group; an admin must handle cross-group returns", itemRec.GetString("code"))
					}
				}

				uncorrelated, err := closeCheckoutsForLine(tx, l, lineRec, itemRec, c.UserID, l.Qty)
				if err != nil {
					return err
				}
				if uncorrelated {
					if !policy.AllowUncorrelated {
						return fmt.Errorf("return of %s does not match any open checkout; uncorrelated returns are not allowed", itemRec.GetString("code"))
					}
					if cartUserRole != "foreman" {
						return fmt.Errorf("return of %s does not match any open checkout; only a foreman can record an uncorrelated return", itemRec.GetString("code"))
					}
					lineRec.Set("uncorrelated", true)
					if err := tx.Save(lineRec); err != nil {
						return fmt.Errorf("mark line uncorrelated: %w", err)
					}
				}
				result.Returned++

				// Maintenance-on-return: a returned serialized unit lands in
				// maintenance (not back to in_service) when its SKU opts in
				// (requires_maintenance_on_return) or the worker flagged the
				// line. The open_checkouts row was already deleted above — the
				// tool IS back; maintenance is a status on the instance, set
				// through the shared SetStatusInTx writer so the audit +
				// quantity recompute + lifecycle event match every other status
				// change. SetStatusInTx is a no-op (auditID empty) if the unit
				// is somehow already in maintenance, so a doubled flag can't
				// double-fire.
				if itemRec.GetString("tracking_mode") == "serialized" && l.ItemInstanceID != "" &&
					(itemRec.GetBool("requires_maintenance_on_return") || l.RequestMaintenance) {
					reason := "flagged on return"
					if itemRec.GetBool("requires_maintenance_on_return") {
						reason = "auto: SKU requires maintenance on return"
					}
					prevStatus, auditID, merr := inststatus.SetStatusInTx(tx, inststatus.SetStatusInput{
						InstanceID: l.ItemInstanceID,
						ItemID:     l.ItemID,
						Target:     inststatus.StatusMaintenance,
						Reason:     reason,
						Source:     events.SourceLocal,
					})
					if merr != nil {
						return merr
					}
					if auditID != "" {
						maintEvents = append(maintEvents, lineEvent{
							Subject: events.InstanceLifecycleSubject(id.KioskCode),
							Payload: events.BuildInstanceLifecyclePayload(events.InstanceLifecycleInput{
								InstanceID:    l.ItemInstanceID,
								InstanceCode:  l.ItemInstanceCode,
								ItemID:        itemRec.Id,
								ItemCode:      itemRec.GetString("code"),
								ItemName:      itemRec.GetString("name"),
								KioskCode:     id.KioskCode,
								LocationCode:  id.LocationCode,
								Action:        inststatus.ActionToMaintenance,
								PrevStatus:    prevStatus,
								NewStatus:     inststatus.StatusMaintenance,
								Reason:        reason,
								Source:        events.SourceLocal,
								SourceAuditID: auditID,
								CompletedAt:   completedAt,
							}),
						})
						result.MaintenanceEntered = append(result.MaintenanceEntered, MaintenanceEntry{
							InstanceID:   l.ItemInstanceID,
							InstanceCode: l.ItemInstanceCode,
							ItemCode:     itemRec.GetString("code"),
							ItemName:     itemRec.GetString("name"),
							Serial:       l.Serial,
							Reason:       reason,
						})
					}
				}

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

			// Resolve the original holder's user code for the event payload
			// when this line carries OriginalCheckoutUserID — the controller
			// needs the code (its user IDs differ from ours) to populate the
			// projected original_checkout_user FK. Same-user returns reuse
			// c.UserCode without a second lookup.
			var origUserCode string
			if l.OriginalCheckoutUserID != "" {
				if l.OriginalCheckoutUserID == c.UserID {
					origUserCode = c.UserCode
				} else {
					u, err := tx.FindRecordById("users", l.OriginalCheckoutUserID)
					if err != nil {
						return fmt.Errorf("find original checkout user %s: %w", l.OriginalCheckoutUserID, err)
					}
					origUserCode = u.GetString("code")
				}
			}

			lineEvents = append(lineEvents, lineEvent{
				Subject: events.ItemActionSubject(id.KioskCode, l.Action),
				Payload: events.BuildItemActionPayload(events.ItemActionInput{
					TransactionID:            txRec.Id,
					LineID:                   lineRec.Id,
					KioskCode:                id.KioskCode,
					LocationCode:             id.LocationCode,
					UserID:                   c.UserID,
					UserCode:                 c.UserCode,
					UserGroup:                cartUserGroupCode,
					ItemID:                   itemRec.Id,
					ItemCode:                 itemRec.GetString("code"),
					ItemName:                 itemRec.GetString("name"),
					Action:                   l.Action,
					Qty:                      l.Qty,
					Serial:                   l.Serial,
					Uncorrelated:             lineRec.GetBool("uncorrelated"),
					OriginalCheckoutUserCode: origUserCode,
					ItemInstanceID:           l.ItemInstanceID,
					CompletedAt:              completedAt,
				}),
			})
		}

		result.LinesCount = len(c.Lines)

		// Denormalize the line count onto the transaction so list views and
		// the CSV export can render it without an N+1 COUNT(*) per row.
		txRec.Set("lines_count", result.LinesCount)
		if err := tx.Save(txRec); err != nil {
			return fmt.Errorf("set lines_count on transaction: %w", err)
		}

		txCompleted = lineEvent{
			Subject: events.TransactionCompleteSubject(id.KioskCode),
			Payload: events.BuildTransactionCompletePayload(events.TransactionCompleteInput{
				TransactionID: txRec.Id,
				KioskCode:     id.KioskCode,
				LocationCode:  id.LocationCode,
				UserID:        c.UserID,
				UserCode:      c.UserCode,
				UserName:      userRec.GetString("name"),
				UserGroup:     cartUserGroupCode,
				StartedAt:     c.StartedAt,
				CompletedAt:   completedAt,
				LinesCount:    result.LinesCount,
				CheckedOut:    result.CheckedOut,
				Returned:      result.Returned,
				Consumed:      result.Consumed,
			}),
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
	for _, ev := range maintEvents {
		publish(ev.Subject, ev.Payload)
	}
	return &result, nil
}

type lineEvent struct {
	Subject string
	Payload map[string]any
}

func createTransaction(tx core.App, c *cart.Cart, id kioskctx.Identity, userGroupCode string, completedAt time.Time) (*core.Record, error) {
	col, err := tx.FindCollectionByNameOrId("transactions")
	if err != nil {
		return nil, fmt.Errorf("find transactions collection: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("kiosk_code", id.KioskCode)
	rec.Set("location_code", id.LocationCode)
	rec.Set("user", c.UserID)
	rec.Set("user_group", userGroupCode)
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
	if l.ItemInstanceID != "" {
		rec.Set("item_instance", l.ItemInstanceID)
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
// tools always have qty=1 and carry an item_instance FK (validated above);
// non-serialized may have qty>1 and carry no instance.
func openCheckoutsForLine(tx core.App, col *core.Collection, line *cart.Line, lineRec, itemRec *core.Record, userID string, completedAt time.Time, qty int) error {
	for i := 0; i < qty; i++ {
		rec := core.NewRecord(col)
		rec.Set("item", itemRec.Id)
		rec.Set("user", userID)
		if line.Serial != "" {
			rec.Set("serial", line.Serial)
		}
		if line.ItemInstanceID != "" {
			rec.Set("item_instance", line.ItemInstanceID)
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
// row is uniquely identified by the item_instance carried on the cart line;
// for non-serialized, only rows belonging to the line's
// original_checkout_user (or the cart user if unset) are eligible. A
// shortfall is surfaced as uncorrelated=true on the caller — we do NOT
// silently borrow rows from other users, since that would mask the audit
// signal that the return doesn't match recorded state.
func closeCheckoutsForLine(tx core.App, line *cart.Line, lineRec, itemRec *core.Record, cartUserID string, qty int) (bool, error) {
	rows, err := candidateOpenRows(tx, line, lineRec, itemRec, cartUserID, qty)
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

// serializedOpenRowHolder returns the user id currently holding the open
// checkout for the given serialized instance, or "" if none is open. There
// is at most one open row per instance. Used to resolve cross-user serialized
// returns at the trust boundary before the foreman gate runs.
func serializedOpenRowHolder(tx core.App, instanceID string) (string, error) {
	rows, err := tx.FindRecordsByFilter("open_checkouts",
		"item_instance = {:inst}", "", 1, 0,
		dbx.Params{"inst": instanceID})
	if err != nil {
		return "", fmt.Errorf("find serialized open row holder: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].GetString("user"), nil
}

func candidateOpenRows(tx core.App, line *cart.Line, lineRec, itemRec *core.Record, cartUserID string, qty int) ([]*core.Record, error) {
	if itemRec.GetString("tracking_mode") == "serialized" {
		// Returns of a serialized SKU target the exact instance scanned.
		// At most one open row possible per instance.
		rows, err := tx.FindRecordsByFilter("open_checkouts",
			"item_instance = {:inst}", "", 1, 0,
			dbx.Params{"inst": line.ItemInstanceID})
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
	return rows, nil
}
