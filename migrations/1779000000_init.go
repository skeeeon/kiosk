// Package migrations holds schema-as-code definitions for the kiosk's
// collections. Migrations are registered via init() side effects and run
// automatically on app start when migratecmd is configured with Automigrate.
//
// The initial migration creates six collections (the default `users` is
// modified in place; `admins`, `items`, `transactions`, `transaction_lines`,
// `open_checkouts` are created fresh) and seeds one bootstrap admin whose
// password is printed to stdout exactly once.
package migrations

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

const (
	adminRule       = "@request.auth.collectionName = 'admins'"
	adminSelfRule   = "id = @request.auth.id"
	bootstrapEmail  = "admin@kiosk.local"
	bootstrapName   = "Bootstrap Admin"
	passwordEntropy = 12 // bytes; ~96 bits → 16-char URL-safe base64
)

func init() {
	m.Register(up, down)
}

func up(app core.App) error {
	if err := modifyUsersCollection(app); err != nil {
		return err
	}
	admins, err := createAdminsCollection(app)
	if err != nil {
		return err
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("reload users: %w", err)
	}
	items, err := createItemsCollection(app)
	if err != nil {
		return err
	}
	transactions, err := createTransactionsCollection(app, users)
	if err != nil {
		return err
	}
	txLines, err := createTransactionLinesCollection(app, transactions, items, users)
	if err != nil {
		return err
	}
	if err := createOpenCheckoutsCollection(app, items, users, txLines); err != nil {
		return err
	}
	return seedBootstrapAdmin(app, admins)
}

func down(app core.App) error {
	// Reverse order to respect FK constraints. Skip silently if not found.
	for _, name := range []string{
		"open_checkouts",
		"transaction_lines",
		"transactions",
		"items",
		"admins",
	} {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			continue
		}
		if err := app.Delete(c); err != nil {
			return fmt.Errorf("delete %s: %w", name, err)
		}
	}
	// The default users collection is left in place; its added fields stay too.
	// Reverting structural changes to a system collection is risky and v1's
	// down migration is for dev-loop use, not production rollback.
	return nil
}

func modifyUsersCollection(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}
	users.Fields.Add(&core.TextField{Name: "code", Required: true})
	users.Fields.Add(&core.SelectField{
		Name:      "role",
		Values:    []string{"worker", "foreman"},
		Required:  true,
		MaxSelect: 1,
	})
	users.Fields.Add(&core.BoolField{Name: "active"})

	users.AddIndex("idx_users_code", true, "code", "")

	openToAdmins := adminRule
	users.ListRule = &openToAdmins
	users.ViewRule = &openToAdmins
	users.CreateRule = &openToAdmins
	users.UpdateRule = &openToAdmins
	users.DeleteRule = &openToAdmins

	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}

func createAdminsCollection(app core.App) (*core.Collection, error) {
	admins := core.NewAuthCollection("admins")
	admins.Fields.Add(&core.TextField{Name: "name", Required: true})
	admins.Fields.Add(&core.BoolField{Name: "active"})

	selfOnly := adminSelfRule
	admins.ListRule = &selfOnly
	admins.ViewRule = &selfOnly
	admins.UpdateRule = &selfOnly
	admins.CreateRule = nil
	admins.DeleteRule = nil

	if err := app.Save(admins); err != nil {
		return nil, fmt.Errorf("save admins: %w", err)
	}
	return admins, nil
}

func createItemsCollection(app core.App) (*core.Collection, error) {
	items := core.NewBaseCollection("items")
	items.Fields.Add(&core.TextField{Name: "code", Required: true})
	items.Fields.Add(&core.TextField{Name: "rfid_epc"})
	items.Fields.Add(&core.TextField{Name: "name", Required: true})
	items.Fields.Add(&core.SelectField{
		Name:      "type",
		Values:    []string{"tool", "consumable"},
		Required:  true,
		MaxSelect: 1,
	})
	items.Fields.Add(&core.TextField{Name: "unit"})
	items.Fields.Add(&core.SelectField{
		Name:      "tracking_mode",
		Values:    []string{"quantity", "serialized"},
		Required:  true,
		MaxSelect: 1,
	})
	items.Fields.Add(&core.TextField{Name: "serial"})
	items.Fields.Add(&core.TextField{Name: "category"})
	items.Fields.Add(&core.BoolField{Name: "active"})
	items.Fields.Add(&core.TextField{Name: "notes"})

	items.AddIndex("idx_items_code", true, "code", "")
	items.AddIndex("idx_items_rfid_epc", true, "rfid_epc", "rfid_epc != ''")
	items.AddIndex("idx_items_serial", true, "serial", "serial != ''")
	items.AddIndex("idx_items_category", false, "category", "")

	rule := adminRule
	items.ListRule = &rule
	items.ViewRule = &rule
	items.CreateRule = &rule
	items.UpdateRule = &rule
	items.DeleteRule = &rule

	if err := app.Save(items); err != nil {
		return nil, fmt.Errorf("save items: %w", err)
	}
	return items, nil
}

func createTransactionsCollection(app core.App, users *core.Collection) (*core.Collection, error) {
	tx := core.NewBaseCollection("transactions")
	tx.Fields.Add(&core.TextField{Name: "kiosk_code", Required: true})
	tx.Fields.Add(&core.TextField{Name: "location_code", Required: true})
	tx.Fields.Add(&core.RelationField{
		Name:         "user",
		CollectionId: users.Id,
		Required:     true,
		MaxSelect:    1,
	})
	tx.Fields.Add(&core.DateField{Name: "started_at"})
	tx.Fields.Add(&core.DateField{Name: "completed_at"})
	tx.Fields.Add(&core.SelectField{
		Name:      "status",
		Values:    []string{"open", "completed", "voided"},
		Required:  true,
		MaxSelect: 1,
	})
	tx.Fields.Add(&core.TextField{Name: "void_reason"})

	tx.AddIndex("idx_transactions_kiosk_code", false, "kiosk_code", "")
	tx.AddIndex("idx_transactions_location_code", false, "location_code", "")
	tx.AddIndex("idx_transactions_user", false, "[user]", "")
	tx.AddIndex("idx_transactions_status", false, "status", "")

	rule := adminRule
	tx.ListRule = &rule
	tx.ViewRule = &rule
	// create/update/delete are nil: writes flow through the in-process commit hook.

	if err := app.Save(tx); err != nil {
		return nil, fmt.Errorf("save transactions: %w", err)
	}
	return tx, nil
}

func createTransactionLinesCollection(app core.App, transactions, items, users *core.Collection) (*core.Collection, error) {
	lines := core.NewBaseCollection("transaction_lines")
	lines.Fields.Add(&core.RelationField{
		Name:          "transaction",
		CollectionId:  transactions.Id,
		Required:      true,
		MaxSelect:     1,
		CascadeDelete: true,
	})
	lines.Fields.Add(&core.RelationField{
		Name:         "item",
		CollectionId: items.Id,
		Required:     true,
		MaxSelect:    1,
	})
	lines.Fields.Add(&core.SelectField{
		Name:      "action",
		Values:    []string{"checkout", "return", "consume"},
		Required:  true,
		MaxSelect: 1,
	})
	lines.Fields.Add(&core.NumberField{
		Name:     "qty",
		Required: true,
		OnlyInt:  true,
	})
	lines.Fields.Add(&core.TextField{Name: "serial"})
	lines.Fields.Add(&core.RelationField{
		Name:         "original_checkout_user",
		CollectionId: users.Id,
		MaxSelect:    1,
	})
	lines.Fields.Add(&core.BoolField{Name: "uncorrelated"})
	lines.Fields.Add(&core.TextField{Name: "notes"})

	lines.AddIndex("idx_tx_lines_transaction", false, "[transaction]", "")
	lines.AddIndex("idx_tx_lines_item", false, "item", "")
	lines.AddIndex("idx_tx_lines_action", false, "action", "")

	rule := adminRule
	lines.ListRule = &rule
	lines.ViewRule = &rule

	if err := app.Save(lines); err != nil {
		return nil, fmt.Errorf("save transaction_lines: %w", err)
	}
	return lines, nil
}

func createOpenCheckoutsCollection(app core.App, items, users, txLines *core.Collection) error {
	open := core.NewBaseCollection("open_checkouts")
	open.Fields.Add(&core.RelationField{
		Name:         "item",
		CollectionId: items.Id,
		Required:     true,
		MaxSelect:    1,
	})
	open.Fields.Add(&core.TextField{Name: "serial"})
	open.Fields.Add(&core.RelationField{
		Name:         "user",
		CollectionId: users.Id,
		Required:     true,
		MaxSelect:    1,
	})
	open.Fields.Add(&core.DateField{Name: "checked_out_at", Required: true})
	open.Fields.Add(&core.RelationField{
		Name:         "transaction_line",
		CollectionId: txLines.Id,
		Required:     true,
		MaxSelect:    1,
	})

	open.AddIndex("idx_open_checkouts_item", false, "item", "")
	open.AddIndex("idx_open_checkouts_user", false, "[user]", "")
	open.AddIndex("idx_open_checkouts_serial", true, "serial", "serial != ''")

	rule := adminRule
	open.ListRule = &rule
	open.ViewRule = &rule

	if err := app.Save(open); err != nil {
		return fmt.Errorf("save open_checkouts: %w", err)
	}
	return nil
}

func seedBootstrapAdmin(app core.App, admins *core.Collection) error {
	password, err := generatePassword()
	if err != nil {
		return fmt.Errorf("generate bootstrap password: %w", err)
	}
	record := core.NewRecord(admins)
	record.Set("email", bootstrapEmail)
	record.Set("name", bootstrapName)
	record.Set("active", true)
	record.SetPassword(password)
	if err := app.Save(record); err != nil {
		return fmt.Errorf("save bootstrap admin: %w", err)
	}

	// Printed once, never again. The user has one chance to capture this.
	// If they miss it, recovery is via the PB superuser UI at /_/.
	// Tests set KIOSK_QUIET_BOOTSTRAP to suppress this noise.
	if os.Getenv("KIOSK_QUIET_BOOTSTRAP") == "" {
		fmt.Println("")
		fmt.Println("========================================================================")
		fmt.Println(" BOOTSTRAP ADMIN CREDENTIALS — shown once, save them now")
		fmt.Println("------------------------------------------------------------------------")
		fmt.Printf("  email:    %s\n", bootstrapEmail)
		fmt.Printf("  password: %s\n", password)
		fmt.Println("------------------------------------------------------------------------")
		fmt.Println(" Sign in at /admin/login. Recovery: PB superuser UI at /_/.")
		fmt.Println("========================================================================")
		fmt.Println("")
	}
	return nil
}

func generatePassword() (string, error) {
	b := make([]byte, passwordEntropy)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
