package commit

import (
	"errors"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
	"github.com/skeeeon/kiosk/internal/events"
	inststatus "github.com/skeeeon/kiosk/internal/instances/status"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// ValidationError signals that the caller's AdminCloseInput is malformed —
// missing required fields, invalid enum values, unset identity. Handlers
// translate this to 400 Bad Request via errors.As. Internal/DB errors keep
// surfacing as plain `error` so they map to 500.
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

func validationErrorf(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// validClosureReasons mirrors the select-enum on transaction_lines.closure_reason
// (see migrations/1789000000_admin_close.go). The list lives here too so the
// commit path rejects unknown reasons up front rather than letting a typo
// land in the ledger.
var validClosureReasons = map[string]struct{}{
	"lost":             {},
	"returned_offline": {},
	"damaged":          {},
	"other":            {},
}

// reasonAffectsInventory reports whether the close should also drop the item's
// inventory count: for quantity-tracked items by decrementing
// items.quantity_on_hand, for serialized items by decommissioning the instance
// (whose active count the quantity is derived from). Lost and damaged both mean
// "this unit is gone from inventory" — the difference between them is audit
// categorization, not effect.
// returned_offline means the unit is back in inventory (no qty change).
// other is conservative: admin can do a separate stock adjustment if the
// situation actually warrants one.
func reasonAffectsInventory(reason string) bool {
	return reason == "lost" || reason == "damaged"
}

// AdminCloseInput packages the values a single admin force-close needs.
// Carrying them as a struct keeps the public signature small and survives
// when fields are added (callers don't have to update positional args).
type AdminCloseInput struct {
	OpenCheckoutID string

	// ActorID is the admin who initiated this close. For events.SourceLocal it's a
	// PB record ID in the kiosk's admins collection; for events.SourceController
	// it's the controller admin's PB record ID (which doesn't exist in the
	// kiosk's DB, so we store it as plain text in controller_admin_id-ish
	// fields and leave the FK admin column null).
	ActorID string

	// Source must be events.SourceLocal or events.SourceController. Determines which actor
	// column on transactions/transaction_lines we populate.
	Source string

	// CommandID is the idempotency anchor for controller-forwarded closes.
	// Empty for local calls; non-empty values are unique-indexed on
	// transactions and catch a duplicate-replay via the unique-violation
	// dance below (mirrors PerformStockAdjustment).
	CommandID string

	// Reason is one of validClosureReasons. Required.
	Reason string

	// Notes is free-text audit detail. Optional.
	Notes string

	// Identity stamps the originating kiosk on the new transaction. Passed
	// in (not read from kioskctx global) so callers can be tested without
	// process-global state.
	Identity kioskctx.Identity
}

// AdminCloseResult is what the endpoint and command handler return after a
// successful close. The shape mirrors commit.Result's spirit (one IDs +
// counters block) without conflating the worker-driven commit pipeline.
type AdminCloseResult struct {
	TransactionID  string `json:"transaction_id"`
	LineID         string `json:"line_id"`
	OpenCheckoutID string `json:"open_checkout_id"`
	ItemID         string `json:"item_id"`
	ItemCode       string `json:"item_code"`
	UserID         string `json:"user_id"`
	UserCode       string `json:"user_code"`
	ClosureReason  string `json:"closure_reason"`
}

// AdminClose closes a single open_checkouts row administratively. The
// invariant "every ledger state change is recorded as a transaction" stays
// intact: this writes one transactions row + one transaction_lines row
// (action="admin_close") and deletes the open_checkouts row, all in one
// DB transaction. After commit, a checkout.admin_close event is published.
//
// The trust invariant on original_checkout_user is preserved: the affected
// worker is resolved server-side from the open_checkouts row, never from
// the caller's input.
func AdminClose(app core.App, in AdminCloseInput, publish PublishFunc) (*AdminCloseResult, error) {
	if in.OpenCheckoutID == "" {
		return nil, validationErrorf("open_checkout_id is required")
	}
	if in.ActorID == "" {
		return nil, validationErrorf("actor id is required")
	}
	if in.Source != events.SourceLocal && in.Source != events.SourceController {
		return nil, validationErrorf("invalid source %q", in.Source)
	}
	if in.Source == events.SourceController && in.CommandID == "" {
		return nil, validationErrorf("command_id is required for controller-source closes")
	}
	if _, ok := validClosureReasons[in.Reason]; !ok {
		return nil, validationErrorf("invalid closure_reason %q", in.Reason)
	}
	if in.Identity.KioskCode == "" || in.Identity.LocationCode == "" {
		return nil, validationErrorf("kiosk identity is not set")
	}
	if publish == nil {
		// Default to a no-op so callers in tests can pass nil. Production
		// always wires events.Publish.
		publish = func(string, any) {}
	}

	completedAt := time.Now().UTC()

	// Fast-path idempotency: a remote command_id that's already produced a
	// transaction returns the prior result without entering the txn at all.
	// The slow path (unique-violation on save) is handled after the txn.
	if in.CommandID != "" {
		if prior, err := findAdminCloseByCommandID(app, in.CommandID); err != nil {
			return nil, err
		} else if prior != nil {
			return prior, nil
		}
	}

	// pendingEvent queues post-commit publishes so we don't fire events for
	// a rolled-back transaction. We accumulate inside the txn callback and
	// drain after RunInTransaction returns nil.
	type pendingEvent struct {
		Subject string
		Payload map[string]any
	}

	var (
		out     AdminCloseResult
		pending []pendingEvent
	)

	err := app.RunInTransaction(func(tx core.App) error {
		openRec, err := tx.FindRecordById("open_checkouts", in.OpenCheckoutID)
		if err != nil {
			return fmt.Errorf("find open_checkout %s: %w", in.OpenCheckoutID, err)
		}

		itemID := openRec.GetString("item")
		userID := openRec.GetString("user")
		instanceID := openRec.GetString("item_instance")
		serial := openRec.GetString("serial")

		itemRec, err := tx.FindRecordById("items", itemID)
		if err != nil {
			return fmt.Errorf("find item %s: %w", itemID, err)
		}
		userRec, err := tx.FindRecordById("users", userID)
		if err != nil {
			return fmt.Errorf("find user %s: %w", userID, err)
		}

		userGroupCode := ""
		if gID := userRec.GetString("group"); gID != "" {
			if g, gerr := tx.FindRecordById("groups", gID); gerr == nil {
				userGroupCode = g.GetString("code")
			}
		}

		txCol, err := tx.FindCollectionByNameOrId("transactions")
		if err != nil {
			return fmt.Errorf("find transactions collection: %w", err)
		}
		linesCol, err := tx.FindCollectionByNameOrId("transaction_lines")
		if err != nil {
			return fmt.Errorf("find transaction_lines collection: %w", err)
		}

		txRec := core.NewRecord(txCol)
		txRec.Set("kiosk_code", in.Identity.KioskCode)
		txRec.Set("location_code", in.Identity.LocationCode)
		txRec.Set("user", userID)
		txRec.Set("user_group", userGroupCode)
		txRec.Set("started_at", completedAt)
		txRec.Set("completed_at", completedAt)
		txRec.Set("status", "completed")
		txRec.Set("lines_count", 1)
		if in.Source == events.SourceLocal {
			txRec.Set("closed_by_admin", in.ActorID)
		}
		if in.CommandID != "" {
			txRec.Set("command_id", in.CommandID)
		}
		if err := tx.Save(txRec); err != nil {
			return fmt.Errorf("save transaction: %w", err)
		}

		lineRec := core.NewRecord(linesCol)
		lineRec.Set("transaction", txRec.Id)
		lineRec.Set("item", itemID)
		lineRec.Set("action", "admin_close")
		lineRec.Set("qty", 1)
		if serial != "" {
			lineRec.Set("serial", serial)
		}
		if instanceID != "" {
			lineRec.Set("item_instance", instanceID)
		}
		// original_checkout_user mirrors the return-path convention: the
		// line records whose checkout this row closed, even when the
		// parent transaction.user is the same value (admin_close always
		// closes one worker's row).
		lineRec.Set("original_checkout_user", userID)
		if in.Source == events.SourceLocal {
			lineRec.Set("closed_by_admin", in.ActorID)
		}
		lineRec.Set("closure_reason", in.Reason)
		if in.Notes != "" {
			lineRec.Set("notes", in.Notes)
		}
		if err := tx.Save(lineRec); err != nil {
			return fmt.Errorf("save transaction_line: %w", err)
		}

		if err := tx.Delete(openRec); err != nil {
			return fmt.Errorf("delete open_checkout %s: %w", openRec.Id, err)
		}

		// Inventory side-effect for lost / damaged: the unit is gone from
		// inventory. How the count drops depends on tracking mode:
		//
		//   - quantity-tracked: decrement quantity_on_hand by 1 + write a
		//     stock_adjustments row + emit inventory.adjust (handled below).
		//   - serialized: quantity_on_hand is DERIVED from the non-retired
		//     instance count, so we only retire the instance (status→retired).
		//     The instances after-update hook recomputes the item's quantity
		//     off the back of that write — no stock_adjustments row, no
		//     inventory.adjust event. The instance.lifecycle event is the
		//     record of the count change.
		//
		// All writes ride this DB transaction so a partial state is impossible.
		// Events publish post-commit alongside the close event.
		out = AdminCloseResult{
			TransactionID:  txRec.Id,
			LineID:         lineRec.Id,
			OpenCheckoutID: in.OpenCheckoutID,
			ItemID:         itemID,
			ItemCode:       itemRec.GetString("code"),
			UserID:         userID,
			UserCode:       userRec.GetString("code"),
			ClosureReason:  in.Reason,
		}

		closeInput := events.AdminCloseInput{
			TransactionID:  txRec.Id,
			LineID:         lineRec.Id,
			KioskCode:      in.Identity.KioskCode,
			LocationCode:   in.Identity.LocationCode,
			OpenCheckoutID: in.OpenCheckoutID,
			ItemID:         itemID,
			ItemCode:       itemRec.GetString("code"),
			ItemName:       itemRec.GetString("name"),
			UserID:         userID,
			UserCode:       userRec.GetString("code"),
			UserGroup:      userGroupCode,
			ItemInstanceID: instanceID,
			Serial:         serial,
			Qty:            1,
			ClosureReason:  in.Reason,
			Notes:          in.Notes,
			Source:         in.Source,
			CommandID:      in.CommandID,
			CompletedAt:    completedAt,
		}
		switch in.Source {
		case events.SourceLocal:
			closeInput.AdminID = in.ActorID
		case events.SourceController:
			closeInput.ControllerAdminID = in.ActorID
		}
		// Append the close event first so it leads the publish order — it's
		// the parent event; the qty + lifecycle side-effects below are its
		// consequences. NATS doesn't promise cross-subject order to
		// downstream consumers, but matching the publish order to the
		// causal order keeps logs (and tests) readable.
		pending = append(pending, pendingEvent{
			Subject: events.AdminCloseSubject(in.Identity.KioskCode),
			Payload: events.BuildAdminClosePayload(closeInput),
		})

		if reasonAffectsInventory(in.Reason) {
			if itemRec.GetString("tracking_mode") == "serialized" {
				// Serialized: retire the instance only. The quantity drop is
				// derived from the non-retired count via the instances
				// recompute hook; no stock_adjustments row / inventory.adjust
				// event is emitted for serialized items. Routed through the
				// shared SetStatusInTx writer so a controller-forwarded close
				// and a local close produce an identical audit + lifecycle
				// trail.
				if instanceID != "" {
					setIn := inststatus.SetStatusInput{
						InstanceID: instanceID,
						ItemID:     itemID,
						Target:     inststatus.StatusRetired,
						Reason:     "admin_close: " + in.Reason,
						Source:     in.Source,
					}
					switch in.Source {
					case events.SourceLocal:
						setIn.AdminID = in.ActorID
					case events.SourceController:
						setIn.ControllerAdminID = in.ActorID
					}
					prevStatus, auditID, derr := inststatus.SetStatusInTx(tx, setIn)
					if derr != nil {
						return derr
					}
					// auditID is empty when the instance was already retired —
					// a no-op that writes nothing and emits no event.
					if auditID != "" {
						pending = append(pending, pendingEvent{
							Subject: events.InstanceLifecycleSubject(in.Identity.KioskCode),
							Payload: buildAdminCloseInstanceLifecyclePayload(in, itemRec,
								instanceID, auditID, prevStatus, completedAt),
						})
					}
				}
			} else {
				adjID, prevQty, newQty, perr := applyLostOrDamagedQtyAdjust(tx, in, itemRec, completedAt)
				if perr != nil {
					return perr
				}
				pending = append(pending, pendingEvent{
					Subject: events.InventoryAdjustSubject(in.Identity.KioskCode),
					Payload: buildAdminCloseInventoryAdjustPayload(in, itemRec, adjID,
						prevQty, newQty, completedAt),
				})
			}
		}
		return nil
	})

	if err != nil {
		// Slow-path idempotency: a concurrent inserter beat us to the
		// command_id. Two flavors share this path:
		//   - unique-violation on transactions.command_id: we got past
		//     the open_checkout lookup and lost the race at save time.
		//   - not-found on the open_checkout lookup: the winner already
		//     deleted the row before we entered the txn. (Fast-path
		//     check at the top of AdminClose can lose to a winner that
		//     hadn't committed yet.)
		// In either case, re-lookup outside the rolled-back txn and
		// return the winner if it exists. If no prior exists for this
		// command_id, the not-found was a real typo'd id (or some other
		// genuine missing-row error inside the txn) and we surface the
		// original error unchanged.
		if in.CommandID != "" && (dberr.IsUniqueViolation(err) || dberr.IsNotFound(err)) {
			prior, ferr := findAdminCloseByCommandID(app, in.CommandID)
			if ferr != nil {
				return nil, ferr
			}
			if prior != nil {
				return prior, nil
			}
		}
		return nil, err
	}

	for _, ev := range pending {
		publish(ev.Subject, ev.Payload)
	}
	return &out, nil
}

// applyLostOrDamagedQtyAdjust decrements items.quantity_on_hand by 1 and
// writes a stock_adjustments audit row inside the supplied transaction.
// Returns the adjustment id + the prev/new quantity so the caller can
// build the inventory.adjust event payload. The reason text composes
// "admin_close: <reason>" so the existing stock_adjustments view shows
// where the drop came from without needing a join.
func applyLostOrDamagedQtyAdjust(tx core.App, in AdminCloseInput, itemRec *core.Record, completedAt time.Time) (string, int, int, error) {
	prevQty := itemRec.GetInt("quantity_on_hand")
	newQty := prevQty - 1
	itemRec.Set("quantity_on_hand", newQty)
	if err := tx.Save(itemRec); err != nil {
		return "", 0, 0, fmt.Errorf("decrement quantity_on_hand: %w", err)
	}

	col, err := tx.FindCollectionByNameOrId("stock_adjustments")
	if err != nil {
		return "", 0, 0, fmt.Errorf("find stock_adjustments collection: %w", err)
	}
	adj := core.NewRecord(col)
	adj.Set("item", itemRec.Id)
	adj.Set("delta", -1)
	adj.Set("new_quantity", newQty)
	adj.Set("reason", "admin_close: "+in.Reason)
	adj.Set("source", in.Source)
	switch in.Source {
	case events.SourceLocal:
		adj.Set("admin", in.ActorID)
	case events.SourceController:
		adj.Set("controller_admin_id", in.ActorID)
	}
	// command_id intentionally NOT propagated: the unique-when-non-empty
	// index on stock_adjustments.command_id is for inventory.adjust command
	// idempotency. An admin_close that happens to share the same command_id
	// would then conflict on the stock_adjustments row. The outer
	// transactions.command_id check has already caught duplicate replays;
	// stock_adjustments doesn't need its own idempotency anchor here.
	if err := tx.Save(adj); err != nil {
		return "", 0, 0, fmt.Errorf("save stock_adjustment: %w", err)
	}
	return adj.Id, prevQty, newQty, nil
}

// buildAdminCloseInventoryAdjustPayload renders the inventory.adjust event
// payload using the same builder PerformStockAdjustment + PublishInventoryAdjustEvent
// use, so the controller's existing aggregator picks these up without a
// new code path.
func buildAdminCloseInventoryAdjustPayload(in AdminCloseInput, itemRec *core.Record, adjID string, prevQty, newQty int, completedAt time.Time) map[string]any {
	input := events.InventoryAdjustInput{
		AdjustmentID: adjID,
		KioskCode:    in.Identity.KioskCode,
		LocationCode: in.Identity.LocationCode,
		ItemID:       itemRec.Id,
		ItemCode:     itemRec.GetString("code"),
		ItemName:     itemRec.GetString("name"),
		Mode:         "delta",
		Value:        -1,
		Delta:        -1,
		PrevQuantity: prevQty,
		NewQuantity:  newQty,
		Reason:       "admin_close: " + in.Reason,
		Source:       in.Source,
		CompletedAt:  completedAt,
	}
	switch in.Source {
	case events.SourceLocal:
		input.AdminID = in.ActorID
	case events.SourceController:
		input.ControllerAdminID = in.ActorID
	}
	return events.BuildInventoryAdjustPayload(input)
}

// buildAdminCloseInstanceLifecyclePayload renders the instance.lifecycle
// event payload for the retire caused by an admin close. Reuses the existing
// builder so the wire shape matches hook-driven events. auditID is the
// kiosk-side instance_audit row id that backs this event; threaded through as
// SourceAuditID so the controller projection is idempotent.
func buildAdminCloseInstanceLifecyclePayload(in AdminCloseInput, itemRec *core.Record, instanceID, auditID, prevStatus string, completedAt time.Time) map[string]any {
	input := events.InstanceLifecycleInput{
		InstanceID:    instanceID,
		ItemID:        itemRec.Id,
		ItemCode:      itemRec.GetString("code"),
		ItemName:      itemRec.GetString("name"),
		KioskCode:     in.Identity.KioskCode,
		LocationCode:  in.Identity.LocationCode,
		Action:        inststatus.ActionRetire,
		PrevStatus:    prevStatus,
		NewStatus:     inststatus.StatusRetired,
		Reason:        "admin_close: " + in.Reason,
		Source:        in.Source,
		SourceAuditID: auditID,
		CompletedAt:   completedAt,
	}
	switch in.Source {
	case events.SourceLocal:
		input.AdminID = in.ActorID
	case events.SourceController:
		input.ControllerAdminID = in.ActorID
	}
	return events.BuildInstanceLifecyclePayload(input)
}

// findAdminCloseByCommandID returns the AdminCloseResult for an
// already-processed command_id, or (nil, nil) if no transaction is stamped
// with that id yet. Callers from the fast path (pre-txn) and the slow path
// (post-unique-violation re-lookup) both use this — there's no behavioral
// difference between the two, only timing.
func findAdminCloseByCommandID(app core.App, commandID string) (*AdminCloseResult, error) {
	txRec, err := app.FindFirstRecordByFilter("transactions",
		"command_id = {:c}", dbx.Params{"c": commandID})
	if err != nil {
		if dberr.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("idempotency lookup: %w", err)
	}
	return loadAdminCloseFromTx(app, txRec)
}

// OpenCheckoutIDForLine resolves a transaction_lines.id to the id of one
// open_checkouts row that still references it. For serialized items there
// is at most one such row; for non-serialized qty>1 there are multiple
// fungible rows and we return the oldest by checked_out_at. Both the
// kiosk-local HTTP handler and the controller-forward command handler use
// this so SPA callers can identify rows by line id (the natural key on
// both sides) rather than by kiosk-side open_checkouts.id (which the
// controller has no view of).
func OpenCheckoutIDForLine(app core.App, lineID string) (string, error) {
	if lineID == "" {
		return "", errors.New("transaction_line_id is required")
	}
	rows, err := app.FindRecordsByFilter("open_checkouts",
		"transaction_line = {:l}", "checked_out_at", 1, 0,
		dbx.Params{"l": lineID})
	if err != nil {
		if dberr.IsNotFound(err) {
			return "", errors.New("no open_checkout for transaction_line — may already be closed")
		}
		return "", fmt.Errorf("open_checkout lookup: %w", err)
	}
	if len(rows) == 0 {
		return "", errors.New("no open_checkout for transaction_line — may already be closed")
	}
	return rows[0].Id, nil
}

// loadAdminCloseFromTx hydrates an AdminCloseResult from a stored
// transactions row. Used by both the fast-path and slow-path replay branches.
func loadAdminCloseFromTx(app core.App, txRec *core.Record) (*AdminCloseResult, error) {
	lineRec, err := app.FindFirstRecordByFilter("transaction_lines",
		"transaction = {:t} && action = 'admin_close'",
		dbx.Params{"t": txRec.Id})
	if err != nil {
		return nil, fmt.Errorf("find prior admin_close line: %w", err)
	}
	itemID := lineRec.GetString("item")
	userID := lineRec.GetString("original_checkout_user")
	itemCode := ""
	if it, ierr := app.FindRecordById("items", itemID); ierr == nil {
		itemCode = it.GetString("code")
	}
	userCode := ""
	if u, uerr := app.FindRecordById("users", userID); uerr == nil {
		userCode = u.GetString("code")
	}
	return &AdminCloseResult{
		TransactionID: txRec.Id,
		LineID:        lineRec.Id,
		ItemID:        itemID,
		ItemCode:      itemCode,
		UserID:        userID,
		UserCode:      userCode,
		ClosureReason: lineRec.GetString("closure_reason"),
	}, nil
}
