// Item-instance command handlers. Mirrors the inventory.adjust family —
// idempotent mutations gated by a controller-supplied command_id, plus a
// read-only snapshot. The kiosk's PB record hooks in internal/instances/hooks.go
// still cover the standard "admin clicks something in the SPA against
// /api/collections/item_instances" path; these handlers serve the
// controller-driven path where the request arrives over NATS with no
// e.Auth context the hooks could use.
//
// Both paths converge on the same instance_audit shape (source=local vs
// controller distinguishes them) and emit identically-shaped
// instance.lifecycle events, so the controller's projection sees a
// uniform stream regardless of origin.
package commands

import (
	"context"
	"encoding/json"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/instances"
)

// instanceCreateRequest is the payload the controller sends to create a
// new physical unit at this kiosk. command_id is the idempotency anchor —
// a retried command finds the prior instance_audit row by command_id and
// returns the prior result instead of double-creating.
type instanceCreateRequest struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	ItemCode          string `json:"item_code"`
	Code              string `json:"code"`
	Serial            string `json:"serial,omitempty"`
	RFIDEPC           string `json:"rfid_epc,omitempty"`
	Notes             string `json:"notes,omitempty"`
	Active            *bool  `json:"active,omitempty"`
}

func (d *Dispatcher) handleInstanceCreate(_ context.Context, payload []byte) Reply {
	var req instanceCreateRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid request body: " + err.Error()}
	}
	if req.CommandID == "" {
		return Reply{Success: false, Error: "command_id is required"}
	}
	if req.ControllerAdminID == "" {
		return Reply{Success: false, Error: "controller_admin_id is required"}
	}
	if req.ItemCode == "" {
		return Reply{Success: false, Error: "item_code is required"}
	}
	if req.Code == "" {
		return Reply{Success: false, Error: "code is required"}
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}

	out, err := instances.PerformCreate(d.app, instances.CreateInput{
		ItemCode:          req.ItemCode,
		Code:              req.Code,
		Serial:            req.Serial,
		RFIDEPC:           req.RFIDEPC,
		Notes:             req.Notes,
		Active:            active,
		Source:            events.SourceController,
		ControllerAdminID: req.ControllerAdminID,
		CommandID:         req.CommandID,
	})
	if err != nil {
		return Reply{Success: false, Error: "create failed: " + err.Error()}
	}
	instances.PublishLifecycle(d.app, out)
	return Reply{Success: true, Data: out.Result}
}

// instanceEditRequest covers cosmetic-only field updates. Nil-able fields
// distinguish "leave alone" from "set empty"; the controller endpoint
// passes through only fields the SPA explicitly touched. No command_id —
// cosmetic edits don't audit so there's nothing to dedupe on. A retry
// just overwrites with the same values.
type instanceEditRequest struct {
	InstanceCode string  `json:"instance_code"`
	Code         *string `json:"code,omitempty"`
	Serial       *string `json:"serial,omitempty"`
	RFIDEPC      *string `json:"rfid_epc,omitempty"`
	Notes        *string `json:"notes,omitempty"`
}

func (d *Dispatcher) handleInstanceEdit(_ context.Context, payload []byte) Reply {
	var req instanceEditRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid request body: " + err.Error()}
	}
	if req.InstanceCode == "" {
		return Reply{Success: false, Error: "instance_code is required"}
	}
	if req.Code == nil && req.Serial == nil && req.RFIDEPC == nil && req.Notes == nil {
		return Reply{Success: false, Error: "at least one field is required"}
	}
	result, err := instances.PerformEdit(d.app, instances.EditInput{
		InstanceCode: req.InstanceCode,
		Code:         req.Code,
		Serial:       req.Serial,
		RFIDEPC:      req.RFIDEPC,
		Notes:        req.Notes,
	})
	if err != nil {
		return Reply{Success: false, Error: "edit failed: " + err.Error()}
	}
	return Reply{Success: true, Data: result}
}

// instanceToggleRequest is shared between decommission and reactivate —
// they take the same input shape, only the action differs. Reason is
// required at the operational level even though the schema field is
// optional; the audit log is more useful with a non-empty reason.
type instanceToggleRequest struct {
	CommandID         string `json:"command_id"`
	ControllerAdminID string `json:"controller_admin_id"`
	InstanceCode      string `json:"instance_code"`
	Reason            string `json:"reason"`
}

func (d *Dispatcher) handleInstanceDecommission(_ context.Context, payload []byte) Reply {
	return d.handleInstanceToggle(payload, false /* targetActive */)
}

func (d *Dispatcher) handleInstanceReactivate(_ context.Context, payload []byte) Reply {
	return d.handleInstanceToggle(payload, true /* targetActive */)
}

func (d *Dispatcher) handleInstanceToggle(payload []byte, targetActive bool) Reply {
	var req instanceToggleRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return Reply{Success: false, Error: "invalid request body: " + err.Error()}
	}
	if req.CommandID == "" {
		return Reply{Success: false, Error: "command_id is required"}
	}
	if req.ControllerAdminID == "" {
		return Reply{Success: false, Error: "controller_admin_id is required"}
	}
	if req.InstanceCode == "" {
		return Reply{Success: false, Error: "instance_code is required"}
	}
	if req.Reason == "" {
		return Reply{Success: false, Error: "reason is required"}
	}
	input := instances.ToggleInput{
		InstanceCode:      req.InstanceCode,
		Reason:            req.Reason,
		Source:            events.SourceController,
		ControllerAdminID: req.ControllerAdminID,
		CommandID:         req.CommandID,
	}
	var (
		out *instances.MutationOutcome
		err error
	)
	if targetActive {
		out, err = instances.PerformReactivate(d.app, input)
	} else {
		out, err = instances.PerformDecommission(d.app, input)
	}
	if err != nil {
		return Reply{Success: false, Error: "toggle failed: " + err.Error()}
	}
	instances.PublishLifecycle(d.app, out)
	return Reply{Success: true, Data: out.Result}
}

// instanceSnapshotRequest filters by item_code or returns every instance
// when empty. Same shape as inventorySnapshotRequest's all-vs-filtered
// split; instances tend to be 1:1 with serialized items so the result set
// is small.
type instanceSnapshotRequest struct {
	ItemCode string `json:"item_code,omitempty"`
}

type instanceSnapshotReply struct {
	Instances []instances.SnapshotRow `json:"instances"`
}

func (d *Dispatcher) handleInstanceSnapshot(_ context.Context, payload []byte) Reply {
	var req instanceSnapshotRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return Reply{Success: false, Error: "invalid request body: " + err.Error()}
		}
	}
	rows, err := instances.Snapshot(d.app, req.ItemCode)
	if err != nil {
		return Reply{Success: false, Error: "snapshot failed: " + err.Error()}
	}
	return Reply{Success: true, Data: instanceSnapshotReply{Instances: rows}}
}
