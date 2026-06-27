package commands

import "context"

// handleConfigSnapshot returns this kiosk's RFID reader/enclosure config (+ live
// connected status) for the controller's per-kiosk Readers tab. Read-only and
// idempotent, so unlike inventory.adjust it carries no command_id. Process state
// (config + live reader handles) is reached through KioskHandlers, wired by
// cmd/kiosk/main.go after both the dispatcher and *handlers.Handlers exist.
//
// A nil KioskHandlers (a test dispatcher without it, or the brief pre-wiring
// window) still returns a Reply rather than hanging, so the controller's 5 s
// request never times out into a false "offline".
func (d *Dispatcher) handleConfigSnapshot(_ context.Context, _ []byte) Reply {
	h := d.KioskHandlers
	if h == nil {
		return Reply{Success: false, Error: "config unavailable: kiosk handlers not wired"}
	}
	return Reply{Success: true, Data: h.RFIDConfigSnapshot()}
}
