// Drops kiosks.last_seen. It was the legacy "last event of any kind"
// timestamp; live online status moved to the heartbeat registry and the SPA
// reads last_transaction_at. The one-release deprecation window opened by
// migration 2000200000 has closed and nothing reads or writes the field
// anymore.
//
// Additive history: migration 2000000000 still creates the field on a fresh
// DB and this one drops it — a deliberate create-then-drop rather than
// editing an already-applied migration (same pattern as 2000800000's
// applied_oc_closes drop).
package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(dropKiosksLastSeenUp, dropKiosksLastSeenDown)
}

func dropKiosksLastSeenUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		return fmt.Errorf("find kiosks collection: %w", err)
	}
	if col.Fields.GetByName("last_seen") == nil {
		return nil // already gone
	}
	col.Fields.RemoveByName("last_seen")
	if err := app.Save(col); err != nil {
		return fmt.Errorf("drop kiosks.last_seen: %w", err)
	}
	return nil
}

func dropKiosksLastSeenDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("kiosks")
	if err != nil {
		return fmt.Errorf("find kiosks collection: %w", err)
	}
	if col.Fields.GetByName("last_seen") != nil {
		return nil
	}
	col.Fields.Add(&core.DateField{Name: "last_seen"})
	return app.Save(col)
}
