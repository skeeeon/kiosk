package handlers

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/metrics"
)

// Metrics returns a point-in-time operational + activity snapshot for this
// kiosk. Admin-gated like the other read endpoints. The metrics.snapshot NATS
// command (internal/commands/metrics.go) returns the same shape via the same
// metrics.Compute call, so the controller's per-kiosk Metrics tab and this
// local view render identically.
func (h *Handlers) Metrics(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	snap, err := metrics.Compute(h.App, h.OperationalMetrics(), kioskctx.Get().KioskCode)
	if err != nil {
		return re.InternalServerError("compute metrics", err)
	}
	return re.JSON(http.StatusOK, snap)
}

// OperationalMetrics gathers process-level health for the metrics snapshot.
// Exported so the NATS metrics.snapshot command handler reuses the exact same
// gathering as this local endpoint — both feed metrics.Compute.
func (h *Handlers) OperationalMetrics() metrics.Operational {
	natsConnected := false
	if p := events.CurrentPublisher(); p != nil {
		if nc, err := events.Conn(p); err == nil && nc != nil && nc.IsConnected() {
			natsConnected = true
		}
	}
	return metrics.Operational{
		UptimeSeconds: int64(time.Since(h.StartedAt).Seconds()),
		NATSConnected: natsConnected,
		RFIDEnabled:   h.Cfg.RFID.Enabled,
		RFIDMode:      h.Cfg.RFID.Mode,
		RFIDConnected: h.RFID != nil && h.RFID.Connected(),
		ActiveCarts:   h.Carts.Count(),
	}
}
