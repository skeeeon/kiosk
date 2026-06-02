// Drops applied_oc_closes. It was the idempotency guard for the controller's
// incremental open_checkouts close projection (return + admin_close deletes).
// The controller no longer materializes open_checkouts — it replays the
// projected transaction_lines ledger on demand (ledger.ReplayOpenRows) — so
// there are no guarded deletes left and nothing reads or writes this table.
//
// Additive history: migration 2000600000 still creates the table on a fresh DB
// and this one drops it, which is a deliberate create-then-drop rather than
// editing an already-applied migration. The up is idempotent (skip if absent);
// the down is a no-op — recreating an idempotency table that no code touches
// would only resurrect dead schema.
package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(dropAppliedOcClosesUp, dropAppliedOcClosesDown)
}

func dropAppliedOcClosesUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("applied_oc_closes")
	if err != nil {
		return nil // already gone (fresh DB never created it, or prior run)
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete applied_oc_closes: %w", err)
	}
	return nil
}

func dropAppliedOcClosesDown(app core.App) error {
	// No-op: nothing reads or writes applied_oc_closes anymore, so there's
	// nothing to restore. A full rollback past 2000600000 still recreates it
	// via that migration's up if it's ever re-run forward.
	return nil
}
