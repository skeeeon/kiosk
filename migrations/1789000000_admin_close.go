package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Extends the ledger so admins can close stale open_checkouts without
// pretending a worker scanned them. Adds:
//
//   - transaction_lines.action gets a new "admin_close" value alongside
//     checkout / return / consume. The action is the ledger's primary
//     discriminator — overloading "return" with a flag would force every
//     future "real returns" query to filter on closed_by_admin IS NULL.
//   - transaction_lines.closed_by_admin (FK to admins, nullable). Records
//     who performed the close. transactions.user still holds the affected
//     worker so per-worker reports still pick up the row.
//   - transaction_lines.closure_reason (select: lost / returned_offline /
//     damaged / other, nullable). The notes column carries free text.
//   - transactions.closed_by_admin (same FK, nullable) — denormalised so
//     transaction-level views can render an actor without joining lines.
//   - transactions.command_id (text, unique when non-empty). Idempotency
//     anchor for controller→kiosk forwards, exactly like the pattern on
//     stock_adjustments.command_id in 1787000000_stock_adjust_remote.go.

func init() {
	m.Register(addAdminCloseUp, addAdminCloseDown)
}

func addAdminCloseUp(app core.App) error {
	if err := extendTransactionLinesForAdminClose(app); err != nil {
		return err
	}
	return extendTransactionsForAdminClose(app)
}

func extendTransactionLinesForAdminClose(app core.App) error {
	col, err := app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		return fmt.Errorf("find transaction_lines: %w", err)
	}

	if f := col.Fields.GetByName("action"); f != nil {
		if sel, ok := f.(*core.SelectField); ok {
			if !containsString(sel.Values, "admin_close") {
				sel.Values = append(sel.Values, "admin_close")
			}
		}
	}

	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return fmt.Errorf("find admins: %w", err)
	}

	if col.Fields.GetByName("closed_by_admin") == nil {
		col.Fields.Add(&core.RelationField{
			Name:         "closed_by_admin",
			CollectionId: admins.Id,
			MaxSelect:    1,
		})
	}
	if col.Fields.GetByName("closure_reason") == nil {
		col.Fields.Add(&core.SelectField{
			Name:      "closure_reason",
			Values:    []string{"lost", "returned_offline", "damaged", "other"},
			MaxSelect: 1,
		})
	}

	if !hasIndex(col, "idx_tx_lines_closed_by_admin") {
		col.AddIndex("idx_tx_lines_closed_by_admin", false,
			"closed_by_admin", "closed_by_admin != ''")
	}

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save transaction_lines: %w", err)
	}
	return nil
}

func extendTransactionsForAdminClose(app core.App) error {
	col, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}

	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return fmt.Errorf("find admins: %w", err)
	}

	if col.Fields.GetByName("closed_by_admin") == nil {
		col.Fields.Add(&core.RelationField{
			Name:         "closed_by_admin",
			CollectionId: admins.Id,
			MaxSelect:    1,
		})
	}
	if col.Fields.GetByName("command_id") == nil {
		col.Fields.Add(&core.TextField{Name: "command_id"})
	}

	// Unique-when-non-empty: legacy transactions have no command_id and must
	// coexist; controller-forwarded admin closes carry a UUID that anchors
	// idempotent replay (catch the unique-violation on duplicate insert and
	// return the prior result).
	if !hasIndex(col, "idx_transactions_command_id") {
		col.AddIndex("idx_transactions_command_id", true,
			"command_id", "command_id != ''")
	}

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save transactions: %w", err)
	}
	return nil
}

func addAdminCloseDown(app core.App) error {
	if col, err := app.FindCollectionByNameOrId("transaction_lines"); err == nil {
		removeIndex(col, "idx_tx_lines_closed_by_admin")
		for _, name := range []string{"closed_by_admin", "closure_reason"} {
			if col.Fields.GetByName(name) != nil {
				col.Fields.RemoveByName(name)
			}
		}
		if f := col.Fields.GetByName("action"); f != nil {
			if sel, ok := f.(*core.SelectField); ok {
				sel.Values = removeString(sel.Values, "admin_close")
			}
		}
		if err := app.Save(col); err != nil {
			return fmt.Errorf("save transaction_lines on down: %w", err)
		}
	}
	if col, err := app.FindCollectionByNameOrId("transactions"); err == nil {
		removeIndex(col, "idx_transactions_command_id")
		for _, name := range []string{"closed_by_admin", "command_id"} {
			if col.Fields.GetByName(name) != nil {
				col.Fields.RemoveByName(name)
			}
		}
		if err := app.Save(col); err != nil {
			return fmt.Errorf("save transactions on down: %w", err)
		}
	}
	return nil
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func removeString(haystack []string, needle string) []string {
	out := haystack[:0]
	for _, v := range haystack {
		if v != needle {
			out = append(out, v)
		}
	}
	return out
}
