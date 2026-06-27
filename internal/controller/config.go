package controller

import (
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// Config proxies the read-only config.snapshot command to a kiosk and passes the
// RFID reader/enclosure topology through to the SPA. Mirrors Metrics:
// admin-gated, heartbeat fast-fail, 503 kiosk_offline on timeout/no-responders.
// No command_id — the command mutates nothing.
func (h *Handlers) Config(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		kioskCode := strings.TrimSpace(re.Request.PathValue("code"))
		if kioskCode == "" {
			return re.BadRequestError("kiosk code is required", nil)
		}
		return dispatchKioskCommand(re, nc, reg, kioskCode,
			events.ConfigSnapshotCommandSubject(kioskCode), "", []byte("{}"))
	}
}
