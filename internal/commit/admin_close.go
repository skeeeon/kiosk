package commit

import (
	"errors"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// SourceLocal / SourceController disambiguate who initiated a close so the
// audit trail can distinguish "kiosk-local admin in the touchscreen UI" from
// "central controller forwarded a command over NATS". Mirrors the constants
// in handlers.SourceLocal / SourceController for symmetry — duplicated here
// because internal/commit must not depend on internal/handlers.
const (
	SourceLocal      = "local"
	SourceController = "controller"
)

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

// reasonAffectsInventory reports whether the close should also decrement
// items.quantity_on_hand and (for serialized items) decommission the
// instance. Lost and damaged both mean "this unit is gone from inventory"
// — the difference between them is audit categorization, not effect.
// returned_offline means the unit is back in inventory (no qty change).
// other is conservative: admin can do a separate stock adjustment if the
// situation actually warrants one.
func reasonAffectsInventory(reason string) bool {
	return reason == "lost" || reason == "damaged"
}

// errAdminCloseIdempotentReplay is the sentinel returned from inside the
// txn callback when a duplicate command_id is detected. The outer wrapper
// catches it, rolls back the (empty) txn, and returns the prior result.
var errAdminCloseIdempotentReplay = errors.New("admin_close idempotent replay")

// AdminCloseInput packages the values a single admin force-close needs.
// Carrying them as a struct keeps the public signature small and survives
// when fields are added (callers don't have to update positional args).
type AdminCloseInput struct {
	OpenCheckoutID string

	// ActorID is the admin who initiated this close. For SourceLocal it's a
	// PB record ID in the kiosk's admins collection; for SourceController
	// it's the controller admin's PB record ID (which doesn't exist in the
	// kiosk's DB, so we store it as plain text in controller_admin_id-ish
	// fields and leave the FK admin column null).
	ActorID string

	// Source must be SourceLocal or SourceController. Determines which actor
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
		return nil, errors.New("open_checkout_id is required")
	}
	if in.ActorID == "" {
		return nil, errors.New("actor id is required")
	}
	if in.Source != SourceLocal && in.Source != SourceController {
		return nil, fmt.Errorf("invalid source %q", in.Source)
	}
	if in.Source == SourceController && in.CommandID == "" {
		return nil, errors.New("command_id is required for controller-source closes")
	}
	if _, ok := validClosureReasons[in.Reason]; !ok {
		return nil, fmt.Errorf("invalid closure_reason %q", in.Reason)
	}
	if in.Identity.KioskCode == "" || in.Identity.LocationCode == "" {
		return nil, errors.New("kiosk identity is not set")
	}
	if publish == nil {
		// Default to a no-op so callers in tests can pass nil. Production
		// always wires events.Publish.
		publish = func(string, any) {}
	}

	completedAt := time.Now().UTC()

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
		// Fast-path idempotency: a remote command_id that's already produced
		// a transaction returns the prior result without re-doing the work.
		if in.CommandID != "" {
			if prior, perr := findPriorAdminClose(tx, in.CommandID); perr != nil {
				return perr
			} else if prior != nil {
				out = *prior
				return errAdminCloseIdempotentReplay
			}
		}

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
		if in.Source == SourceLocal {
			txRec.Set("closed_by_admin", in.ActorID)
		}
		if in.CommandID != "" {
			txRec.Set("command_id", in.CommandID)
		}
		if err := tx.Save(txRec); err != nil {
			if in.CommandID != "" && dberr.IsUniqueViolation(err) {
				return errAdminCloseIdempotentReplay
			}
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
		if in.Source == SourceLocal {
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

		// Inventory side-effect for lost / damaged: decrement qty, write a
		// stock_adjustments row, and (for serialized items) flip the
		// instance to active=false + write an instance_audit row. All in
		// the same DB transaction so a partial state is impossible. Events
		// publish post-commit alongside the close event.
		if reasonAffectsInventory(in.Reason) {
			adjID, prevQty, newQty, perr := applyLostOrDamagedQtyAdjust(tx, in, itemRec, completedAt)
			if perr != nil {
				return perr
			}
			pending = append(pending, pendingEvent{
				Subject: events.InventoryAdjustSubject(in.Identity.KioskCode),
				Payload: buildAdminCloseInventoryAdjustPayload(in, itemRec, adjID,
					prevQty, newQty, completedAt),
			})

			if instanceID != "" {
				prevActive, derr := decommissionInstanceForAdminClose(tx, in,
					instanceID, itemID)
				if derr != nil {
					return derr
				}
				pending = append(pending, pendingEvent{
					Subject: events.InstanceLifecycleSubject(in.Identity.KioskCode),
					Payload: buildAdminCloseInstanceLifecyclePayload(in, itemRec,
						instanceID, prevActive, completedAt),
				})
			}
		}

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

		input := events.AdminCloseInput{
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
		case SourceLocal:
			input.AdminID = in.ActorID
		case SourceController:
			input.ControllerAdminID = in.ActorID
		}
		// Prepend the close event so consumers that care about ordering see
		// "close" before "inventory.adjust" / "instance.lifecycle" — the
		// close is logically the parent event and the others are its
		// consequences.
		pending = append([]pendingEvent{{
			Subject: events.AdminCloseSubject(in.Identity.KioskCode),
			Payload: events.BuildAdminClosePayload(input),
		}}, pending...)
		return nil
	})

	if errors.Is(err, errAdminCloseIdempotentReplay) {
		// Slow path may leave out unpopulated; refetch by command_id.
		if out.TransactionID == "" && in.CommandID != "" {
			refetch, ferr := fetchAdminCloseByCommandID(app, in.CommandID)
			if ferr != nil {
				return nil, ferr
			}
			out = *refetch
		}
		return &out, nil
	}
	if err != nil {
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
	case SourceLocal:
		adj.Set("admin", in.ActorID)
	case SourceController:
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

// decommissionInstanceForAdminClose flips item_instances.active to false
// and writes the matching instance_audit row inside the supplied
// transaction. Returns the previous active state so the caller can build
// the instance.lifecycle event with the prev/new flag pair.
func decommissionInstanceForAdminClose(tx core.App, in AdminCloseInput, instanceID, itemID string) (bool, error) {
	instRec, err := tx.FindRecordById("item_instances", instanceID)
	if err != nil {
		return false, fmt.Errorf("find item_instance %s: %w", instanceID, err)
	}
	prevActive := instRec.GetBool("active")
	if !prevActive {
		// Already decommissioned (rare — would mean someone retired the
		// instance manually but didn't close its open checkout). Don't
		// write a no-op audit row, but still report prevActive=false so
		// the event reflects reality.
		return false, nil
	}
	instRec.Set("active", false)
	if err := tx.Save(instRec); err != nil {
		return false, fmt.Errorf("set instance inactive: %w", err)
	}

	col, err := tx.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		return false, fmt.Errorf("find instance_audit collection: %w", err)
	}
	audit := core.NewRecord(col)
	audit.Set("item_instance", instanceID)
	audit.Set("item", itemID)
	audit.Set("action", "decommission")
	audit.Set("prev_active", true)
	audit.Set("new_active", false)
	audit.Set("reason", "admin_close: "+in.Reason)
	audit.Set("source", in.Source)
	switch in.Source {
	case SourceLocal:
		audit.Set("admin", in.ActorID)
	case SourceController:
		audit.Set("controller_admin_id", in.ActorID)
	}
	if err := tx.Save(audit); err != nil {
		return false, fmt.Errorf("save instance_audit: %w", err)
	}
	return true, nil
}

// buildAdminCloseInventoryAdjustPayload renders the inventory.adjust event
// payload using the same shape PerformStockAdjustment + PublishInventoryAdjustEvent
// emit, so the controller's existing aggregator picks these up without a
// new code path. Keys mirror EventPayload in internal/controller/consumer.go.
func buildAdminCloseInventoryAdjustPayload(in AdminCloseInput, itemRec *core.Record, adjID string, prevQty, newQty int, completedAt time.Time) map[string]any {
	payload := map[string]any{
		"adjustment_id": adjID,
		"kiosk_code":    in.Identity.KioskCode,
		"location_code": in.Identity.LocationCode,
		"item_id":       itemRec.Id,
		"item_code":     itemRec.GetString("code"),
		"item_name":     itemRec.GetString("name"),
		"mode":          "delta",
		"value":         -1,
		"delta":         -1,
		"prev_quantity": prevQty,
		"new_quantity":  newQty,
		"reason":        "admin_close: " + in.Reason,
		"source":        in.Source,
		"completed_at":  completedAt,
	}
	switch in.Source {
	case SourceLocal:
		payload["admin_id"] = in.ActorID
	case SourceController:
		payload["controller_admin_id"] = in.ActorID
	}
	return payload
}

// buildAdminCloseInstanceLifecyclePayload renders the instance.lifecycle
// event payload for the decommission caused by an admin close. Reuses the
// existing builder so the wire shape matches hook-driven events.
func buildAdminCloseInstanceLifecyclePayload(in AdminCloseInput, itemRec *core.Record, instanceID string, prevActive bool, completedAt time.Time) map[string]any {
	input := events.InstanceLifecycleInput{
		InstanceID:   instanceID,
		ItemID:       itemRec.Id,
		ItemCode:     itemRec.GetString("code"),
		ItemName:     itemRec.GetString("name"),
		KioskCode:    in.Identity.KioskCode,
		LocationCode: in.Identity.LocationCode,
		Action:       "decommission",
		PrevActive:   prevActive,
		NewActive:    false,
		Reason:       "admin_close: " + in.Reason,
		Source:       in.Source,
		CompletedAt:  completedAt,
	}
	switch in.Source {
	case SourceLocal:
		input.AdminID = in.ActorID
	case SourceController:
		input.ControllerAdminID = in.ActorID
	}
	return events.BuildInstanceLifecyclePayload(input)
}

// findPriorAdminClose returns the AdminCloseResult for an already-processed
// command_id, or nil if not found.
func findPriorAdminClose(tx core.App, commandID string) (*AdminCloseResult, error) {
	txRec, err := tx.FindFirstRecordByFilter("transactions",
		"command_id = {:c}", dbx.Params{"c": commandID})
	if err != nil {
		if dberr.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("idempotency lookup: %w", err)
	}
	return loadAdminCloseFromTx(tx, txRec)
}

// fetchAdminCloseByCommandID reads the result outside the rolled-back
// transaction in the slow-path race case (concurrent insert collision).
func fetchAdminCloseByCommandID(app core.App, commandID string) (*AdminCloseResult, error) {
	txRec, err := app.FindFirstRecordByFilter("transactions",
		"command_id = {:c}", dbx.Params{"c": commandID})
	if err != nil {
		return nil, fmt.Errorf("idempotent replay re-fetch: %w", err)
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
