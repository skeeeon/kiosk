package handlers

import (
	"net/http"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/scan"
)

// Scan dispatches a raw barcode/QR value to user, item, or unknown via the
// scan resolver. The PB-backed lookups live on this handler; resolution
// logic itself is in the scan package and is unit-tested with fakes.
func (h *Handlers) Scan(re *core.RequestEvent) error {
	var body struct {
		Value string `json:"value"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}

	r := &scan.Resolver{
		UserPrefix: h.Cfg.Scanning.UserQRPrefix,
		ItemPrefix: h.Cfg.Scanning.ItemBarcodePrefix,
		Lookups: scan.Lookups{
			UserByCode: h.scanUserByCode,
			ItemByCode: h.scanItemByCode,
			ItemByRFID: h.scanItemByRFID,
		},
	}
	return re.JSON(http.StatusOK, r.Resolve(body.Value))
}

func (h *Handlers) scanUserByCode(code string) (*scan.User, error) {
	rec, err := h.App.FindFirstRecordByFilter("users", "code = {:code}", dbx.Params{"code": code})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return userFromRecord(rec), nil
}

func (h *Handlers) scanItemByCode(code string) (*scan.Item, error) {
	rec, err := h.App.FindFirstRecordByFilter("items", "code = {:code}", dbx.Params{"code": code})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return itemFromRecord(rec), nil
}

func (h *Handlers) scanItemByRFID(epc string) (*scan.Item, error) {
	if epc == "" {
		return nil, nil
	}
	rec, err := h.App.FindFirstRecordByFilter("items", "rfid_epc = {:epc}", dbx.Params{"epc": epc})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return itemFromRecord(rec), nil
}
