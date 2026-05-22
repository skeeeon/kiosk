package migrations

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Promotes users.group from a free-text string to a foreign key on a new
// `groups` collection. Existing values are preserved: one group row is
// created per distinct non-empty string, with code = name = the original
// string. Admins enrich the auto-created rows with contact info post-upgrade.
//
// Cross-user-return semantics in the commit hook are unchanged — equality
// on the FK id behaves identically to equality on the old string. The
// transactions.user_group denormalized snapshot stays a text column; the
// commit code now stamps the group's *code* (read via FK lookup) into it,
// so renaming a group does not retroactively mutate historical transactions.
//
// Idempotent: if users.group is already a relation field the migration is
// a no-op. If the groups collection exists but users.group is still text,
// the swap completes.

func init() {
	m.Register(addGroupsUp, addGroupsDown)
}

func addGroupsUp(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}

	// Capture existing group strings before we touch the field — once the
	// text column is dropped and re-added as a relation, prior values are
	// gone from the row.
	preSwap := map[string]string{} // userID → groupCode snapshot
	currentField := users.Fields.GetByName("group")
	if currentField != nil {
		if _, alreadyRelation := currentField.(*core.RelationField); alreadyRelation {
			return nil
		}
		rows, err := app.FindRecordsByFilter("users", "group != ''", "", 0, 0)
		if err != nil {
			return fmt.Errorf("snapshot user groups: %w", err)
		}
		for _, u := range rows {
			if g := u.GetString("group"); g != "" {
				preSwap[u.Id] = g
			}
		}
	}

	groups, err := createGroupsCollection(app)
	if err != nil {
		return err
	}

	codeToID, err := ensureGroupRowsForCodes(app, groups, preSwap)
	if err != nil {
		return err
	}

	if currentField != nil {
		removeIndex(users, "idx_users_group")
		users.Fields.RemoveByName("group")
		if err := app.Save(users); err != nil {
			return fmt.Errorf("save users after dropping group field: %w", err)
		}
		users, err = app.FindCollectionByNameOrId("users")
		if err != nil {
			return fmt.Errorf("reload users: %w", err)
		}
	}

	if users.Fields.GetByName("group") == nil {
		users.Fields.Add(&core.RelationField{
			Name:         "group",
			CollectionId: groups.Id,
			MaxSelect:    1,
			MinSelect:    0,
			// CascadeDelete intentionally false: deleting a group should
			// unset the FK on assigned users, not delete the workers.
		})
	}
	if !hasIndex(users, "idx_users_group") {
		users.AddIndex("idx_users_group", false, "[group]", "")
	}
	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users with group relation field: %w", err)
	}

	for userID, code := range preSwap {
		gID, ok := codeToID[code]
		if !ok {
			continue
		}
		u, err := app.FindRecordById("users", userID)
		if err != nil {
			return fmt.Errorf("reload user %s: %w", userID, err)
		}
		u.Set("group", gID)
		if err := app.Save(u); err != nil {
			return fmt.Errorf("set group FK on user %s: %w", userID, err)
		}
	}

	return nil
}

func createGroupsCollection(app core.App) (*core.Collection, error) {
	if existing, err := app.FindCollectionByNameOrId("groups"); err == nil {
		return existing, nil
	}

	col := core.NewBaseCollection("groups")
	col.Fields.Add(&core.TextField{Name: "code", Required: true})
	col.Fields.Add(&core.TextField{Name: "name", Required: true})
	col.Fields.Add(&core.EmailField{Name: "contact_email"})
	col.Fields.Add(&core.TextField{Name: "contact_phone"})
	col.Fields.Add(&core.TextField{Name: "notes"})
	col.Fields.Add(&core.BoolField{Name: "active"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	col.AddIndex("idx_groups_code", true, "code", "")
	col.AddIndex("idx_groups_active", false, "active", "")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	col.CreateRule = &rule
	col.UpdateRule = &rule
	col.DeleteRule = &rule

	if err := app.Save(col); err != nil {
		return nil, fmt.Errorf("save groups: %w", err)
	}
	return col, nil
}

// ensureGroupRowsForCodes returns a map of code → groups.id, creating any
// rows that don't already exist. Pre-existing rows with the same code are
// reused so a re-run after partial application doesn't duplicate.
func ensureGroupRowsForCodes(app core.App, groups *core.Collection, userToCode map[string]string) (map[string]string, error) {
	codeToID := map[string]string{}
	for _, code := range userToCode {
		if _, seen := codeToID[code]; seen {
			continue
		}
		existing, err := app.FindFirstRecordByFilter("groups", "code = {:code}", dbx.Params{"code": code})
		if err == nil {
			codeToID[code] = existing.Id
			continue
		}
		rec := core.NewRecord(groups)
		rec.Set("code", code)
		rec.Set("name", code)
		rec.Set("active", true)
		if err := app.Save(rec); err != nil {
			return nil, fmt.Errorf("create group %q: %w", code, err)
		}
		codeToID[code] = rec.Id
	}
	return codeToID, nil
}

func addGroupsDown(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}

	// Snapshot FK → code mapping before we drop the relation column.
	idToCode := map[string]string{}
	if g, _ := app.FindCollectionByNameOrId("groups"); g != nil {
		rows, err := app.FindRecordsByFilter("groups", "", "", 0, 0)
		if err != nil {
			return fmt.Errorf("snapshot groups: %w", err)
		}
		for _, r := range rows {
			idToCode[r.Id] = r.GetString("code")
		}
	}

	userToCode := map[string]string{}
	if users.Fields.GetByName("group") != nil {
		rows, err := app.FindRecordsByFilter("users", "group != ''", "", 0, 0)
		if err != nil {
			return fmt.Errorf("snapshot user FKs: %w", err)
		}
		for _, u := range rows {
			id := u.GetString("group")
			if code, ok := idToCode[id]; ok && code != "" {
				userToCode[u.Id] = code
			}
		}

		removeIndex(users, "idx_users_group")
		users.Fields.RemoveByName("group")
		if err := app.Save(users); err != nil {
			return fmt.Errorf("save users after dropping relation: %w", err)
		}
		users, err = app.FindCollectionByNameOrId("users")
		if err != nil {
			return fmt.Errorf("reload users: %w", err)
		}
	}

	if users.Fields.GetByName("group") == nil {
		users.Fields.Add(&core.TextField{Name: "group"})
	}
	if !hasIndex(users, "idx_users_group") {
		users.AddIndex("idx_users_group", false, "[group]", "")
	}
	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users with text group field: %w", err)
	}

	for userID, code := range userToCode {
		u, err := app.FindRecordById("users", userID)
		if err != nil {
			return fmt.Errorf("reload user %s: %w", userID, err)
		}
		u.Set("group", code)
		if err := app.Save(u); err != nil {
			return fmt.Errorf("restore group string on user %s: %w", userID, err)
		}
	}

	if g, err := app.FindCollectionByNameOrId("groups"); err == nil {
		if err := app.Delete(g); err != nil {
			return fmt.Errorf("delete groups: %w", err)
		}
	}

	return nil
}
