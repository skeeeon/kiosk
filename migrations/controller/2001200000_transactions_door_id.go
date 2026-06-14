// Adds door_id to the controller's projected transactions, mirroring the
// kiosk-side column (migration 1800000000). Carries the per-door/terminal
// attribution tag from the transaction.complete event so fleet-level
// transaction views and the CSV export keep the same fidelity as a local
// kiosk. Optional + non-unique index, idempotent.
package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

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
		return fmt.Errorf("save transactions (door_id): %w", err)
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
		return fmt.Errorf("save transactions (drop door_id): %w", err)
	}
	return nil
}
