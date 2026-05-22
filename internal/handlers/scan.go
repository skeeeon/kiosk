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
//
// The result is enriched with identify metadata after resolution:
// user → open-checkout count; item → current holder + open count. This
// lets the splash screen render a useful identify panel and the badge-in
// flash include "X items out" without extra round-trips from the SPA.
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
			ItemInstanceByCode: h.scanInstanceByCode,
			ItemInstanceByRFID: h.scanInstanceByRFID,
		},
	}
	result := r.Resolve(body.Value)
	h.enrichScanResult(&result)
	return re.JSON(http.StatusOK, result)
}

// enrichScanResult fills the optional identify fields the SPA consumes on
// the splash and badge-in flows. Failures are swallowed: identify is a
// nice-to-have, never a reason to fail a scan.
func (h *Handlers) enrichScanResult(r *scan.Result) {
	switch r.Type {
	case scan.ResultUser:
		if u, ok := r.Record.(*scan.User); ok && u != nil {
			u.OpenCount = h.countOpenCheckoutsForUser(u.ID)
		}
	case scan.ResultItem:
		if it, ok := r.Record.(*scan.Item); ok && it != nil {
			it.OpenCount, it.Holder = h.openCheckoutSummaryForItem(it.ID, "")
		}
	case scan.ResultItemInstance:
		if m, ok := r.Record.(*scan.InstanceMatch); ok && m != nil && m.Item != nil {
			// Instance scans show "is this specific unit out?" — narrow the
			// summary to the instance so the holder name reflects the unit
			// the worker just scanned.
			m.Item.OpenCount, m.Item.Holder = h.openCheckoutSummaryForItem(m.Item.ID, m.Instance.ID)
		}
	}
}

func (h *Handlers) countOpenCheckoutsForUser(userID string) int {
	n, _ := h.App.CountRecords("open_checkouts", dbx.HashExp{"user": userID})
	return int(n)
}

// openCheckoutSummaryForItem returns (total open count, representative holder
// name). instanceID may be empty for quantity-tracked items; when set, the
// summary is scoped to that exact unit. The holder is the name of one
// arbitrary holder when the count is non-zero — the count tells the caller
// whether there are others.
func (h *Handlers) openCheckoutSummaryForItem(itemID, instanceID string) (int, string) {
	filter := "item = {:item}"
	params := dbx.Params{"item": itemID}
	if instanceID != "" {
		filter = "item_instance = {:inst}"
		params = dbx.Params{"inst": instanceID}
	}
	rows, err := h.App.FindRecordsByFilter("open_checkouts", filter, "-checked_out_at", 1, 0, params)
	if err != nil || len(rows) == 0 {
		return 0, ""
	}
	total, err := h.App.CountRecords("open_checkouts", dbx.HashExp{"item": itemID})
	if err != nil {
		total = int64(len(rows))
	}
	if instanceID != "" {
		// Scoped count for instance scans — at most one row, but recompute
		// so callers see a consistent shape.
		total = int64(len(rows))
	}
	var holderName string
	if u, e := h.App.FindRecordById("users", rows[0].GetString("user")); e == nil && u != nil {
		holderName = u.GetString("name")
	}
	return int(total), holderName
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
