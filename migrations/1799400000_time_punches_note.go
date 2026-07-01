package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds time_punches.note: an optional free-text annotation on a punch, usable
// by ANY source (unlike `reason`, which is required only for admin/corrective
// punches). A worker can leave context on a self punch — "left early, dentist"
// — without needing admin powers. Defaults empty so existing punches are
// unaffected. An optional attribution column on an append-only ledger — the
// same shape as job_code / transactions.door_id. Never validated. Idempotent.
//
// Shared kiosk migration → the column also lands in the controller and virtual
// timeclock DBs (both blank-import package migrations), where the aggregator's
// punch projection writes the fleet's notes through.

func init() {
	m.Register(timePunchesNoteUp, timePunchesNoteDown)
}

func timePunchesNoteUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("time_punches")
	if err != nil {
		return fmt.Errorf("find time_punches: %w", err)
	}
	if col.Fields.GetByName("note") != nil {
		return nil
	}
	col.Fields.Add(&core.TextField{Name: "note"})
	if err := app.Save(col); err != nil {
		return fmt.Errorf("add time_punches.note: %w", err)
	}
	return nil
}

func timePunchesNoteDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("time_punches")
	if err != nil {
		return nil
	}
	if col.Fields.GetByName("note") != nil {
		col.Fields.RemoveByName("note")
		if err := app.Save(col); err != nil {
			return fmt.Errorf("drop time_punches.note: %w", err)
		}
	}
	return nil
}
