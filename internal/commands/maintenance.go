// Maintenance command handlers — integrity rebuild and ledger republish.
// Both are operator tools that fix or refill state without changing the
// ledger's source-of-truth (transaction_lines). The controller-driven
// path here is a thin NATS wrapper around the same kiosk-side functions
// the HTTP handlers use, so behavior is identical to a kiosk admin
// triggering the same actions locally.
//
// Idempotency is implicit:
//   - integrity.rebuild wipes then re-replays open_checkouts from the
//     ledger; running it twice produces the same final state.
//   - ledger.republish re-emits events with the same source_line_id; the
//     controller's projection is unique-indexed on source_line_id so
//     duplicates are no-ops.
//
// Neither command needs a command_id for correctness; we still pass one
// through for log correlation.
package commands

import (
	"context"
	"encoding/json"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
)

// integrityRebuildRequest is the optional payload the controller sends. An
// empty body is also valid — the kiosk reads its own ledger regardless of
// what the controller knows.
type integrityRebuildRequest struct {
	CommandID         string `json:"command_id,omitempty"`
	ControllerAdminID string `json:"controller_admin_id,omitempty"`
}

func (d *Dispatcher) handleIntegrityRebuild(_ context.Context, payload []byte) Reply {
	var req integrityRebuildRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return Reply{Success: false, Error: "invalid request body: " + err.Error()}
		}
	}
	result, err := handlers.PerformIntegrityRebuild(d.app,
		events.SourceController, "", req.ControllerAdminID, req.CommandID)
	if err != nil {
		return Reply{Success: false, Error: "rebuild failed: " + err.Error()}
	}
	return Reply{Success: true, Data: result}
}

// ledgerRepublishRequest carries the optional time window. Both are
// RFC3339 strings; empty = no clip on that end.
type ledgerRepublishRequest struct {
	CommandID         string `json:"command_id,omitempty"`
	ControllerAdminID string `json:"controller_admin_id,omitempty"`
	From              string `json:"from,omitempty"`
	To                string `json:"to,omitempty"`
}

func (d *Dispatcher) handleLedgerRepublish(_ context.Context, payload []byte) Reply {
	var req ledgerRepublishRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return Reply{Success: false, Error: "invalid request body: " + err.Error()}
		}
	}
	result, err := handlers.PerformLedgerRepublish(d.app, req.From, req.To, events.Publish)
	if err != nil {
		return Reply{Success: false, Error: "republish failed: " + err.Error()}
	}
	return Reply{Success: true, Data: result}
}
