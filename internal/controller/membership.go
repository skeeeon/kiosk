package controller

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// KiosksForItem returns the kiosk records that stock the given item — one
// row per (kiosk, item) membership row, resolved to the kiosk record.
// Empty slice when nothing matches.
func KiosksForItem(app core.App, itemID string) ([]*core.Record, error) {
	if itemID == "" {
		return nil, nil
	}
	rows, err := app.FindRecordsByFilter("kiosk_items",
		"item = {:item}", "", 0, 0, dbx.Params{"item": itemID})
	if err != nil {
		return nil, fmt.Errorf("find kiosk_items by item: %w", err)
	}
	out := make([]*core.Record, 0, len(rows))
	for _, r := range rows {
		kioskID := r.GetString("kiosk")
		if kioskID == "" {
			continue
		}
		k, err := app.FindRecordById("kiosks", kioskID)
		if err != nil {
			// Stale FK should be impossible (CascadeDelete on the relation),
			// but don't blow up the whole publish if it happens.
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

// ItemsForKiosk returns the items records stocked at the given kiosk.
func ItemsForKiosk(app core.App, kioskID string) ([]*core.Record, error) {
	if kioskID == "" {
		return nil, nil
	}
	rows, err := app.FindRecordsByFilter("kiosk_items",
		"kiosk = {:kiosk}", "", 0, 0, dbx.Params{"kiosk": kioskID})
	if err != nil {
		return nil, fmt.Errorf("find kiosk_items by kiosk: %w", err)
	}
	out := make([]*core.Record, 0, len(rows))
	for _, r := range rows {
		itemID := r.GetString("item")
		if itemID == "" {
			continue
		}
		it, err := app.FindRecordById("items", itemID)
		if err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}
