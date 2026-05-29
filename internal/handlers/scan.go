package handlers

import (
	"net/http"
	"strings"

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
			// Always populate the hydrated list — even at modest counts (5+)
			// it's tens of small rows, well below "premature pagination" size.
			// The SPA's worker self-service panel renders straight from this
			// without a second round trip.
			u.OpenCheckouts = h.openCheckoutsForUser(u.ID)
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

// openCheckoutsForUser returns the worker's outstanding rows hydrated with
// item code/name and instance serial. Ordered oldest-first so the SPA panel
// surfaces what's been out longest at the top. Returns nil on any error —
// the scan reply gracefully falls back to OpenCount-only rendering.
func (h *Handlers) openCheckoutsForUser(userID string) []scan.OpenCheckoutDetail {
	rows, err := h.App.FindRecordsByFilter("open_checkouts",
		"user = {:u}", "checked_out_at", 0, 0,
		dbx.Params{"u": userID})
	if err != nil || len(rows) == 0 {
		return nil
	}

	// Bulk-resolve items + instances to avoid N+1. Tens of rows max in
	// practice, so a single per-row Find is also fine — but this is more
	// future-proof and reads as cleanly.
	itemIDs := map[string]struct{}{}
	instanceIDs := map[string]struct{}{}
	for _, r := range rows {
		if id := r.GetString("item"); id != "" {
			itemIDs[id] = struct{}{}
		}
		if id := r.GetString("item_instance"); id != "" {
			instanceIDs[id] = struct{}{}
		}
	}
	itemByID := h.bulkFindByIDs("items", itemIDs)
	instanceByID := h.bulkFindByIDs("item_instances", instanceIDs)

	out := make([]scan.OpenCheckoutDetail, 0, len(rows))
	for _, r := range rows {
		d := scan.OpenCheckoutDetail{
			ID:           r.Id,
			ItemID:       r.GetString("item"),
			Qty:          1,
			CheckedOutAt: r.GetDateTime("checked_out_at").String(),
		}
		if item := itemByID[d.ItemID]; item != nil {
			d.ItemCode = item.GetString("code")
			d.ItemName = item.GetString("name")
		}
		if instID := r.GetString("item_instance"); instID != "" {
			d.ItemInstanceID = instID
			if inst := instanceByID[instID]; inst != nil {
				d.ItemInstanceCode = inst.GetString("code")
				d.InstanceSerial = inst.GetString("serial")
			}
		}
		out = append(out, d)
	}
	return out
}

// bulkFindByIDs returns id → *Record for the supplied id set. Empty set
// short-circuits to an empty map; misses are silently omitted from the map.
func (h *Handlers) bulkFindByIDs(collection string, ids map[string]struct{}) map[string]*core.Record {
	out := map[string]*core.Record{}
	if len(ids) == 0 {
		return out
	}
	expr := ""
	params := dbx.Params{}
	i := 0
	for id := range ids {
		key := "i" + itoa(i)
		if expr != "" {
			expr += " || "
		}
		expr += "id = {:" + key + "}"
		params[key] = id
		i++
	}
	rows, err := h.App.FindRecordsByFilter(collection, expr, "", 0, 0, params)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.Id] = r
	}
	return out
}

// itoa is a tiny strconv.Itoa shim so the file doesn't pull strconv in for
// one call site. The id counts here are single-digit; %d formatting via
// fmt.Sprintf would also work but adds a heavier import.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
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
	// rfid_epc is stored lower-case (normalized on write + backfilled). Fold
	// the lookup key to match, so an upper-case scanned/printed EPC resolves.
	epc = strings.ToLower(strings.TrimSpace(epc))
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
