// Package authfix holds small cross-binary hooks that adjust PB's default
// auth semantics to match this app's needs. Each function takes a core.App
// and is safe to call once at process boot — registering the same hook
// twice would double-fire on every event.
package authfix

import (
	"github.com/pocketbase/pocketbase/core"
)

// EnforceEmailVisibility installs an OnRecordCreate hook on the users and
// admins auth collections that stamps emailVisibility=true on every new
// row before save. PB defaults the field to false, which masks the email
// in API responses to anyone who isn't the record owner — breaking the
// admin SPA's worker/admin lists and the "last edited by <email>" footer
// on notification templates.
//
// Only fires on Create. Update paths leave existing emailVisibility values
// alone so an operator who explicitly chooses to mask their email later
// isn't fought with. Existing rows pre-dating this hook are covered by
// the 1786000000_email_visibility_backfill migration.
func EnforceEmailVisibility(app core.App) {
	app.OnRecordCreate("users", "admins").BindFunc(func(e *core.RecordEvent) error {
		e.Record.SetEmailVisibility(true)
		return e.Next()
	})
}
