package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds optional `group` to users (e.g. "electrical", "hvac") and a matching
// `user_group` snapshot to transactions. Groups let a single tool crib serve
// multiple trades on one site: cross-user returns are constrained to the
// foreman's own group at commit time. Empty group = ungrouped, which the
// commit rules treat as the strictest setting (foreman can't act on their
// behalf). The transactions snapshot is set at commit time so reports stay
// correct after a worker moves between groups.

func init() {
	m.Register(addUserGroupsUp, addUserGroupsDown)
}

func addUserGroupsUp(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}
	if users.Fields.GetByName("group") == nil {
		users.Fields.Add(&core.TextField{Name: "group"})
	}
	if !hasIndex(users, "idx_users_group") {
		users.AddIndex("idx_users_group", false, "[group]", "")
	}
	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users: %w", err)
	}

	tx, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}
	if tx.Fields.GetByName("user_group") == nil {
		tx.Fields.Add(&core.TextField{Name: "user_group"})
	}
	if !hasIndex(tx, "idx_transactions_user_group") {
		tx.AddIndex("idx_transactions_user_group", false, "user_group", "")
	}
	if err := app.Save(tx); err != nil {
		return fmt.Errorf("save transactions: %w", err)
	}
	return nil
}

func addUserGroupsDown(app core.App) error {
	if tx, err := app.FindCollectionByNameOrId("transactions"); err == nil {
		if tx.Fields.GetByName("user_group") != nil {
			tx.Fields.RemoveByName("user_group")
		}
		removeIndex(tx, "idx_transactions_user_group")
		if err := app.Save(tx); err != nil {
			return fmt.Errorf("save transactions: %w", err)
		}
	}
	if users, err := app.FindCollectionByNameOrId("users"); err == nil {
		if users.Fields.GetByName("group") != nil {
			users.Fields.RemoveByName("group")
		}
		removeIndex(users, "idx_users_group")
		if err := app.Save(users); err != nil {
			return fmt.Errorf("save users: %w", err)
		}
	}
	return nil
}
