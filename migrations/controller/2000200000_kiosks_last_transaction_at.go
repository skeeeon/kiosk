package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds kiosks.last_transaction_at — a date field that captures the most
// recent transaction.complete event from each kiosk. Pairs with the
// touchKiosk narrowing in internal/controller/consumer.go: liveness moves
// to the in-memory heartbeat registry, so the persisted timestamp can
// finally mean what its name says — "when did this kiosk last actually
// transact?" — rather than "last event of any kind."
//
// The old `last_seen` field stays put for one release. touchKiosk writes
// both columns; the SPA reads last_transaction_at; a future migration
// drops last_seen once the field has aged out of downstream consumers.

func init() {
	m.Register(addKiosksLastTransactionAtUp, addKiosksLastTransactionAtDown)
}

func addKiosksLastTransactionAtUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		// Controller hasn't created the collection yet — should not
		// happen since migrations apply in timestamp order, but treat
		// as a no-op to keep the runner robust.
		return nil
	}
	if col.Fields.GetByName("last_transaction_at") == nil {
		col.Fields.Add(&core.DateField{Name: "last_transaction_at"})
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save kiosks (add last_transaction_at): %w", err)
	}
	return nil
}

func addKiosksLastTransactionAtDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("last_transaction_at") != nil {
		col.Fields.RemoveByName("last_transaction_at")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save kiosks (drop last_transaction_at): %w", err)
	}
	return nil
}
