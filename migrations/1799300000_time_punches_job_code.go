package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds time_punches.job_code: an optional free-text job / work-order number
// tagging the hours clocked. Attached to the clock-in punch (display pairing
// carries it onto the interval); stored on whatever punch a caller supplies it
// on, never validated. Defaults empty so existing punches are unaffected. An
// optional attribution column on an append-only ledger — the same shape as
// transactions.door_id. Idempotent.
//
// Shared kiosk migration → the column also lands in the controller and virtual
// timeclock DBs (both blank-import package migrations), where the aggregator's
// punch projection writes the fleet's job codes through.

func init() {
	m.Register(timePunchesJobCodeUp, timePunchesJobCodeDown)
}

func timePunchesJobCodeUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("time_punches")
	if err != nil {
		return fmt.Errorf("find time_punches: %w", err)
	}
	if col.Fields.GetByName("job_code") != nil {
		return nil
	}
	col.Fields.Add(&core.TextField{Name: "job_code"})
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add time_punches.job_code: %w", err)
	}
	return nil
}

func timePunchesJobCodeDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("time_punches")
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("job_code") != nil {
		col.Fields.RemoveByName("job_code")
		if err := app.Save(col); err != nil {
			return fmt.Errorf("drop time_punches.job_code: %w", err)
		}
	}
	return nil
}
