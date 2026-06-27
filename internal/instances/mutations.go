// Pure mutation functions for item_instances + instance_audit, used by
// both the kiosk's existing PB record-hook flow and the controller's
// command-bus flow.
//
// The PB hooks in hooks.go cover the standard "admin clicks something in
// the SPA against PocketBase REST" path: they fire on /api/collections/*
// writes from an admin-authed session. Command-bus mutations come in over
// NATS from the controller, with no HTTP request and no e.Auth — so the
// hooks don't apply, and the dispatcher calls these mutation functions
// directly. Both paths converge on the same audit + lifecycle-event
// shape so downstream consumers (the controller's instance_lifecycle_audit
// projection, the SPA's audit timeline) can't tell where the mutation
// originated except via the `source` field on each audit row.
//
// Idempotency for command-bus calls is the unique-when-non-empty
// command_id index on instance_audit. A redelivery looks up the prior
// row and returns its outcome instead of double-applying. Same shape as
// stock_adjustments + PerformStockAdjustment.
package instances

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

// errIdempotentReplay flags a command-bus mutation that's already been
// applied. The outer function rolls back the txn and re-fetches the prior
// outcome. Same sentinel pattern as handlers/stock_adjust.go.
var errIdempotentReplay = errors.New("idempotent replay")

// CreateInput packages the args for PerformCreate. ItemCode resolves to
// items.id server-side — kiosks and the controller speak in codes, not
// IDs, since IDs differ across PB instances. Status defaults to in_service
// when empty.
type CreateInput struct {
	ItemCode          string
	Code              string
	Serial            string
	RFIDEPC           string
	Notes             string
	EnclosureID       string // access-controlled cabinet the unit lives in (enclosure_diff partition); empty = counter/crib or single-cabinet
	Status            string // StatusInService (default) | StatusMaintenance | StatusRetired
	Source            string // events.SourceLocal | events.SourceController
	AdminID           string // local admin PB id (source=local); empty otherwise
	ControllerAdminID string // controller admin id text (source=controller); empty otherwise
	CommandID         string // idempotency anchor for controller commands
}

// EditInput packages the args for PerformEdit. Pointer fields distinguish
// "not set" from "explicit empty value" so callers can leave fields alone
// without overwriting them. Cosmetic only — no audit, no lifecycle event,
// mirroring the existing PB hook's "edits that don't change status don't
// audit" rule.
type EditInput struct {
	InstanceCode string  // identifies which row to edit
	Code         *string // rename
	Serial       *string
	RFIDEPC      *string
	Notes        *string
	EnclosureID  *string // reassign the unit to a cabinet; *"" clears it
}

// ToggleInput packages the args for PerformSetStatus. Reason is required
// (operationally — "why is this drill being sent to the bench") even though
// the schema field is optional.
type ToggleInput struct {
	InstanceCode      string
	Reason            string
	Source            string
	AdminID           string
	ControllerAdminID string
	CommandID         string
}

// InstanceResult is the wire payload returned by every mutation function.
// Just the cross-binary identifiers + the post-mutation status — enough for
// the controller to render the updated row in the SPA without a follow-up
// snapshot fetch.
type InstanceResult struct {
	InstanceID   string `json:"instance_id"`
	InstanceCode string `json:"instance_code"`
	ItemID       string `json:"item_id"`
	ItemCode     string `json:"item_code"`
	Status       string `json:"status"`
	EnclosureID  string `json:"enclosure_id"`
}

// MutationOutcome bundles InstanceResult with the audit row id and the
// lifecycle action so the caller can build the matching instance.lifecycle
// event without re-loading the audit row. Used for create / set-status;
// PerformEdit doesn't produce an audit row (cosmetic) so it returns just
// InstanceResult.
type MutationOutcome struct {
	Result        InstanceResult
	AuditRecordID string
	Action        string // create | to_maintenance | return_to_service | retire | unretire
	PrevStatus    string
	NewStatus     string
	Reason        string
}

// PerformCreate inserts a row in item_instances and writes the matching
// instance_audit row. Returns errIdempotentReplay (wrapped) when a prior
// command_id has already created an instance — the caller re-fetches the
// prior outcome instead of double-creating.
func PerformCreate(app core.App, in CreateInput) (*MutationOutcome, error) {
	if err := validateSource(in.Source, in.CommandID); err != nil {
		return nil, err
	}
	if in.Code == "" {
		return nil, fmt.Errorf("code is required")
	}
	status := in.Status
	if status == "" {
		status = StatusInService
	}

	var out MutationOutcome
	err := app.RunInTransaction(func(tx core.App) error {
		// Idempotency fast path: an already-processed command_id returns
		// the prior outcome without re-inserting.
		if in.CommandID != "" {
			if _, ok, lerr := findInstanceAuditByCommandID(tx, in.CommandID); lerr != nil {
				return fmt.Errorf("idempotency lookup: %w", lerr)
			} else if ok {
				// Already processed under this command_id; roll back the
				// (empty) txn and re-read the prior outcome below.
				return errIdempotentReplay
			}
		}

		item, err := tx.FindFirstRecordByFilter("items", "code = {:c}",
			dbx.Params{"c": in.ItemCode})
		if err != nil {
			return fmt.Errorf("find item %q: %w", in.ItemCode, err)
		}

		col, err := tx.FindCollectionByNameOrId("item_instances")
		if err != nil {
			return fmt.Errorf("find item_instances collection: %w", err)
		}
		inst := core.NewRecord(col)
		inst.Set("item", item.Id)
		inst.Set("code", in.Code)
		if in.Serial != "" {
			inst.Set("serial", in.Serial)
		}
		if in.RFIDEPC != "" {
			inst.Set("rfid_epc", in.RFIDEPC)
		}
		if in.Notes != "" {
			inst.Set("notes", in.Notes)
		}
		if in.EnclosureID != "" {
			inst.Set("enclosure_id", in.EnclosureID)
		}
		inst.Set("status", status)
		if err := tx.Save(inst); err != nil {
			return fmt.Errorf("save item_instance: %w", err)
		}

		audit, err := writeAudit(tx, auditWriteInput{
			InstanceID:        inst.Id,
			ItemID:            item.Id,
			Action:            ActionCreate,
			PrevStatus:        "",
			NewStatus:         status,
			Reason:            in.Notes,
			Source:            in.Source,
			AdminID:           in.AdminID,
			ControllerAdminID: in.ControllerAdminID,
			CommandID:         in.CommandID,
		})
		if err != nil {
			if in.CommandID != "" && dberr.IsUniqueViolation(err) {
				return errIdempotentReplay
			}
			return err
		}
		out = MutationOutcome{
			Result: InstanceResult{
				InstanceID:   inst.Id,
				InstanceCode: inst.GetString("code"),
				ItemID:       item.Id,
				ItemCode:     item.GetString("code"),
				Status:       status,
				EnclosureID:  inst.GetString("enclosure_id"),
			},
			AuditRecordID: audit.Id,
			Action:        ActionCreate,
			PrevStatus:    "",
			NewStatus:     status,
			Reason:        in.Notes,
		}
		return nil
	})
	if errors.Is(err, errIdempotentReplay) {
		// Both detection paths (upfront lookup + unique violation on write)
		// converge here. Re-read the prior outcome outside the rolled-back
		// txn so there's a single exit and one canonical result shape.
		return refetchOutcomeByCommandID(app, in.CommandID)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PerformEdit applies cosmetic field updates (code, serial, rfid_epc,
// notes). No audit, no lifecycle event — mirrors the existing PB hook's
// rule that edits which don't change status are not lifecycle events.
// Returns the post-edit identifiers so the caller can echo to the SPA.
func PerformEdit(app core.App, in EditInput) (*InstanceResult, error) {
	if in.InstanceCode == "" {
		return nil, fmt.Errorf("instance_code is required")
	}
	var out InstanceResult
	err := app.RunInTransaction(func(tx core.App) error {
		inst, err := tx.FindFirstRecordByFilter("item_instances",
			"code = {:c}", dbx.Params{"c": in.InstanceCode})
		if err != nil {
			return fmt.Errorf("find item_instance %q: %w", in.InstanceCode, err)
		}
		if in.Code != nil {
			inst.Set("code", *in.Code)
		}
		if in.Serial != nil {
			inst.Set("serial", *in.Serial)
		}
		if in.RFIDEPC != nil {
			inst.Set("rfid_epc", *in.RFIDEPC)
		}
		if in.Notes != nil {
			inst.Set("notes", *in.Notes)
		}
		if in.EnclosureID != nil {
			inst.Set("enclosure_id", *in.EnclosureID)
		}
		if err := tx.Save(inst); err != nil {
			return fmt.Errorf("save item_instance: %w", err)
		}
		item, err := tx.FindRecordById("items", inst.GetString("item"))
		if err != nil {
			return fmt.Errorf("find item: %w", err)
		}
		out = InstanceResult{
			InstanceID:   inst.Id,
			InstanceCode: inst.GetString("code"),
			ItemID:       item.Id,
			ItemCode:     item.GetString("code"),
			Status:       inst.GetString("status"),
			EnclosureID:  inst.GetString("enclosure_id"),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PerformSetStatus transitions an instance to the target status and writes
// the matching audit row. The action verb is derived from the (prev → target)
// transition, so it correctly distinguishes return_to_service (from
// maintenance) from unretire (from retired) even though both target
// in_service. No-op (returns the existing outcome, no audit) when the instance
// is already in the target status. Idempotent on command_id for controller
// commands.
func PerformSetStatus(app core.App, in ToggleInput, target string) (*MutationOutcome, error) {
	if err := validateSource(in.Source, in.CommandID); err != nil {
		return nil, err
	}
	if in.InstanceCode == "" {
		return nil, fmt.Errorf("instance_code is required")
	}
	if target != StatusInService && target != StatusMaintenance && target != StatusRetired {
		return nil, fmt.Errorf("invalid status %q", target)
	}

	var out MutationOutcome
	err := app.RunInTransaction(func(tx core.App) error {
		if in.CommandID != "" {
			if _, ok, lerr := findInstanceAuditByCommandID(tx, in.CommandID); lerr != nil {
				return fmt.Errorf("idempotency lookup: %w", lerr)
			} else if ok {
				// Already processed under this command_id; roll back the
				// (empty) txn and re-read the prior outcome below.
				return errIdempotentReplay
			}
		}

		inst, err := tx.FindFirstRecordByFilter("item_instances",
			"code = {:c}", dbx.Params{"c": in.InstanceCode})
		if err != nil {
			return fmt.Errorf("find item_instance %q: %w", in.InstanceCode, err)
		}
		itemID := inst.GetString("item")
		item, err := tx.FindRecordById("items", itemID)
		if err != nil {
			return fmt.Errorf("find item: %w", err)
		}

		prevStatus, auditID, serr := SetStatusInTx(tx, SetStatusInput{
			InstanceID:        inst.Id,
			ItemID:            itemID,
			Target:            target,
			Reason:            in.Reason,
			Source:            in.Source,
			AdminID:           in.AdminID,
			ControllerAdminID: in.ControllerAdminID,
			CommandID:         in.CommandID,
		})
		if serr != nil {
			if in.CommandID != "" && dberr.IsUniqueViolation(serr) {
				return errIdempotentReplay
			}
			return serr
		}

		result := InstanceResult{
			InstanceID:   inst.Id,
			InstanceCode: inst.GetString("code"),
			ItemID:       itemID,
			ItemCode:     item.GetString("code"),
			Status:       inst.GetString("status"),
			EnclosureID:  inst.GetString("enclosure_id"),
		}
		if auditID == "" {
			// No-op: already in target status. Report current state, no event.
			out = MutationOutcome{
				Result:     result,
				PrevStatus: prevStatus,
				NewStatus:  prevStatus,
			}
			return nil
		}
		out = MutationOutcome{
			Result:        result,
			AuditRecordID: auditID,
			Action:        actionForTransition(prevStatus, target),
			PrevStatus:    prevStatus,
			NewStatus:     target,
			Reason:        in.Reason,
		}
		return nil
	})
	if errors.Is(err, errIdempotentReplay) {
		// Both detection paths (upfront lookup + unique violation on write)
		// converge here. Re-read the prior outcome outside the rolled-back
		// txn so there's a single exit and one canonical result shape.
		return refetchOutcomeByCommandID(app, in.CommandID)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PublishLifecycle builds and emits the instance.lifecycle event for one
// MutationOutcome. Called by the kiosk-side command handlers after a
// successful Perform*; the PB record hooks have their own path
// (writeAudit) that emits inline. Both converge on the same payload shape.
//
// No-op when outcome.AuditRecordID is empty — that's the set-status-already-
// in-target case, which writes no audit and emits no event.
func PublishLifecycle(app core.App, out *MutationOutcome) {
	if out == nil || out.AuditRecordID == "" {
		return
	}
	id := kioskctx.Get()
	source, adminID, controllerAdminID, commandID := readAttribution(app, out.AuditRecordID)
	// Look up the current EPC so the controller's instance_epc_index (L3) stays
	// current. Best-effort — a missing/retired row just omits it.
	rfidEPC := ""
	if rec, err := app.FindRecordById("item_instances", out.Result.InstanceID); err == nil {
		rfidEPC = rec.GetString("rfid_epc")
	}
	payload := events.BuildInstanceLifecyclePayload(events.InstanceLifecycleInput{
		InstanceID:        out.Result.InstanceID,
		InstanceCode:      out.Result.InstanceCode,
		ItemID:            out.Result.ItemID,
		ItemCode:          out.Result.ItemCode,
		KioskCode:         id.KioskCode,
		LocationCode:      id.LocationCode,
		Action:            out.Action,
		PrevStatus:        out.PrevStatus,
		NewStatus:         out.NewStatus,
		Reason:            out.Reason,
		Source:            source,
		AdminID:           adminID,
		ControllerAdminID: controllerAdminID,
		CommandID:         commandID,
		SourceAuditID:     out.AuditRecordID,
		CompletedAt:       time.Now().UTC(),
		RFIDEPC:           rfidEPC,
	})
	events.Publish(events.InstanceLifecycleSubject(id.KioskCode), payload)
}

// Snapshot returns the item_instances rows on this kiosk, optionally
// filtered by item_code. Used by the instance.snapshot command.
type SnapshotRow struct {
	InstanceID   string `json:"instance_id"`
	InstanceCode string `json:"instance_code"`
	ItemID       string `json:"item_id"`
	ItemCode     string `json:"item_code"`
	ItemName     string `json:"item_name"`
	Serial       string `json:"serial"`
	RFIDEPC      string `json:"rfid_epc"`
	Status       string `json:"status"`
	Notes        string `json:"notes"`
	EnclosureID  string `json:"enclosure_id"`
	Created      string `json:"created"`
	Updated      string `json:"updated"`
	// Advisory last-observed location (location/sightings). Rides the
	// instance.snapshot reply so the controller's KioskInstancesPanel shows the
	// same "Last seen" the local panel does — kept current by the L3 mirror
	// watcher in managed mode. Empty until something is observed.
	LastObservedAt      string  `json:"last_observed_at"`
	LastObservedZone    string  `json:"last_observed_zone"`
	LastObservedGateway string  `json:"last_observed_gateway"`
	LastObservedLat     float64 `json:"last_observed_lat"`
	LastObservedLon     float64 `json:"last_observed_lon"`
	// Out reports whether this instance is currently checked out, derived
	// from open_checkouts. Snapshot leaves it false (it has no checkout
	// context); the command handler that ships this over the wire fills it
	// so the controller needn't replay its ledger to learn out-status.
	Out bool `json:"out"`
}

// Snapshot returns every item_instance row, optionally filtered by item
// code. Retired rows are included — the SPA renders them grayed out rather
// than hiding them, so an "un-retire" affordance has something to click on.
func Snapshot(app core.App, itemCode string) ([]SnapshotRow, error) {
	filter := ""
	params := dbx.Params{}
	if itemCode != "" {
		item, err := app.FindFirstRecordByFilter("items", "code = {:c}",
			dbx.Params{"c": itemCode})
		if err != nil {
			return nil, fmt.Errorf("find item %q: %w", itemCode, err)
		}
		filter = "item = {:item}"
		params["item"] = item.Id
	}
	rows, err := app.FindRecordsByFilter("item_instances", filter, "code", 0, 0, params)
	if err != nil {
		return nil, fmt.Errorf("list item_instances: %w", err)
	}
	out := make([]SnapshotRow, 0, len(rows))
	itemCache := map[string]*core.Record{}
	for _, r := range rows {
		itemID := r.GetString("item")
		item, ok := itemCache[itemID]
		if !ok {
			item, _ = app.FindRecordById("items", itemID)
			itemCache[itemID] = item
		}
		row := SnapshotRow{
			InstanceID:   r.Id,
			InstanceCode: r.GetString("code"),
			ItemID:       itemID,
			Serial:       r.GetString("serial"),
			RFIDEPC:      r.GetString("rfid_epc"),
			Status:       r.GetString("status"),
			Notes:        r.GetString("notes"),
			EnclosureID:  r.GetString("enclosure_id"),
			Created:      r.GetDateTime("created").String(),
			Updated:      r.GetDateTime("updated").String(),

			LastObservedAt:      r.GetDateTime("last_observed_at").String(),
			LastObservedZone:    r.GetString("last_observed_zone"),
			LastObservedGateway: r.GetString("last_observed_gateway"),
			LastObservedLat:     r.GetFloat("last_observed_lat"),
			LastObservedLon:     r.GetFloat("last_observed_lon"),
		}
		if item != nil {
			row.ItemCode = item.GetString("code")
			row.ItemName = item.GetString("name")
		}
		out = append(out, row)
	}
	return out, nil
}

// --- internal helpers ---

func validateSource(source, commandID string) error {
	if source != events.SourceLocal && source != events.SourceController {
		return fmt.Errorf("invalid source %q", source)
	}
	if source == events.SourceController && commandID == "" {
		return fmt.Errorf("command_id is required for controller-source mutations")
	}
	return nil
}

func findInstanceAuditByCommandID(tx core.App, commandID string) (*core.Record, bool, error) {
	rec, err := tx.FindFirstRecordByFilter("instance_audit",
		"command_id = {:c}", dbx.Params{"c": commandID})
	if err != nil {
		if dberr.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return rec, true, nil
}

func buildOutcome(inst, audit *core.Record) MutationOutcome {
	return MutationOutcome{
		Result: InstanceResult{
			InstanceID:   inst.Id,
			InstanceCode: inst.GetString("code"),
			ItemID:       inst.GetString("item"),
			Status:       inst.GetString("status"),
			EnclosureID:  inst.GetString("enclosure_id"),
		},
		AuditRecordID: audit.Id,
		Action:        audit.GetString("action"),
		PrevStatus:    audit.GetString("prev_status"),
		NewStatus:     audit.GetString("new_status"),
		Reason:        audit.GetString("reason"),
	}
}

func refetchOutcomeByCommandID(app core.App, commandID string) (*MutationOutcome, error) {
	audit, ok, err := findInstanceAuditByCommandID(app, commandID)
	if err != nil {
		return nil, fmt.Errorf("idempotent replay re-fetch audit: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("idempotent replay: command_id %q not found after rollback", commandID)
	}
	inst, err := app.FindRecordById("item_instances", audit.GetString("item_instance"))
	if err != nil {
		return nil, fmt.Errorf("idempotent replay re-fetch instance: %w", err)
	}
	out := buildOutcome(inst, audit)
	// Re-hydrate item_code from items by id since buildOutcome can't.
	if item, err := app.FindRecordById("items", inst.GetString("item")); err == nil && item != nil {
		out.Result.ItemCode = item.GetString("code")
	}
	return &out, nil
}

func readAttribution(app core.App, auditID string) (source, adminID, controllerAdminID, commandID string) {
	rec, err := app.FindRecordById("instance_audit", auditID)
	if err != nil {
		return "", "", "", ""
	}
	return rec.GetString("source"),
		rec.GetString("admin"),
		rec.GetString("controller_admin_id"),
		rec.GetString("command_id")
}
