package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// PocketBase blocks direct writes to an auth record's `email` field unless
// the request has "manage access" — granted only when the caller is a PB
// superuser or the collection's ManageRule matches. The init migration set
// list/view/create/update/delete on `users` to adminRule but never set
// ManageRule, so SPA admins (who aren't PB superusers) saw the email field
// rejected with validation_values_mismatch on every save.
//
// Same story for `admins`: 1782 relaxed the CRUD rules to adminRule but
// left ManageRule unset, so admins editing each other's email (or their
// own through the standard update endpoint) hit the same wall.
//
// Setting ManageRule = adminRule on both collections is the surgical fix —
// it grants the upsert form manager-level access for admin callers without
// touching the row-level rules.

func init() {
	m.Register(addAuthManageRulesUp, addAuthManageRulesDown)
}

func addAuthManageRulesUp(app core.App) error {
	for _, name := range []string{"users", "admins"} {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return fmt.Errorf("find %s: %w", name, err)
		}
		rule := adminRule
		col.ManageRule = &rule
		if err := app.Save(col); err != nil {
			return fmt.Errorf("save %s: %w", name, err)
		}
	}
	return nil
}

func addAuthManageRulesDown(app core.App) error {
	for _, name := range []string{"users", "admins"} {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			continue
		}
		col.ManageRule = nil
		if err := app.Save(col); err != nil {
			return fmt.Errorf("save %s: %w", name, err)
		}
	}
	return nil
}
