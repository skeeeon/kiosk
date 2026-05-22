package controller

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// ReportOpenCheckouts mirrors the kiosk's reports endpoint for cross-fleet
// use. Since the controller doesn't maintain an open_checkouts table (the
// kiosks own that view of their own state), we compute the same shape on
// demand from the projected transaction_lines ledger. The ?kiosk_code=
// filter slices the fleet view to one kiosk.
func (h *Handlers) ReportOpenCheckouts(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	kioskCode := re.Request.URL.Query().Get("kiosk_code")

	rows, err := ledger.ReplayOpenRows(h.App, kioskCode)
	if err != nil {
		return re.InternalServerError("replay open rows", err)
	}
	dtos, err := ledger.Hydrate(h.App, rows)
	if err != nil {
		return re.InternalServerError("hydrate open rows", err)
	}
	return re.JSON(http.StatusOK, dtos)
}
