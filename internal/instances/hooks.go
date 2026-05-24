// Package instances binds PB record hooks on item_instances that write an
// audit row + publish an instance.lifecycle event for create / decommission
// (active true→false) / reactivate (active false→true) / delete.
//
// Cosmetic edits (code / serial / rfid_epc / notes) are intentionally not
// audited — they don't change "which physical thing this row represents."
// Auditing every typo fix would bury the high-signal lifecycle changes.
//
// Auth attribution: the hooks bind to the *Request variants so they fire
// only on REST-API-driven changes (PB SDK, /_/ superuser UI, admin SPA).
// e.Auth identifies the admin; back-fills / migrations that mutate
// item_instances outside an HTTP request write no audit row.
package instances

import (
	"log/slog"
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

// Hooks owns the small amount of cross-hook state we need (capturing record
// snapshots in OnRecordDelete so OnRecordAfterDeleteSuccess can write the
// final audit row even though the underlying row is gone).
type Hooks struct {
	pub publisher

	// pendingDelete: item_instances.id → captured snapshot from OnRecordDelete.
	// Read and removed in OnRecordAfterDeleteSuccess. sync.Map because PB may
	// fire hooks from any goroutine; lock contention is irrelevant at the
	// volume this collection sees.
	pendingDelete sync.Map
}

// New constructs a Hooks bound to events.Publish. Use NewWith for tests.
func New() *Hooks {
	return NewWith(natsPublisher{})
}

// NewWith allows callers (tests) to inject a custom publisher.
func NewWith(pub publisher) *Hooks {
	return &Hooks{pub: pub}
}

// Register binds the hooks on the given app. Safe to call once at boot.
func (h *Hooks) Register(app core.App) {
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
		newActive := e.Record.GetBool("active")
		h.writeAudit(app, auditInput{
			Record:    e.Record,
			Action:    "create",
			NewActive: newActive,
			Reason:    e.Record.GetString("notes"),
			AdminID:   adminID,
		})
		return nil
	})

	app.OnRecordUpdateRequest("item_instances").BindFunc(func(e *core.RecordRequestEvent) error {
		// Capture original active state BEFORE Next so we can compare after
		// the save. e.Record.Original() returns a record loaded from the DB
		// at request entry, so it reflects pre-mutation state.
		prevActive := false
		if orig := e.Record.Original(); orig != nil {
			prevActive = orig.GetBool("active")
		}
		if err := e.Next(); err != nil {
			return err
		}
		newActive := e.Record.GetBool("active")
		if newActive == prevActive {
			// Cosmetic edit — no audit. The reason field on item_instances
			// (notes) might change, but we don't audit notes-only changes.
			return nil
		}
		adminID := authAdminID(e)
		if adminID == "" {
			return nil
		}
		action := "reactivate"
		if prevActive && !newActive {
			action = "decommission"
		}
		h.writeAudit(app, auditInput{
			Record:     e.Record,
			Action:     action,
			PrevActive: prevActive,
			NewActive:  newActive,
			Reason:     e.Record.GetString("notes"),
			AdminID:    adminID,
		})
		return nil
	})

	app.OnRecordDeleteRequest("item_instances").BindFunc(func(e *core.RecordRequestEvent) error {
		// Snapshot the record before the delete cascades. OnRecordDelete /
		// OnRecordDeleteRequest fires before the row is gone, so we can read
		// every field; AfterDeleteSuccess sees an empty record.
		snap := snapshot(e.Record)
		snap.adminID = authAdminID(e)
		if snap.adminID == "" {
			return e.Next()
		}
		h.pendingDelete.Store(e.Record.Id, snap)
		return e.Next()
	})

	app.OnRecordAfterDeleteSuccess("item_instances").BindFunc(func(e *core.RecordEvent) error {
		v, ok := h.pendingDelete.LoadAndDelete(e.Record.Id)
		if !ok {
			return e.Next()
		}
		snap := v.(deleteSnapshot)
		// Build the audit row from the snapshot. The record itself is now
		// stale (PB may have wiped its fields), so we don't pass e.Record
		// through; we build from snap.
		h.writeAuditFromSnapshot(app, snap)
		return e.Next()
	})
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
	PrevActive bool
	NewActive  bool
	Reason     string
	AdminID    string
}

type deleteSnapshot struct {
	id         string
	code       string
	itemID     string
	prevActive bool
	reason     string
	adminID    string
}

// snapshot captures the fields off the about-to-be-deleted record that the
// AfterDeleteSuccess hook needs to write the audit row. The record's data
// can be voided by the time the after-hook fires, so we read everything we
// need up front. Item code/name are resolved later via the App handle.
func snapshot(rec *core.Record) deleteSnapshot {
	return deleteSnapshot{
		id:         rec.Id,
		code:       rec.GetString("code"),
		itemID:     rec.GetString("item"),
		prevActive: rec.GetBool("active"),
		reason:     rec.GetString("notes"),
	}
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
	rec.Set("prev_active", in.PrevActive)
	rec.Set("new_active", in.NewActive)
	if in.Reason != "" {
		rec.Set("reason", in.Reason)
	}
	rec.Set("admin", in.AdminID)
	rec.Set("source", "local")
	if err := app.Save(rec); err != nil {
		slog.Warn("instances.audit.save_failed",
			"instance_id", in.Record.Id, "action", in.Action, "error", err)
		return
	}

	id := kioskctx.Get()
	itemCode, itemName := lookupItemCode(app, in.Record.GetString("item"))
	payload := events.BuildInstanceLifecyclePayload(events.InstanceLifecycleInput{
		InstanceID:   in.Record.Id,
		InstanceCode: in.Record.GetString("code"),
		ItemID:       in.Record.GetString("item"),
		ItemCode:     itemCode,
		ItemName:     itemName,
		KioskCode:    id.KioskCode,
		LocationCode: id.LocationCode,
		Action:       in.Action,
		PrevActive:   in.PrevActive,
		NewActive:    in.NewActive,
		Reason:       in.Reason,
		Source:       "local",
		AdminID:      in.AdminID,
		CompletedAt:  time.Now().UTC(),
	})
	h.pub.Publish(events.InstanceLifecycleSubject(id.KioskCode), payload)
}

func (h *Hooks) writeAuditFromSnapshot(app core.App, snap deleteSnapshot) {
	col, err := app.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		slog.Warn("instances.audit.collection_missing", "error", err)
		return
	}

	rec := core.NewRecord(col)
	rec.Set("item_instance", snap.id)
	rec.Set("item", snap.itemID)
	rec.Set("action", "delete")
	rec.Set("prev_active", snap.prevActive)
	rec.Set("new_active", false)
	if snap.reason != "" {
		rec.Set("reason", snap.reason)
	}
	rec.Set("admin", snap.adminID)
	rec.Set("source", "local")
	if err := app.Save(rec); err != nil {
		slog.Warn("instances.audit.save_failed",
			"instance_id", snap.id, "action", "delete", "error", err)
		return
	}

	id := kioskctx.Get()
	itemCode, itemName := lookupItemCode(app, snap.itemID)
	payload := events.BuildInstanceLifecyclePayload(events.InstanceLifecycleInput{
		InstanceID:   snap.id,
		InstanceCode: snap.code,
		ItemID:       snap.itemID,
		ItemCode:     itemCode,
		ItemName:     itemName,
		KioskCode:    id.KioskCode,
		LocationCode: id.LocationCode,
		Action:       "delete",
		PrevActive:   snap.prevActive,
		NewActive:    false,
		Reason:       snap.reason,
		Source:       "local",
		AdminID:      snap.adminID,
		CompletedAt:  time.Now().UTC(),
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
