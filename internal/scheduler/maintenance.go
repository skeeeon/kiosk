package scheduler

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// runMaintenanceDigest is the STANDALONE maintenance-digest runner: it lists
// the kiosk's own item_instances currently in maintenance and hydrates each
// with its item code/name. Mirrors instances.Snapshot's hydration.
//
// On the controller this runner is overridden (scheduler.RegisterRunner) with
// a fleet-wide snapshot fan-out, because the controller's local item_instances
// table is empty — instances live on the kiosks. See
// internal/controller/reports_maintenance.go.
//
// An empty result returns a populated context with zero rows (not an error)
// so the digest still fires and the template renders its "nothing in
// maintenance" branch — same contract as runOpenCheckoutsDigest.
func runMaintenanceDigest(app core.App, row *core.Record) (string, any, error) {
	rowKioskCode := row.GetString("kiosk_code")

	insts, err := app.FindRecordsByFilter("item_instances",
		"status = {:s}", "code", 0, 0, dbx.Params{"s": "maintenance"})
	if err != nil {
		return "", nil, fmt.Errorf("maintenance digest: list instances: %w", err)
	}

	rows := make([]notifications.MaintenanceDigestRow, 0, len(insts))
	itemCache := map[string]*core.Record{}
	for _, r := range insts {
		itemID := r.GetString("item")
		item, ok := itemCache[itemID]
		if !ok {
			item, _ = app.FindRecordById("items", itemID)
			itemCache[itemID] = item
		}
		dr := notifications.MaintenanceDigestRow{
			InstanceCode: r.GetString("code"),
			Serial:       r.GetString("serial"),
			Notes:        r.GetString("notes"),
		}
		if item != nil {
			dr.ItemCode = item.GetString("code")
			dr.ItemName = item.GetString("name")
		}
		rows = append(rows, dr)
	}

	ctx := notifications.MaintenanceDigestContext{
		Kiosk:       digestKioskInfo(rowKioskCode),
		GeneratedAt: time.Now().UTC(),
		Rows:        rows,
		RowsCount:   len(rows),
	}
	return notifications.EventTypeMaintenanceDigest, ctx, nil
}
