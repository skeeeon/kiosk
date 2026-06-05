package commands

import (
	"context"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// checkoutSnapshotReply is the checkout.snapshot reply shape. The rows are the
// same hydrated DTOs the kiosk's own /reports/open-checkouts endpoint returns
// (ledger.OpenCheckoutDTO), so the controller can concatenate fleet replies
// and feed the existing CSV writer / SPA without remapping.
type checkoutSnapshotReply struct {
	OpenCheckouts []ledger.OpenCheckoutDTO `json:"open_checkouts"`
}

// handleCheckoutSnapshot returns the kiosk's currently-open checkouts, read
// directly from the materialized open_checkouts table (O(what's out), not
// O(history)) and hydrated through the same ledger.Hydrate path the report
// uses — so the DTO shape and id scheme match exactly (the admin-close flow
// keys on the transaction_line id surfaced here). Read-only: no DB writes, no
// events, no idempotency. Bounded by the open-checkout count, well under NATS's
// default message limit.
func (d *Dispatcher) handleCheckoutSnapshot(_ context.Context, _ []byte) Reply {
	ocRows, err := d.app.FindRecordsByFilter("open_checkouts", "", "checked_out_at", 0, 0)
	if err != nil {
		return Reply{Success: false, Error: "open_checkouts lookup failed: " + err.Error()}
	}
	rows := make([]ledger.OpenRow, 0, len(ocRows))
	for _, oc := range ocRows {
		rows = append(rows, ledger.OpenRow{
			Item:            oc.GetString("item"),
			ItemInstance:    oc.GetString("item_instance"),
			User:            oc.GetString("user"),
			Serial:          oc.GetString("serial"),
			CheckedOutAt:    oc.GetDateTime("checked_out_at").Time(),
			TransactionLine: oc.GetString("transaction_line"),
		})
	}
	dtos, err := ledger.Hydrate(d.app, rows)
	if err != nil {
		return Reply{Success: false, Error: "hydrate open checkouts failed: " + err.Error()}
	}
	return Reply{Success: true, Data: checkoutSnapshotReply{OpenCheckouts: dtos}}
}
