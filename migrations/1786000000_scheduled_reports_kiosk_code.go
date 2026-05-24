// Adds kiosk_code to scheduled_reports. Empty = fleet-wide (or "this
// kiosk" on a standalone deployment where there's only one); a specific
// code scopes a schedule to one kiosk in the fleet.
//
// Shared migration (runs on both binaries via init()). Standalone kiosks
// never populate it — the value defaults to empty and the runner reads
// the local DB as before. The controller binary owns the scheduler in
// managed mode and the SPA exposes a dropdown when running there.
package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(addScheduledReportsKioskCodeUp, addScheduledReportsKioskCodeDown)
}

func addScheduledReportsKioskCodeUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("scheduled_reports")
	if err != nil {
		// Base collection migration hasn't run yet — should not happen
		// since migrations apply in timestamp order, but stay robust.
		return nil
	}
	if col.Fields.GetByName("kiosk_code") == nil {
		col.Fields.Add(&core.TextField{Name: "kiosk_code"})
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save scheduled_reports (add kiosk_code): %w", err)
	}
	return nil
}

func addScheduledReportsKioskCodeDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("scheduled_reports")
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("kiosk_code") != nil {
		col.Fields.RemoveByName("kiosk_code")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save scheduled_reports (drop kiosk_code): %w", err)
	}
	return nil
}
