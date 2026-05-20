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
			UserByCode:         h.scanUserByCode,
			ItemByCode:         h.scanItemByCode,
			ItemByRFID:         h.scanItemByRFID,
			ItemInstanceByCode: h.scanInstanceByCode,
			ItemInstanceByRFID: h.scanInstanceByRFID,
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

func (h *Handlers) scanInstanceByCode(code string) (*scan.InstanceMatch, error) {
	if code == "" {
		return nil, nil
	}
	rec, err := h.App.FindFirstRecordByFilter("item_instances", "code = {:code}", dbx.Params{"code": code})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h.instanceMatchFromRecord(rec)
}

func (h *Handlers) scanInstanceByRFID(epc string) (*scan.InstanceMatch, error) {
	if epc == "" {
		return nil, nil
	}
	rec, err := h.App.FindFirstRecordByFilter("item_instances", "rfid_epc = {:epc}", dbx.Params{"epc": epc})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h.instanceMatchFromRecord(rec)
}

// instanceMatchFromRecord loads the instance's parent item so the caller has
// the full picture in a single round-trip.
func (h *Handlers) instanceMatchFromRecord(rec *core.Record) (*scan.InstanceMatch, error) {
	itemRec, err := h.App.FindRecordById("items", rec.GetString("item"))
	if err != nil {
		return nil, err
	}
	return &scan.InstanceMatch{
		Instance: &scan.ItemInstance{
			ID:      rec.Id,
			ItemID:  rec.GetString("item"),
			Code:    rec.GetString("code"),
			Serial:  rec.GetString("serial"),
			RFIDEPC: rec.GetString("rfid_epc"),
			Active:  rec.GetBool("active"),
			Notes:   rec.GetString("notes"),
		},
		Item: itemFromRecord(itemRec),
	}, nil
}
