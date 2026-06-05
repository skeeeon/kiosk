package controller

import (
	"encoding/json"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// Snapshot enrichment. The kiosk's inventory.snapshot / instance.snapshot
// replies now carry `out` directly (read from the kiosk's own open_checkouts —
// the authoritative source), so the controller no longer replays its projected
// ledger to learn what's out. Inventory replies still don't carry item `type`
// (tool/consumable), so the controller adds that from its catalog. The only
// replay left here is a transitional fallback for a pre-rollout kiosk whose
// snapshot omits `out`; once the fleet is upgraded it never runs. These panels
// are online-only (fetchKioskData 503s an offline kiosk), so there's no
// offline-fallback concern — that lives in the report/digest gather paths.
//
// Replies are decoded as maps so any field the kiosk adds later survives the
// round-trip untouched, and so we can detect whether `out` is present.

// enrichInventorySnapshot adds `type` (tool/consumable, from the controller's
// catalog) to each item, and — only for a pre-rollout kiosk that omits it —
// backfills `out` from the projected ledger.
func enrichInventorySnapshot(app core.App, kioskCode string, raw json.RawMessage) (any, error) {
	var reply struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	hasOut := len(reply.Items) > 0
	if hasOut {
		_, hasOut = reply.Items[0]["out"]
	}
	var (
		codeToType map[string]string
		outByCode  map[string]int
		err        error
	)
	if hasOut {
		codeToType, err = catalogCodeToType(app) // cheap: no replay
	} else {
		outByCode, codeToType, err = inventoryLedgerFacts(app, kioskCode) // fallback: replay
	}
	if err != nil {
		return nil, err
	}
	for _, it := range reply.Items {
		code, _ := it["item_code"].(string)
		it["type"] = codeToType[code] // "" if the catalog hasn't synced it yet
		if !hasOut {
			it["out"] = outByCode[code]
		}
	}
	return reply, nil
}

// enrichInstanceSnapshot is a pass-through once the kiosk ships `out` per
// instance; for a pre-rollout kiosk that omits it, it backfills out-status from
// the projected ledger's open-rows set.
func enrichInstanceSnapshot(app core.App, kioskCode string, raw json.RawMessage) (any, error) {
	var reply struct {
		Instances []map[string]any `json:"instances"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	if len(reply.Instances) == 0 {
		return reply, nil
	}
	if _, ok := reply.Instances[0]["out"]; ok {
		return reply, nil // kiosk already shipped out-status
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

// catalogCodeToType returns the controller catalog's item code→type map with
// no ledger replay — used to decorate snapshots whose `out` the kiosk already
// shipped (the kiosk's reply doesn't carry item `type`).
func catalogCodeToType(app core.App) (map[string]string, error) {
	rows, err := app.FindRecordsByFilter("items", "", "", 0, 0)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.GetString("code")] = r.GetString("type")
	}
	return m, nil
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
