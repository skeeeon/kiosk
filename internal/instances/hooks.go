// Package instances binds PB record hooks on item_instances that write an
// audit row + publish an instance.lifecycle event for create and status
// changes (in_service ⇄ maintenance ⇄ retired).
//
// Cosmetic edits (code / serial / rfid_epc / notes) and status-unchanged
// updates are intentionally not audited — they don't change "which physical
// thing this row represents" or its lifecycle state. Auditing every typo fix
// would bury the high-signal lifecycle changes.
//
// Auth attribution: the audit hooks bind to the *Request variants so they fire
// only on REST-API-driven changes (PB SDK, /_/ superuser UI, admin SPA).
// e.Auth identifies the admin; back-fills / migrations that mutate
// item_instances outside an HTTP request write no audit row.
//
// item_instances are never hard-deleted by the UI (admins retire instead). A
// parent items delete still cascades to its instances, though; the model-level
// after-delete hook recomputes the parent quantity for that case. Cascade
// deletes write no audit row — there's no admin request context, and the
// lifecycle event ("the SKU is gone") belongs at the item level.
package instances

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// publisher is the *Publisher / *Publish hook target. Decoupled so tests can
// capture events without real NATS.
type publisher interface {
	Publish(subject string, payload any)
}

// natsPublisher adapts events.Publish for the interface above.
type natsPublisher struct{}

func (natsPublisher) Publish(subject string, payload any) {
	events.Publish(subject, payload)
}

// Hooks owns the small amount of cross-hook state we need (capturing the
// parent item id in OnRecordDelete so OnRecordAfterDeleteSuccess can recompute
// the item's derived quantity even though the row + its FK are gone).
type Hooks struct {
	pub publisher

	// pendingRecompute: item_instances.id → parent item id, captured in the
	// model-level OnRecordDelete (before the row + its FK are gone) and drained
	// in OnRecordAfterDeleteSuccess to recompute the item's derived quantity.
	// Unconditional — it covers superuser /_/ and cascade deletes (a parent
	// items delete cascades here). PB may fire hooks from any goroutine; a
	// plain mutex is enough at this collection's volume.
	pendingRecomputeMu sync.Mutex
	pendingRecompute   map[string]string
}

// New constructs a Hooks bound to events.Publish. Use NewWith for tests.
func New() *Hooks {
	return NewWith(natsPublisher{})
}

// NewWith allows callers (tests) to inject a custom publisher.
func NewWith(pub publisher) *Hooks {
	return &Hooks{
		pub:              pub,
		pendingRecompute: map[string]string{},
	}
}

// storePendingRecompute records the parent item id of an about-to-be-deleted
// instance so the after-delete hook can recompute the item's quantity.
func (h *Hooks) storePendingRecompute(id, itemID string) {
	h.pendingRecomputeMu.Lock()
	h.pendingRecompute[id] = itemID
	h.pendingRecomputeMu.Unlock()
}

// takePendingRecompute returns the captured item id for a deleted instance and
// removes it. Falls back to ("", false) when nothing was captured.
func (h *Hooks) takePendingRecompute(id string) (string, bool) {
	h.pendingRecomputeMu.Lock()
	itemID, ok := h.pendingRecompute[id]
	if ok {
		delete(h.pendingRecompute, id)
	}
	h.pendingRecomputeMu.Unlock()
	return itemID, ok
}

// Register binds the hooks on the given app. Safe to call once at boot.
func (h *Hooks) Register(app core.App) {
	// Normalize rfid_epc to lower-case hex on EVERY write, regardless of path.
	// These are model-level hooks (not the *Request variants), so they fire on
	// the admin SPA's PB-REST writes, the controller command-bus mutations
	// (instances.Perform* → tx.Save), and seeds alike. The LLRP reader emits
	// lower-case hex and the scan/diff matchers compare exactly, so a stored
	// upper-case EPC would never match an observed tag — silently dropping the
	// tag (counter_scan) or synthesizing a wrong cart line (enclosure_diff).
	app.OnRecordCreate("item_instances").BindFunc(func(e *core.RecordEvent) error {
		normalizeEPC(e.Record)
		// Default a new unit to in_service. status is a Required field, so this
		// also lets every create path that doesn't care about lifecycle —
		// superuser /_/ quick-add, CSV import, test seeds — omit it and still
		// pass validation. Explicit callers (PerformCreate, retire/maintenance
		// flows) set it first, so this is a no-op for them. Runs before
		// e.Next() so the value is present when validation + persist run.
		if e.Record.GetString("status") == "" {
			e.Record.Set("status", StatusInService)
		}
		return e.Next()
	})
	app.OnRecordUpdate("item_instances").BindFunc(func(e *core.RecordEvent) error {
		normalizeEPC(e.Record)
		return e.Next()
	})

	// Keep items.quantity_on_hand for SERIALIZED items in sync with the count
	// of non-retired instances. These are model-level *AfterSuccess hooks (not
	// the *Request variants) so they fire for EVERY persistence path regardless
	// of auth: the admin SPA + superuser REST writes, the controller
	// command-bus mutations (instances.Perform* → tx.Save), commit's
	// maintenance-on-return, and admin_close's retire. PB defers AfterSuccess
	// hooks fired inside a transaction to run post-commit on the parent app
	// (core/db.go), so a recompute here lands once, after the instance write is
	// durable. RecomputeItemQuantity is a no-op for non-serialized items and
	// when nothing changed, so binding it unconditionally is safe and cheap. It
	// saves the items row (never item_instances), so it can't re-trigger these
	// hooks.
	app.OnRecordAfterCreateSuccess("item_instances").BindFunc(func(e *core.RecordEvent) error {
		if err := RecomputeItemQuantity(e.App, e.Record.GetString("item")); err != nil {
			slog.Warn("instances.recompute.create_failed",
				"instance_id", e.Record.Id, "error", err)
		}
		return e.Next()
	})
	app.OnRecordAfterUpdateSuccess("item_instances").BindFunc(func(e *core.RecordEvent) error {
		if err := RecomputeItemQuantity(e.App, e.Record.GetString("item")); err != nil {
			slog.Warn("instances.recompute.update_failed",
				"instance_id", e.Record.Id, "error", err)
		}
		return e.Next()
	})
	// Capture the parent item id before the delete so the after-hook can
	// recompute even when PB has voided the record's fields. Model-level +
	// unconditional so it also covers superuser and cascade deletes (a parent
	// items delete cascades to its instances).
	app.OnRecordDelete("item_instances").BindFunc(func(e *core.RecordEvent) error {
		h.storePendingRecompute(e.Record.Id, e.Record.GetString("item"))
		return e.Next()
	})

	app.OnRecordCreateRequest("item_instances").BindFunc(func(e *core.RecordRequestEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		adminID := authAdminID(e)
		if adminID == "" {
			// No admin context — back-fill, server-side seed, or anonymous
			// (which the rules block anyway). Skip the audit row.
			return nil
		}
		h.writeAudit(app, auditInput{
			Record:     e.Record,
			Action:     ActionCreate,
			PrevStatus: "",
			NewStatus:  e.Record.GetString("status"),
			Reason:     e.Record.GetString("notes"),
			AdminID:    adminID,
		})
		return nil
	})

	app.OnRecordUpdateRequest("item_instances").BindFunc(func(e *core.RecordRequestEvent) error {
		// Capture original status BEFORE Next so we can compare after the save.
		// e.Record.Original() returns a record loaded from the DB at request
		// entry, so it reflects pre-mutation state.
		prevStatus := ""
		if orig := e.Record.Original(); orig != nil {
			prevStatus = orig.GetString("status")
		}
		if err := e.Next(); err != nil {
			return err
		}
		newStatus := e.Record.GetString("status")
		if newStatus == prevStatus {
			// Cosmetic edit — no audit. The notes field might change, but we
			// don't audit notes-only changes.
			return nil
		}
		adminID := authAdminID(e)
		if adminID == "" {
			return nil
		}
		h.writeAudit(app, auditInput{
			Record:     e.Record,
			Action:     actionForTransition(prevStatus, newStatus),
			PrevStatus: prevStatus,
			NewStatus:  newStatus,
			Reason:     e.Record.GetString("notes"),
			AdminID:    adminID,
		})
		return nil
	})

	app.OnRecordAfterDeleteSuccess("item_instances").BindFunc(func(e *core.RecordEvent) error {
		// Recompute the parent item's derived quantity, unconditionally — it
		// must stay correct for cascade deletes (a parent items delete cascades
		// here, with no admin context). takePendingRecompute gives the captured
		// item id; fall back to the (possibly-stale) record field if missing.
		itemID, ok := h.takePendingRecompute(e.Record.Id)
		if !ok {
			itemID = e.Record.GetString("item")
		}
		if err := RecomputeItemQuantity(e.App, itemID); err != nil {
			slog.Warn("instances.recompute.delete_failed",
				"instance_id", e.Record.Id, "error", err)
		}
		return e.Next()
	})
}

// normalizeEPC lower-cases (and trims) the record's rfid_epc in place so the
// stored form matches the LLRP reader's lower-case hex output. No-op when the
// field is empty or already normalized. See the Register comment for why.
func normalizeEPC(rec *core.Record) {
	epc := rec.GetString("rfid_epc")
	if epc == "" {
		return
	}
	if norm := strings.ToLower(strings.TrimSpace(epc)); norm != epc {
		rec.Set("rfid_epc", norm)
	}
}

// authAdminID extracts the requesting admin's PB record id. Returns "" when
// the request is unauthenticated or the caller is not an admin (back-fills,
// system seeds, other auth collections). Collection rules already block
// non-admin REST writes; this guard is defense in depth.
func authAdminID(e *core.RecordRequestEvent) string {
	if e.RequestEvent == nil || e.Auth == nil {
		return ""
	}
	col := e.Auth.Collection()
	if col == nil || col.Name != "admins" {
		return ""
	}
	return e.Auth.Id
}

type auditInput struct {
	Record     *core.Record
	Action     string
	PrevStatus string
	NewStatus  string
	Reason     string
	AdminID    string
}

func (h *Hooks) writeAudit(app core.App, in auditInput) {
	col, err := app.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		slog.Warn("instances.audit.collection_missing", "error", err)
		return
	}

	rec := core.NewRecord(col)
	rec.Set("item_instance", in.Record.Id)
	rec.Set("item", in.Record.GetString("item"))
	rec.Set("action", in.Action)
	rec.Set("prev_status", in.PrevStatus)
	rec.Set("new_status", in.NewStatus)
	if in.Reason != "" {
		rec.Set("reason", in.Reason)
	}
	rec.Set("admin", in.AdminID)
	rec.Set("source", events.SourceLocal)
	if err := app.Save(rec); err != nil {
		slog.Warn("instances.audit.save_failed",
			"instance_id", in.Record.Id, "action", in.Action, "error", err)
		return
	}

	id := kioskctx.Get()
	itemCode, itemName := lookupItemCode(app, in.Record.GetString("item"))
	payload := events.BuildInstanceLifecyclePayload(events.InstanceLifecycleInput{
		InstanceID:    in.Record.Id,
		InstanceCode:  in.Record.GetString("code"),
		ItemID:        in.Record.GetString("item"),
		ItemCode:      itemCode,
		ItemName:      itemName,
		KioskCode:     id.KioskCode,
		LocationCode:  id.LocationCode,
		Action:        in.Action,
		PrevStatus:    in.PrevStatus,
		NewStatus:     in.NewStatus,
		Reason:        in.Reason,
		Source:        events.SourceLocal,
		AdminID:       in.AdminID,
		SourceAuditID: rec.Id,
		CompletedAt:   time.Now().UTC(),
		RFIDEPC:       in.Record.GetString("rfid_epc"),
	})
	h.pub.Publish(events.InstanceLifecycleSubject(id.KioskCode), payload)
}

// lookupItemCode resolves the item's code + name. Best-effort: returns
// ("", "") on miss (cascade-pending item, broken FK) so callers can still
// emit the audit row with the FK intact.
func lookupItemCode(app core.App, itemID string) (string, string) {
	if itemID == "" {
		return "", ""
	}
	item, err := app.FindRecordById("items", itemID)
	if err != nil {
		return "", ""
	}
	return item.GetString("code"), item.GetString("name")
}
