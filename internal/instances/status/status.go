// Package status holds the item_instances lifecycle vocabulary (the status
// enum + audit action verbs) and the single in-transaction status writer
// shared by the kiosk's commit path and the instances package.
//
// It exists as a leaf so internal/commit can drive a maintenance-on-return /
// retire transition inside its own DB transaction WITHOUT importing the whole
// internal/instances package — which would close an import cycle, since the
// notification + migration packages (transitively pulled into the instances
// test binary) already depend on commit. Keeping the primitive here means
// commit → status (leaf) and instances → status (leaf), with no back-edge.
//
// internal/instances re-exports every name here via thin aliases (see
// internal/instances/status.go) so its own code and tests keep referring to
// instances.StatusInService / instances.SetStatusInTx / instances.writeAudit
// unchanged.
package status

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// Instance lifecycle status enum (item_instances.status) + the audit action
// verbs that record transitions between them. "Out / checked out" is NOT a
// status — it stays derived from open_checkouts. See migration 1794.
const (
	StatusInService   = "in_service"
	StatusMaintenance = "maintenance"
	StatusRetired     = "retired"

	ActionCreate          = "create"
	ActionToMaintenance   = "to_maintenance"
	ActionReturnToService = "return_to_service"
	ActionRetire          = "retire"
	ActionUnretire        = "unretire"
)

// ActionForTransition derives the audit action verb from a status change.
// Deriving from the actual (prev → target) pair — rather than trusting a
// caller-supplied verb — keeps return_to_service (from maintenance) and
// unretire (from retired) correctly distinguished even though both target
// in_service.
func ActionForTransition(prev, target string) string {
	switch target {
	case StatusMaintenance:
		return ActionToMaintenance
	case StatusRetired:
		return ActionRetire
	case StatusInService:
		if prev == StatusRetired {
			return ActionUnretire
		}
		return ActionReturnToService
	}
	return ""
}

// SetStatusInput packages a single status transition for SetStatusInTx.
type SetStatusInput struct {
	InstanceID        string
	ItemID            string // optional; resolved from the instance when empty
	Target            string // StatusInService | StatusMaintenance | StatusRetired
	Reason            string
	Source            string // events.SourceLocal | events.SourceController
	AdminID           string
	ControllerAdminID string
	CommandID         string
}

// SetStatusInTx flips an item_instance's status inside an existing transaction
// and writes the matching instance_audit row. The action verb is derived from
// the (prev → target) transition. Returns the prior status and the audit row
// id; auditID is empty when the instance is already in the target status (a
// no-op that writes nothing). The caller publishes the instance.lifecycle
// event post-commit (commit.go / admin_close.go do this; the HTTP/command path
// goes through instances.PerformSetStatus instead).
//
// The single in-transaction status writer, shared by commit's
// maintenance-on-return and admin_close's retire so both produce an identical
// audit + recompute trail. The model-level after-success hook recomputes the
// item's derived quantity off the back of the instance Save.
func SetStatusInTx(tx core.App, in SetStatusInput) (prevStatus, auditID string, err error) {
	inst, err := tx.FindRecordById("item_instances", in.InstanceID)
	if err != nil {
		return "", "", fmt.Errorf("find item_instance %s: %w", in.InstanceID, err)
	}
	prevStatus = inst.GetString("status")
	if prevStatus == in.Target {
		return prevStatus, "", nil
	}
	inst.Set("status", in.Target)
	if err := tx.Save(inst); err != nil {
		return prevStatus, "", fmt.Errorf("set instance status: %w", err)
	}

	itemID := in.ItemID
	if itemID == "" {
		itemID = inst.GetString("item")
	}
	audit, err := WriteAudit(tx, AuditWriteInput{
		InstanceID:        in.InstanceID,
		ItemID:            itemID,
		Action:            ActionForTransition(prevStatus, in.Target),
		PrevStatus:        prevStatus,
		NewStatus:         in.Target,
		Reason:            in.Reason,
		Source:            in.Source,
		AdminID:           in.AdminID,
		ControllerAdminID: in.ControllerAdminID,
		CommandID:         in.CommandID,
	})
	if err != nil {
		return prevStatus, "", err
	}
	return prevStatus, audit.Id, nil
}

// AuditWriteInput is the field set for one instance_audit row.
type AuditWriteInput struct {
	InstanceID        string
	ItemID            string
	Action            string
	PrevStatus        string
	NewStatus         string
	Reason            string
	Source            string
	AdminID           string
	ControllerAdminID string
	CommandID         string
}

// WriteAudit inserts one instance_audit row inside the given transaction and
// returns it. Reason / admin / controller_admin_id / command_id are only set
// when non-empty so the column stays NULL otherwise.
func WriteAudit(tx core.App, in AuditWriteInput) (*core.Record, error) {
	col, err := tx.FindCollectionByNameOrId("instance_audit")
	if err != nil {
		return nil, fmt.Errorf("find instance_audit collection: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("item_instance", in.InstanceID)
	rec.Set("item", in.ItemID)
	rec.Set("action", in.Action)
	rec.Set("prev_status", in.PrevStatus)
	rec.Set("new_status", in.NewStatus)
	if in.Reason != "" {
		rec.Set("reason", in.Reason)
	}
	if in.AdminID != "" {
		rec.Set("admin", in.AdminID)
	}
	if in.ControllerAdminID != "" {
		rec.Set("controller_admin_id", in.ControllerAdminID)
	}
	rec.Set("source", in.Source)
	if in.CommandID != "" {
		rec.Set("command_id", in.CommandID)
	}
	if err := tx.Save(rec); err != nil {
		return nil, fmt.Errorf("save instance_audit: %w", err)
	}
	return rec, nil
}
