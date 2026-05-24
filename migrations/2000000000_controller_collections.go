// Controller-only schema. Registered with the PB migration runner ONLY when
// the process is the kiosk-controller (cmd/controller sets KIOSK_ROLE=controller
// before importing this package). Plain kiosks leave the env var unset, so
// this migration stays dormant for them — their local DBs never get the
// controller's `kiosks` registry collection or the source_* fields on
// transactions.
//
// What this adds on the controller side:
//   - `kiosks` registry collection (one row per kiosk in the fleet).
//   - `source_kiosk_code` + `source_transaction_id` on `transactions`, plus a
//     unique index over the pair. Together they make the JetStream consumer
//     idempotent under redelivery — the same event arriving twice can't create
//     two ledger rows.
//   - `source_line_id` on `transaction_lines`, unique when non-empty.
package migrations

import (
	"fmt"
	"sync"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// RegisterControllerMigrations adds the controller-only schema migrations to
// the PB registry. cmd/controller calls this in main; the kiosk binary
// doesn't, so its DB never sees the kiosks registry or the source_* fields.
// Tests in internal/controller call this in their setupApp helper.
//
// sync.Once guards against double-registration: PB rejects two migrations
// with the same name, and tests legitimately set up many isolated apps in
// one binary run.
func RegisterControllerMigrations() {
	controllerOnce.Do(func() {
		m.Register(upController, downController)
		// kiosk_items membership + open kiosks.CreateRule. Lives in its own
		// file but registers in the same sync.Once so a process never
		// double-registers either migration.
		RegisterKioskItemsMigration()
		// last_transaction_at on the kiosks collection — replaces last_seen
		// as the per-kiosk "when did this kiosk last actually transact"
		// signal once heartbeat takes over liveness duty.
		RegisterKiosksLastTransactionAtMigration()
		// inventory_audit collection — fleet-wide projection of every
		// inventory.adjust event for the Adjustment audit Reports tab.
		RegisterInventoryAuditMigration()
		// instance_lifecycle_audit collection — fleet-wide projection of
		// every instance.lifecycle event for the Instance lifecycle
		// Reports tab.
		RegisterInstanceLifecycleAuditMigration()
		// kiosk_code column on open_checkouts so the controller can hold
		// the whole fleet's open rows in one table. Projection writes via
		// ProjectOpenCheckouts in internal/controller/consumer.go.
		RegisterOpenCheckoutsKioskCodeMigration()
	})
}

var controllerOnce sync.Once

func upController(app core.App) error {
	if err := createKiosksCollection(app); err != nil {
		return err
	}
	if err := addTransactionSourceFields(app); err != nil {
		return err
	}
	if err := addTransactionLineSourceFields(app); err != nil {
		return err
	}
	return nil
}

func downController(app core.App) error {
	// Reverse order. Skip silently if collections/fields aren't present —
	// down migrations are dev-loop tools, not production rollbacks.
	if lines, err := app.FindCollectionByNameOrId("transaction_lines"); err == nil {
		if lines.Fields.GetByName("source_line_id") != nil {
			lines.Fields.RemoveByName("source_line_id")
		}
		removeIndex(lines, "idx_tx_lines_source_line")
		if err := app.Save(lines); err != nil {
			return fmt.Errorf("save transaction_lines: %w", err)
		}
	}
	if tx, err := app.FindCollectionByNameOrId("transactions"); err == nil {
		for _, name := range []string{"source_kiosk_code", "source_transaction_id"} {
			if tx.Fields.GetByName(name) != nil {
				tx.Fields.RemoveByName(name)
			}
		}
		removeIndex(tx, "idx_transactions_source")
		if err := app.Save(tx); err != nil {
			return fmt.Errorf("save transactions: %w", err)
		}
	}
	if k, err := app.FindCollectionByNameOrId("kiosks"); err == nil {
		if err := app.Delete(k); err != nil {
			return fmt.Errorf("delete kiosks: %w", err)
		}
	}
	return nil
}

func createKiosksCollection(app core.App) error {
	// Idempotent — if a previous run created it, leave it alone.
	if _, err := app.FindCollectionByNameOrId("kiosks"); err == nil {
		return nil
	}

	k := core.NewBaseCollection("kiosks")
	k.Fields.Add(&core.TextField{Name: "kiosk_code", Required: true})
	k.Fields.Add(&core.TextField{Name: "location_code"})
	k.Fields.Add(&core.DateField{Name: "last_seen"})
	k.Fields.Add(&core.SelectField{
		Name:      "status",
		Values:    []string{"unknown", "active", "disabled"},
		Required:  true,
		MaxSelect: 1,
	})
	k.Fields.Add(&core.TextField{Name: "notes"})

	k.AddIndex("idx_kiosks_kiosk_code", true, "kiosk_code", "")

	rule := adminRule
	k.ListRule = &rule
	k.ViewRule = &rule
	k.UpdateRule = &rule
	// create/delete stay nil — auto-registration happens via the consumer
	// using the DAO, and removing a kiosk record is an out-of-band ops job
	// done via the PB superuser UI.

	if err := app.Save(k); err != nil {
		return fmt.Errorf("save kiosks: %w", err)
	}
	return nil
}

func addTransactionSourceFields(app core.App) error {
	tx, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}

	if tx.Fields.GetByName("source_kiosk_code") == nil {
		tx.Fields.Add(&core.TextField{Name: "source_kiosk_code"})
	}
	if tx.Fields.GetByName("source_transaction_id") == nil {
		tx.Fields.Add(&core.TextField{Name: "source_transaction_id"})
	}

	// Unique pair; safe to also have rows where both are blank (those would
	// be controller-local rows, which we don't expect but won't reject).
	// PB index syntax: list columns; uniqueness applies across the tuple.
	if !hasIndex(tx, "idx_transactions_source") {
		tx.AddIndex("idx_transactions_source", true,
			"source_kiosk_code, source_transaction_id",
			"source_kiosk_code != '' AND source_transaction_id != ''")
	}

	if err := app.Save(tx); err != nil {
		return fmt.Errorf("save transactions: %w", err)
	}
	return nil
}

func addTransactionLineSourceFields(app core.App) error {
	lines, err := app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		return fmt.Errorf("find transaction_lines: %w", err)
	}

	if lines.Fields.GetByName("source_line_id") == nil {
		lines.Fields.Add(&core.TextField{Name: "source_line_id"})
	}

	// Kiosk-local line IDs are 15-char random PB IDs; cross-fleet collision
	// is astronomically unlikely so we don't need to compound with kiosk_code.
	// The filter lets controller-local lines (empty source) coexist.
	if !hasIndex(lines, "idx_tx_lines_source_line") {
		lines.AddIndex("idx_tx_lines_source_line", true,
			"source_line_id", "source_line_id != ''")
	}

	if err := app.Save(lines); err != nil {
		return fmt.Errorf("save transaction_lines: %w", err)
	}
	return nil
}

func hasIndex(c *core.Collection, name string) bool {
	for _, idx := range c.Indexes {
		if extractIndexName(idx) == name {
			return true
		}
	}
	return false
}

func removeIndex(c *core.Collection, name string) {
	out := c.Indexes[:0]
	for _, idx := range c.Indexes {
		if extractIndexName(idx) != name {
			out = append(out, idx)
		}
	}
	c.Indexes = out
}

// extractIndexName pulls the index name out of a PB index DDL string. PB
// stores indexes as raw SQL like
//
//	CREATE UNIQUE INDEX `idx_foo` ON `bar` (`col`) WHERE ...
//
// so we look for the name between the first pair of backticks.
func extractIndexName(ddl string) string {
	start := -1
	for i := 0; i < len(ddl); i++ {
		if ddl[i] == '`' {
			if start == -1 {
				start = i + 1
				continue
			}
			return ddl[start:i]
		}
	}
	return ""
}
