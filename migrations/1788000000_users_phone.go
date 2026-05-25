package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds an optional `phone` text field to users. Direct-contact channel for
// reaching a specific worker (e.g. "call Bob about the impact driver");
// group.contact_phone covers foreman/crew reachability, this covers the
// person. Not surfaced as a list column to keep the workers table dense;
// edited via the worker dialog.

func init() {
	m.Register(addUsersPhoneUp, addUsersPhoneDown)
}

func addUsersPhoneUp(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}
	if users.Fields.GetByName("phone") == nil {
		users.Fields.Add(&core.TextField{Name: "phone"})
	}
	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}

func addUsersPhoneDown(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}
	if users.Fields.GetByName("phone") != nil {
		users.Fields.RemoveByName("phone")
	}
	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}
