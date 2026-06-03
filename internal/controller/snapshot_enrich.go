package controller

import (
	"encoding/json"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// Snapshot enrichment. The kiosk's inventory.snapshot / instance.snapshot
// replies carry kiosk-local mutable state (on-hand, thresholds, instance
// metadata) but not "what's currently out" — the kiosk knows that from its
// open_checkouts table, and the snapshot doesn't ship it. Rather than grow the
// wire protocol, the controller derives "out" from its OWN projected ledger
// (ledger.ReplayOpenRows — the same convergent source behind the Currently-out
// and low-stock views) and decorates the reply with it. The SPA then computes
// available + low-stock from on-hand + out + type exactly as the local kiosk
// Items view does, so the controller's per-kiosk panels stay in lockstep with
// the kiosk's own screen and with the controller's other "out" numbers.
//
// Replies are decoded as maps so any field the kiosk adds later survives the
// round-trip untouched.

// enrichInventorySnapshot adds `out` (units currently out, from the ledger)
// and `type` (tool/consumable, from the controller's catalog) to each item.
func enrichInventorySnapshot(app core.App, kioskCode string, raw json.RawMessage) (any, error) {
	var reply struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	outByCode, codeToType, err := inventoryLedgerFacts(app, kioskCode)
	if err != nil {
		return nil, err
	}
	for _, it := range reply.Items {
		code, _ := it["item_code"].(string)
		it["out"] = outByCode[code]   // 0 when nothing is out for this item
		it["type"] = codeToType[code] // "" if the catalog hasn't synced it yet
	}
	return reply, nil
}

// enrichInstanceSnapshot marks each instance row with whether it's currently
// out, by membership in the ledger's open-rows set.
func enrichInstanceSnapshot(app core.App, kioskCode string, raw json.RawMessage) (any, error) {
	var reply struct {
		Instances []map[string]any `json:"instances"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	open, err := ledger.ReplayOpenRows(app, kioskCode)
	if err != nil {
		return nil, err
	}
	outIDs := make(map[string]struct{}, len(open))
	for _, o := range open {
		if o.ItemInstance != "" {
			outIDs[o.ItemInstance] = struct{}{}
		}
	}
	for _, inst := range reply.Instances {
		id, _ := inst["instance_id"].(string)
		_, out := outIDs[id]
		inst["out"] = out
	}
	return reply, nil
}

// inventoryLedgerFacts loads the controller's catalog once and replays the
// kiosk's projected ledger, returning the per-item-code "out" count and a
// code→type map. Both are controller-local (no kiosk round-trip). The "out"
// count comes from the same ReplayOpenRows the Currently-out / low-stock views
// use, so every controller "out" number agrees by construction.
func inventoryLedgerFacts(app core.App, kioskCode string) (outByCode map[string]int, codeToType map[string]string, err error) {
	rows, err := app.FindRecordsByFilter("items", "", "", 0, 0)
	if err != nil {
		return nil, nil, err
	}
	idToCode := make(map[string]string, len(rows))
	codeToType = make(map[string]string, len(rows))
	for _, r := range rows {
		code := r.GetString("code")
		idToCode[r.Id] = code
		codeToType[code] = r.GetString("type")
	}
	open, err := ledger.ReplayOpenRows(app, kioskCode)
	if err != nil {
		return nil, nil, err
	}
	outByCode = make(map[string]int)
	for _, o := range open {
		if code, ok := idToCode[o.Item]; ok {
			outByCode[code]++
		}
	}
	return outByCode, codeToType, nil
}
