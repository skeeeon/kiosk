package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Splits the overloaded transactions.door_id (migration 1800000000) into its
// two real roles:
//
//   - terminal_id — the interaction / custody-acceptance point (the screen a
//     worker accepted the cart at; the ?terminal= URL param on a manual
//     commit). Always-present attribution.
//   - enclosure_id — the access-controlled cabinet an enclosure_diff cart drew
//     from (cart.EnclosureID, set at StartByExternal). Set only for
//     enclosure_diff transactions.
//
// door_id historically held whichever of the two a given flow happened to set,
// so it's renamed in place to terminal_id (preserving existing values) and a
// new nullable enclosure_id is added. The historical attribution-only drift on
// old enclosure_diff rows (their cabinet id stays under terminal_id) is
// acceptable — neither column ever gated auth or correlation. Both nullable +
// non-unique indexed; idempotent. See docs/asset-tracker-plan.md (Phase 1).

func init() {
	m.Register(renameDoorToTerminalUp, renameDoorToTerminalDown)
}

func renameDoorToTerminalUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("transactions")
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}
	if f := col.Fields.GetByName("door_id"); f != nil {
		f.SetName("terminal_id")
		removeIndex(col, "idx_transactions_door_id")
	}
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
		return fmt.Errorf("rename transactions.door_id -> terminal_id (+enclosure_id): %w", err)
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
	if f := col.Fields.GetByName("terminal_id"); f != nil {
		f.SetName("door_id")
		removeIndex(col, "idx_transactions_terminal_id")
	}
	if !hasIndex(col, "idx_transactions_door_id") {
		col.AddIndex("idx_transactions_door_id", false, "door_id", "")
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("revert transactions.terminal_id -> door_id: %w", err)
	}
	return nil
}
