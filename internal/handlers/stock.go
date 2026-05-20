package handlers

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// availableForItem reports how many units of an item are available to take
// right now.
//
//   For tools, quantity_on_hand is the fleet count (total owned), and the
//   open_checkouts table holds one row per unit currently out. Available is
//   the difference, clamped at zero on the floor (negative would mean more
//   units are out than the kiosk thinks it owns — the integrity check
//   surfaces that; here we just don't pretend you can take a 6th of 5).
//
//   For consumables, quantity_on_hand IS the available stock — the commit
//   hook decrements it on each consume — and open_checkouts is not involved.
func availableForItem(app core.App, item *core.Record) (int, error) {
	onHand := item.GetInt("quantity_on_hand")
	if item.GetString("type") == "consumable" {
		return onHand, nil
	}
	open, err := openCheckoutCountForItem(app, item.Id)
	if err != nil {
		return 0, err
	}
	available := onHand - open
	if available < 0 {
		available = 0
	}
	return available, nil
}

// openCheckoutCountForItem returns the number of rows in open_checkouts
// belonging to the given item id (across all users).
func openCheckoutCountForItem(app core.App, itemID string) (int, error) {
	n, err := app.CountRecords("open_checkouts", dbx.HashExp{"item": itemID})
	if err != nil {
		return 0, fmt.Errorf("count open_checkouts for item %s: %w", itemID, err)
	}
	return int(n), nil
}

// setLowStockWarning returns the warnings slice with any prior low_stock:*
// entry replaced by the given one (or removed when w is ""). Other warnings
// (cross_user_return:*, etc.) are preserved in their original order.
func setLowStockWarning(warnings []string, w string) []string {
	out := warnings[:0:0]
	for _, x := range warnings {
		if len(x) >= len("low_stock:") && x[:len("low_stock:")] == "low_stock:" {
			continue
		}
		out = append(out, x)
	}
	if w != "" {
		out = append(out, w)
	}
	return out
}

// lowStockWarning returns a "low_stock:available=N" warning string if the
// requested qty for the given action would exceed available stock, or "" if
// no warning is needed. Returns are exempt: returning a tool can't reduce
// availability of that tool.
func lowStockWarning(app core.App, item *core.Record, action string, qty int) (string, error) {
	if qty <= 0 || action == "return" {
		return "", nil
	}
	available, err := availableForItem(app, item)
	if err != nil {
		return "", err
	}
	if qty > available {
		return fmt.Sprintf("low_stock:available=%d", available), nil
	}
	return "", nil
}
