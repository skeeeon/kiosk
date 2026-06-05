package instances

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
)

// quantity.go keeps items.quantity_on_hand in sync with the count of
// non-retired item_instances for SERIALIZED items. For a serialized SKU the
// stored quantity is a materialized view of "how many physical units we still
// own" (in_service + maintenance — a unit on the bench is still owned, just not
// available) — the same shape as open_checkouts being a materialized view of
// the ledger. "Available" (in_service − out) is derived in the SPA, not here.
// Recompute (a full re-count from source of truth, never an increment) runs
// from the item_instances after-success hooks in hooks.go, so it covers every
// write path: the admin SPA / superuser REST writes, the controller
// command-bus mutations (Perform*), commit's maintenance-on-return, and
// admin_close's retire. Quantity-tracked tools and consumables are never
// touched — their quantity_on_hand is authoritative, owned by the stock-adjust
// + commit-consume paths.

// CountNonRetired returns the number of non-retired (in_service + maintenance)
// item_instances for an item id.
func CountNonRetired(app core.App, itemID string) (int, error) {
	if itemID == "" {
		return 0, nil
	}
	n, err := app.CountRecords("item_instances",
		dbx.NewExp("item = {:item} AND status != {:retired}",
			dbx.Params{"item": itemID, "retired": StatusRetired}))
	if err != nil {
		return 0, fmt.Errorf("count non-retired item_instances for %s: %w", itemID, err)
	}
	return int(n), nil
}

// RecomputeItemQuantity sets items.quantity_on_hand = CountNonRetired for a
// SERIALIZED item. It is a no-op when:
//
//   - itemID is empty,
//   - the item no longer exists (cascade delete of the parent item voids its
//     instances — there is no quantity left to maintain),
//   - the item is not serialized (quantity/consumable items own their stored
//     stock; recompute must never clobber it),
//   - the stored quantity already equals the active count (skip the write).
//
// Because the serialized + existence checks live inside this function, every
// call site can invoke it unconditionally with just an item id and trust it to
// do the right thing.
func RecomputeItemQuantity(app core.App, itemID string) error {
	if itemID == "" {
		return nil
	}
	item, err := app.FindRecordById("items", itemID)
	if err != nil {
		if dberr.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("find item %s: %w", itemID, err)
	}
	if item.GetString("tracking_mode") != "serialized" {
		return nil
	}
	n, err := CountNonRetired(app, itemID)
	if err != nil {
		return err
	}
	if item.GetInt("quantity_on_hand") == n {
		return nil
	}
	item.Set("quantity_on_hand", n)
	if err := app.Save(item); err != nil {
		return fmt.Errorf("save recomputed quantity_on_hand for %s: %w", itemID, err)
	}
	return nil
}
