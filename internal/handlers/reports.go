// Reports endpoints serve cross-cutting read queries that don't fit cleanly
// into PB's collection REST API. The Currently-out / Aging tabs of the
// admin SPA use this on the controller (where there's no commit-maintained
// open_checkouts table to read directly — events from the fleet land as
// transactions + transaction_lines, which we replay on demand) and on the
// kiosk for symmetry.
package handlers

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// ReportOpenCheckouts returns the rows currently checked out, computed by
// replaying the transaction_lines ledger. Optional ?kiosk_code= filter
// scopes the replay to one kiosk's transactions — useful on the controller
// where projected transactions span the whole fleet.
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
