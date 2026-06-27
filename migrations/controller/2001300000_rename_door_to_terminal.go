// Mirrors the kiosk-side split of transactions.door_id into terminal_id +
// enclosure_id (kiosk migration 1801000000) on the controller's projected
// transactions, so fleet-level transaction views and the shared CSV export
// keep the same fidelity.
//
// Subtlety: the controller binary blank-imports BOTH migration sets, so the
// kiosk migrations also run on the controller DB. On a fresh controller DB
// kiosk 1801000000 runs first and already produces terminal_id + enclosure_id
// on this shared collection; the controller's own 2001200000 (which predates
// the split) then re-adds a stray door_id. On an already-migrated controller
// DB only door_id is present. This migration therefore *converges* to
// terminal_id + enclosure_id from either starting point rather than assuming a
// plain rename. Idempotent.
package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(renameDoorToTerminalUp, renameDoorToTerminalDown)
}

func renameDoorToTerminalUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}
	// Rename door_id -> terminal_id only when terminal_id isn't already present
	// (the already-migrated-controller case). When terminal_id exists, the
	// kiosk migration already did the rename and door_id is just a stray re-add.
	if col.Fields.GetByName("terminal_id") == nil {
		if door := col.Fields.GetByName("door_id"); door != nil {
			door.SetName("terminal_id")
		}
	}
	// Drop any leftover door_id field + index (the stray re-add on fresh DBs).
	if col.Fields.GetByName("door_id") != nil {
		col.Fields.RemoveByName("door_id")
	}
	removeIndex(col, "idx_transactions_door_id")
	if !hasIndex(col, "idx_transactions_terminal_id") {
		col.AddIndex("idx_transactions_terminal_id", false, "terminal_id", "")
	}
	if col.Fields.GetByName("enclosure_id") == nil {
		col.Fields.Add(&core.TextField{Name: "enclosure_id"})
	}
	if !hasIndex(col, "idx_transactions_enclosure_id") {
		col.AddIndex("idx_transactions_enclosure_id", false, "enclosure_id", "")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("converge transactions door_id -> terminal_id (+enclosure_id): %w", err)
	}
	return nil
}

func renameDoorToTerminalDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return nil
	}
	removeIndex(col, "idx_transactions_enclosure_id")
	if col.Fields.GetByName("enclosure_id") != nil {
		col.Fields.RemoveByName("enclosure_id")
	}
	if col.Fields.GetByName("door_id") == nil {
		if terminal := col.Fields.GetByName("terminal_id"); terminal != nil {
			terminal.SetName("door_id")
		}
	}
	removeIndex(col, "idx_transactions_terminal_id")
	if !hasIndex(col, "idx_transactions_door_id") {
		col.AddIndex("idx_transactions_door_id", false, "door_id", "")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("revert transactions.terminal_id -> door_id: %w", err)
	}
	return nil
}
