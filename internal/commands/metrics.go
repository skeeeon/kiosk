package commands

import (
	"context"

	"github.com/skeeeon/kiosk/internal/metrics"
)

// handleMetricsSnapshot returns this kiosk's operational + activity snapshot.
// Read-only and idempotent, so unlike inventory.adjust it carries no
// command_id. Process-level state (cart store, RFID reader, NATS connection,
// config) is reached through KioskHandlers, which cmd/kiosk/main.go wires after
// both the dispatcher and *handlers.Handlers exist.
//
// A nil KioskHandlers (a test dispatcher without it, or the brief window before
// wiring) still returns a Reply rather than hanging, so the controller's 5 s
// request never times out into a false "offline".
func (d *Dispatcher) handleMetricsSnapshot(_ context.Context, _ []byte) Reply {
	h := d.KioskHandlers
	if h == nil {
		return Reply{Success: false, Error: "metrics unavailable: kiosk handlers not wired"}
	}
	snap, err := metrics.Compute(d.app, h.OperationalMetrics(), d.kioskCode)
	if err != nil {
		return Reply{Success: false, Error: err.Error()}
	}
	return Reply{Success: true, Data: snap}
}
