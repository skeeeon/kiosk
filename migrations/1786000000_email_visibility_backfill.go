package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Sets emailVisibility=true on every existing users + admins row. PB's
// default is false, which masks the email field in API responses to
// non-owners — including admins viewing each other's records from the
// SPA. The kiosk's admin views need email back for both list rendering
// and the "last edited by <email>" affordance on notification templates.
//
// New rows are stamped emailVisibility=true by an OnRecordCreate hook
// installed at process boot (see internal/authfix). This migration only
// fixes the historical population.
//
// Down restores the PB default by setting emailVisibility=false on every
// row — same shape, opposite value — so a rollback returns the schema to
// its pre-migration semantics. Operators rarely need the down path; it's
// here for dev-loop symmetry.

func init() {
	m.Register(emailVisibilityBackfillUp, emailVisibilityBackfillDown)
}

func emailVisibilityBackfillUp(app core.App) error {
	return setEmailVisibilityAll(app, true)
}

func emailVisibilityBackfillDown(app core.App) error {
	return setEmailVisibilityAll(app, false)
}

func setEmailVisibilityAll(app core.App, visible bool) error {
	for _, name := range []string{"users", "admins"} {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			// Collection missing — likely a partial test environment. Skip
			// rather than fail the whole migration.
			continue
		}
		rows, err := app.FindRecordsByFilter(col.Name, "", "", 0, 0)
		if err != nil {
			return fmt.Errorf("list %s: %w", col.Name, err)
		}
		for _, r := range rows {
			if r.EmailVisibility() == visible {
				continue
			}
			r.SetEmailVisibility(visible)
			if err := app.Save(r); err != nil {
				return fmt.Errorf("save %s/%s: %w", col.Name, r.Id, err)
			}
		}
	}
	return nil
}
