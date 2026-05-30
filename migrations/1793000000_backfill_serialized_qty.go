package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Backfills items.quantity_on_hand for SERIALIZED items to the count of their
// active item_instances.
//
// As of this release, a serialized item's quantity_on_hand is a derived value
// (a materialized view of "how many physical units are in service"), kept in
// sync going forward by the instances recompute hook in
// internal/instances/hooks.go. Before that, the field was an independent,
// hand-edited number that could drift arbitrarily from the real instance
// count. This one-shot pass reconciles existing rows so the stored value
// matches the active count from day one.
//
// Raw UPDATE rather than per-record Save: avoids re-triggering the recompute
// hook mid-migration and computes every item in a single statement.
//
// Shared migration: the controller's items never carry kiosk-local
// quantity_on_hand (catalog sync excludes it) and its item_instances table is
// always empty, so serialized rows there resolve to 0 — harmless.

func init() {
	m.Register(backfillSerializedQtyUp, backfillSerializedQtyDown)
}

func backfillSerializedQtyUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("items"); err != nil {
		return nil
	}
	if _, err := app.FindCollectionByNameOrId("item_instances"); err != nil {
		// Instances collection not present yet — nothing to derive from.
		return nil
	}
	if _, err := app.DB().NewQuery(
		"UPDATE items SET quantity_on_hand = (" +
			"SELECT COUNT(*) FROM item_instances " +
			"WHERE item_instances.item = items.id AND item_instances.active = 1" +
			") WHERE tracking_mode = 'serialized'",
	).Execute(); err != nil {
		return fmt.Errorf("backfill serialized quantity_on_hand: %w", err)
	}
	return nil
}

func backfillSerializedQtyDown(app core.App) error {
	// Irreversible: the pre-backfill (possibly drifted) values aren't
	// recoverable. No-op so a down migration doesn't fail.
	return nil
}
