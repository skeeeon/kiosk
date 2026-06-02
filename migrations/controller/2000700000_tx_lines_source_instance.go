// Adds source_item_instance_id to the controller's transaction_lines.
//
// The controller computes "what's currently out" by replaying its projected
// ledger (ledger.ReplayOpenRows) rather than materializing an open_checkouts
// table. Replay matches a serialized checkout to its return by instance id, so
// each projected line must carry that id. The kiosk-local item_instances.id
// can't live on the existing item_instance RelationField — it points at the
// controller's own (always-empty) item_instances collection and would fail the
// FK — so we keep a parallel text column, exactly the modeling
// open_checkouts.source_item_instance_id already uses (migration 2000500000).
package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(txLinesSourceInstanceUp, txLinesSourceInstanceDown)
}

func txLinesSourceInstanceUp(app core.App) error {
	lines, err := app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		return fmt.Errorf("find transaction_lines: %w", err)
	}
	if lines.Fields.GetByName("source_item_instance_id") == nil {
		lines.Fields.Add(&core.TextField{Name: "source_item_instance_id"})
	}
	if !hasIndex(lines, "idx_tx_lines_source_instance") {
		lines.AddIndex("idx_tx_lines_source_instance", false,
			"source_item_instance_id",
			"source_item_instance_id != ''")
	}
	if err := app.Save(lines); err != nil {
		return fmt.Errorf("save transaction_lines (source_item_instance_id): %w", err)
	}
	return nil
}

func txLinesSourceInstanceDown(app core.App) error {
	lines, err := app.FindCollectionByNameOrId("transaction_lines")
	if err != nil {
		return nil
	}
	if lines.Fields.GetByName("source_item_instance_id") != nil {
		lines.Fields.RemoveByName("source_item_instance_id")
	}
	removeIndex(lines, "idx_tx_lines_source_instance")
	if err := app.Save(lines); err != nil {
		return fmt.Errorf("save transaction_lines (drop source_item_instance_id): %w", err)
	}
	return nil
}
