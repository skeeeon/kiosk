package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// PB 0.22+ no longer adds `created`/`updated` to base collections automatically;
// the initial migration declared none, so attempts to sort or filter by them
// against `transaction_lines` (or any other base collection) return 404 from
// PB's records endpoint. This backfills them where they're useful:
//
//   created: every collection — gives admins a stable insertion-time ordering
//            independent of PB's id format.
//   updated: collections whose records can change after creation (items,
//            users, admins, transactions). Skipped on transaction_lines and
//            open_checkouts: both are write-once from the commit hook's
//            perspective (the uncorrelated flag is set in the same write, and
//            open_checkouts is only inserted/deleted, never mutated).
//
// Idempotent: existing fields with the same name are left alone, so re-running
// against a half-applied state is safe.

func init() {
	m.Register(addAutodateFieldsUp, addAutodateFieldsDown)
}

type autodateSpec struct {
	collection string
	addUpdated bool
}

var autodateTargets = []autodateSpec{
	{"users", true},
	{"admins", true},
	{"items", true},
	{"transactions", true},
	{"transaction_lines", false},
	{"open_checkouts", false},
}

func addAutodateFieldsUp(app core.App) error {
	for _, s := range autodateTargets {
		col, err := app.FindCollectionByNameOrId(s.collection)
		if err != nil {
			return fmt.Errorf("find %s: %w", s.collection, err)
		}

		changed := false
		if col.Fields.GetByName("created") == nil {
			col.Fields.Add(&core.AutodateField{
				Name:     "created",
				OnCreate: true,
			})
			changed = true
		}
		if s.addUpdated && col.Fields.GetByName("updated") == nil {
			col.Fields.Add(&core.AutodateField{
				Name:     "updated",
				OnCreate: true,
				OnUpdate: true,
			})
			changed = true
		}

		if changed {
			if err := app.Save(col); err != nil {
				return fmt.Errorf("save %s: %w", s.collection, err)
			}
		}
	}
	return nil
}

func addAutodateFieldsDown(app core.App) error {
	for _, s := range autodateTargets {
		col, err := app.FindCollectionByNameOrId(s.collection)
		if err != nil {
			continue
		}

		changed := false
		if f := col.Fields.GetByName("created"); f != nil {
			if _, ok := f.(*core.AutodateField); ok {
				col.Fields.RemoveByName("created")
				changed = true
			}
		}
		if s.addUpdated {
			if f := col.Fields.GetByName("updated"); f != nil {
				if _, ok := f.(*core.AutodateField); ok {
					col.Fields.RemoveByName("updated")
					changed = true
				}
			}
		}

		if changed {
			if err := app.Save(col); err != nil {
				return fmt.Errorf("save %s: %w", s.collection, err)
			}
		}
	}
	return nil
}
