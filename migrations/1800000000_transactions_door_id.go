package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds transactions.door_id: an optional per-transaction attribution tag for
// which physical door/terminal a checkout was completed at. Empty for the
// common single-kiosk case; populated either by the RFID enclosure_diff flow
// (cart.DoorID, set at StartByExternal) or by a manual terminal supplying it
// on the commit request (?door= URL param). Pure attribution — never gates
// auth or correlation — so it's nullable and non-breaking. Idempotent.

func init() {
	m.Register(transactionsDoorIDUp, transactionsDoorIDDown)
}

func transactionsDoorIDUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}
	if col.Fields.GetByName("door_id") == nil {
		col.Fields.Add(&core.TextField{Name: "door_id"})
	}
	if !hasIndex(col, "idx_transactions_door_id") {
		col.AddIndex("idx_transactions_door_id", false, "door_id", "")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add transactions.door_id: %w", err)
	}
	return nil
}

func transactionsDoorIDDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return nil
	}
	removeIndex(col, "idx_transactions_door_id")
	if col.Fields.GetByName("door_id") != nil {
		col.Fields.RemoveByName("door_id")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop transactions.door_id: %w", err)
	}
	return nil
}
