package controller

import (
	"net/http"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// ReportOpenCheckouts mirrors the kiosk's reports endpoint for cross-fleet
// use. The controller's open_checkouts table is maintained incrementally
// by ProjectOpenCheckouts as item.{checkout,return} + checkout.admin_close
// events flow in, so this endpoint reads it directly rather than replaying
// the ledger. The ?kiosk_code= filter slices the fleet view to one kiosk.
func (h *Handlers) ReportOpenCheckouts(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	kioskCode := re.Request.URL.Query().Get("kiosk_code")

	filter := ""
	params := dbx.Params{}
	if kioskCode != "" {
		filter = "kiosk_code = {:k}"
		params["k"] = kioskCode
	}
	recs, err := h.App.FindRecordsByFilter("open_checkouts",
		filter, "checked_out_at", 0, 0, params)
	if err != nil {
		return re.InternalServerError("load open_checkouts", err)
	}

	rows := make([]ledger.OpenRow, 0, len(recs))
	for _, r := range recs {
		rows = append(rows, ledger.OpenRow{
			Item: r.GetString("item"),
			// source_item_instance_id is the cross-binary identifier; the
			// RelationField on the row stays empty on the controller.
			ItemInstance:    r.GetString("source_item_instance_id"),
			User:            r.GetString("user"),
			Serial:          r.GetString("serial"),
			CheckedOutAt:    r.GetDateTime("checked_out_at").Time(),
			TransactionLine: r.GetString("transaction_line"),
		})
	}
	dtos, err := ledger.Hydrate(h.App, rows)
	if err != nil {
		return re.InternalServerError("hydrate open rows", err)
	}
	return re.JSON(http.StatusOK, dtos)
}
