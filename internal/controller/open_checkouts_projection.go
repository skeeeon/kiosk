// Open-checkouts projection. The controller's open_checkouts table mirrors
// the per-kiosk materialized view that internal/commit/commit.go maintains
// locally on each kiosk — recreated here from the same item.{action} +
// checkout.admin_close events so digests and per-kiosk reports can answer
// "what's currently out at kiosk X" without a full-ledger replay.
//
// The matching rules below are a deliberate mirror of
// commit.closeCheckoutsForLine / candidateOpenRows. Drift between the two
// would cause the controller's view to diverge from the kiosks' over time.
// Any change there needs a matching change here.
package controller

import (
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// projectOpenCheckoutsForItemAction is the dispatcher inside the item.{action}
// branch — runs after ProjectLine so the projected line FK is available for
// the open_checkouts row(s). Returns the worst outcome of the two stages.
func (a *Aggregator) projectOpenCheckoutsForItemAction(p EventPayload) projectOutcome {
	switch p.Action {
	case "checkout":
		return a.ProjectOpenCheckoutsInsert(p)
	case "return":
		return a.ProjectOpenCheckoutsClose(p)
	}
	// consume / admin_close (when it arrives via item.* — today it doesn't)
	// have no open_checkouts side effect.
	return projectAck
}

// ProjectOpenCheckoutsInsert mirrors commit.openCheckoutsForLine: for one
// item.checkout event with qty=N, insert N rows into open_checkouts.
//
// Idempotency: redelivery of the same checkout finds rows already present
// for the projected line FK and ack-skips. JetStream consumers run
// single-threaded, so the pre-check inside the transaction is sufficient
// — no compound unique index needed.
func (a *Aggregator) ProjectOpenCheckoutsInsert(p EventPayload) projectOutcome {
	if p.Qty < 1 {
		return projectAck
	}

	line, err := a.findLine(p.LineID)
	if err != nil {
		slog.Warn("controller.aggregator.oc_insert.line_lookup_failed", "error", err)
		return projectRetry
	}
	if line == nil {
		// Should not happen — ProjectLine ran successfully just before.
		// Treat as transient so JetStream redelivers.
		return projectRetry
	}

	item, err := a.findItemByCode(p.ItemCode)
	if err != nil {
		return projectRetry
	}
	if item == nil {
		// Catalog hasn't caught up. Drop, matching ProjectLine's posture.
		slog.Warn("controller.aggregator.oc_insert.unknown_item",
			"item_code", p.ItemCode, "kiosk_code", p.KioskCode)
		return projectAck
	}
	user, err := a.findUserByCode(p.UserCode)
	if err != nil {
		return projectRetry
	}
	if user == nil {
		slog.Warn("controller.aggregator.oc_insert.unknown_user",
			"user_code", p.UserCode, "kiosk_code", p.KioskCode)
		return projectAck
	}

	col, err := a.app.FindCollectionByNameOrId("open_checkouts")
	if err != nil {
		return projectRetry
	}

	txErr := a.app.RunInTransaction(func(tx core.App) error {
		existing, err := tx.FindRecordsByFilter("open_checkouts",
			"transaction_line = {:tl}", "", 1, 0,
			dbx.Params{"tl": line.Id})
		if err != nil {
			return fmt.Errorf("oc dedupe lookup: %w", err)
		}
		if len(existing) > 0 {
			return nil
		}
		for i := 0; i < p.Qty; i++ {
			rec := core.NewRecord(col)
			rec.Set("kiosk_code", p.KioskCode)
			rec.Set("item", item.Id)
			rec.Set("user", user.Id)
			if p.Serial != "" {
				rec.Set("serial", p.Serial)
			}
			// source_item_instance_id is the cross-binary identifier (the
			// kiosk's item_instances.id). We deliberately don't write the
			// item_instance RelationField — it points at the controller's
			// own (always-empty) item_instances collection and would fail
			// the FK constraint.
			if p.ItemInstanceID != "" {
				rec.Set("source_item_instance_id", p.ItemInstanceID)
			}
			rec.Set("checked_out_at", p.CompletedAt)
			rec.Set("transaction_line", line.Id)
			if err := tx.Save(rec); err != nil {
				return fmt.Errorf("save open_checkout: %w", err)
			}
		}
		return nil
	})
	if txErr != nil {
		slog.Warn("controller.aggregator.oc_insert.tx_failed", "error", txErr)
		return projectRetry
	}
	return projectAck
}

// ProjectOpenCheckoutsClose mirrors commit.closeCheckoutsForLine: delete up
// to qty rows on a return. Serialized lines target the exact item_instance;
// non-serialized prefer original_checkout_user_code, falling back to any
// other user, FIFO by checked_out_at, all scoped to this kiosk_code.
//
// Idempotency: redelivery deletes rows that no longer exist (a no-op).
// We don't attempt to recreate deleted rows on duplicate return events;
// the worst case is the second delete matches different rows, which would
// be a real correctness issue — but since JetStream delivers in-order and
// our consumer is single-threaded, the only way to hit this is if the
// kiosk emits the same return event twice, which is what the kiosk-side
// idempotency anchors prevent.
func (a *Aggregator) ProjectOpenCheckoutsClose(p EventPayload) projectOutcome {
	if p.Qty < 1 {
		return projectAck
	}

	txErr := a.app.RunInTransaction(func(tx core.App) error {
		rows, err := candidateOpenRowsControllerSide(tx, p)
		if err != nil {
			return err
		}
		deleted := 0
		for _, r := range rows {
			if deleted >= p.Qty {
				break
			}
			if err := tx.Delete(r); err != nil {
				return fmt.Errorf("delete open_checkout %s: %w", r.Id, err)
			}
			deleted++
		}
		return nil
	})
	if txErr != nil {
		slog.Warn("controller.aggregator.oc_close.tx_failed", "error", txErr)
		return projectRetry
	}
	return projectAck
}

// candidateOpenRowsControllerSide picks the open_checkouts rows a return
// event should close. Mirrors commit.candidateOpenRows with kiosk_code
// scoping added — the controller's table is fleet-wide, so every query
// pins kiosk_code first.
func candidateOpenRowsControllerSide(tx core.App, p EventPayload) ([]*core.Record, error) {
	// Serialized return: at most one row per instance, exact match.
	if p.ItemInstanceID != "" {
		rows, err := tx.FindRecordsByFilter("open_checkouts",
			"kiosk_code = {:k} && source_item_instance_id = {:inst}",
			"", 1, 0,
			dbx.Params{"k": p.KioskCode, "inst": p.ItemInstanceID})
		if err != nil {
			return nil, fmt.Errorf("find serialized open row: %w", err)
		}
		return rows, nil
	}

	item, err := findItemByCodeOnApp(tx, p.ItemCode)
	if err != nil {
		return nil, fmt.Errorf("oc close item lookup: %w", err)
	}
	if item == nil {
		return nil, nil
	}

	// Foreman returns carry the holder's user_code in OriginalCheckoutUserCode;
	// self returns fall back to the cart user. Mirrors the kiosk-side default.
	targetCode := p.OriginalCheckoutUserCode
	if targetCode == "" {
		targetCode = p.UserCode
	}
	target, err := findUserByCodeOnApp(tx, targetCode)
	if err != nil {
		return nil, fmt.Errorf("oc close target user lookup: %w", err)
	}
	if target == nil {
		return nil, nil
	}

	primary, err := tx.FindRecordsByFilter("open_checkouts",
		"kiosk_code = {:k} && item = {:item} && user = {:user}",
		"checked_out_at", p.Qty, 0,
		dbx.Params{"k": p.KioskCode, "item": item.Id, "user": target.Id})
	if err != nil {
		return nil, fmt.Errorf("find primary open rows: %w", err)
	}
	if len(primary) >= p.Qty {
		return primary, nil
	}

	need := p.Qty - len(primary)
	fallback, err := tx.FindRecordsByFilter("open_checkouts",
		"kiosk_code = {:k} && item = {:item} && user != {:user}",
		"checked_out_at", need, 0,
		dbx.Params{"k": p.KioskCode, "item": item.Id, "user": target.Id})
	if err != nil {
		return nil, fmt.Errorf("find fallback open rows: %w", err)
	}
	return append(primary, fallback...), nil
}

// handleCheckoutAdminClose dispatches a checkout.admin_close event. The
// audit log lives in the parent handle() function; this is the
// open_checkouts state-projection side-effect.
func (a *Aggregator) handleCheckoutAdminClose(msg jetstream.Msg, p EventPayload) {
	switch a.ProjectOpenCheckoutsAdminClose(p) {
	case projectAck:
		_ = msg.Ack()
	case projectRetry:
		_ = msg.Nak()
	}
}

// ProjectOpenCheckoutsAdminClose deletes the one open_checkouts row that
// a checkout.admin_close event closed. For serialized rows the
// item_instance uniquely identifies it; for non-serialized we pin by
// (kiosk_code, item, user) and take the FIFO-oldest — the kiosk's
// admin-close picks the row to close by the open_checkouts.id it was
// given, but on the controller we don't have a stable cross-binary
// row id, so (kiosk, item, user, FIFO) is the equivalent match.
func (a *Aggregator) ProjectOpenCheckoutsAdminClose(p EventPayload) projectOutcome {
	txErr := a.app.RunInTransaction(func(tx core.App) error {
		var (
			rows []*core.Record
			err  error
		)
		if p.ItemInstanceID != "" {
			rows, err = tx.FindRecordsByFilter("open_checkouts",
				"kiosk_code = {:k} && source_item_instance_id = {:i}",
				"", 1, 0,
				dbx.Params{"k": p.KioskCode, "i": p.ItemInstanceID})
		} else {
			item, ierr := findItemByCodeOnApp(tx, p.ItemCode)
			if ierr != nil {
				return fmt.Errorf("oc admin_close item lookup: %w", ierr)
			}
			if item == nil {
				return nil
			}
			user, uerr := findUserByCodeOnApp(tx, p.UserCode)
			if uerr != nil {
				return fmt.Errorf("oc admin_close user lookup: %w", uerr)
			}
			if user == nil {
				return nil
			}
			rows, err = tx.FindRecordsByFilter("open_checkouts",
				"kiosk_code = {:k} && item = {:item} && user = {:user}",
				"checked_out_at", 1, 0,
				dbx.Params{"k": p.KioskCode, "item": item.Id, "user": user.Id})
		}
		if err != nil {
			return fmt.Errorf("oc admin_close target lookup: %w", err)
		}
		for _, r := range rows {
			if err := tx.Delete(r); err != nil {
				return fmt.Errorf("delete open_checkout: %w", err)
			}
		}
		return nil
	})
	if txErr != nil {
		slog.Warn("controller.aggregator.oc_admin_close.tx_failed", "error", txErr)
		return projectRetry
	}
	return projectAck
}
