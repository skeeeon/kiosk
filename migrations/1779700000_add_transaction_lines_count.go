package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Adds a denormalized `lines_count` to transactions so the admin UI and the
// CSV export don't need an N+1 COUNT(*) per row to render the lines column.
// The commit hook sets it once at promote time; the value is stable because
// transactions are append-only after commit.
//
// Backfills existing rows via a single UPDATE that counts each transaction's
// lines from the ledger. Idempotent: re-running computes the same counts.

func init() {
	m.Register(addTransactionLinesCountUp, addTransactionLinesCountDown)
}

func addTransactionLinesCountUp(app core.App) error {
	tx, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}
	if tx.Fields.GetByName("lines_count") == nil {
		tx.Fields.Add(&core.NumberField{Name: "lines_count", OnlyInt: true})
		if err := app.Save(tx); err != nil {
			return fmt.Errorf("save transactions: %w", err)
		}
	}

	_, err = app.DB().NewQuery(`
		UPDATE transactions
		SET lines_count = (
			SELECT COUNT(*) FROM transaction_lines
			WHERE transaction_lines.[[transaction]] = transactions.id
		)
	`).Execute()
	if err != nil {
		return fmt.Errorf("backfill lines_count: %w", err)
	}
	return nil
}

func addTransactionLinesCountDown(app core.App) error {
	tx, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return nil
	}
	if f := tx.Fields.GetByName("lines_count"); f != nil {
		if _, ok := f.(*core.NumberField); ok {
			tx.Fields.RemoveByName("lines_count")
			if err := app.Save(tx); err != nil {
				return fmt.Errorf("save transactions: %w", err)
			}
		}
	}
	return nil
}
