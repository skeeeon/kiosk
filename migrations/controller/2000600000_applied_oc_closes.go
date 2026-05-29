// applied_oc_closes is the controller-side idempotency anchor for
// open_checkouts close projections (return + admin_close).
//
// Those projectors DELETE rows, so unlike the insert path there's no
// surviving row to dedupe against on JetStream redelivery. ProjectLine runs
// in a separate transaction before the return close, so "skip if the line
// already exists" can't distinguish "close already applied" from "line
// written but close not yet applied" — a redelivery after a lost Ack would
// re-select and delete a *different* fungible row (or, with the old
// cross-user fallback, an innocent worker's row).
//
// Each close records its triggering transaction_line id here ("ret:<return
// line id>" for return closes, "ac:<admin_close line id>" for admin closes)
// inside the SAME transaction as the deletes. The line id is used (not the
// open_checkout id) because it's stable across both the live event and a
// ledger.republish, which can't recover the open_checkout id — that row was
// deleted at close time. The unique index on dedupe_key makes a duplicate the
// redelivery signal: the guard row's presence means the close already
// committed atomically, so re-runs no-op.
package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(addAppliedOcClosesUp, addAppliedOcClosesDown)
}

const appliedOcClosesCollection = "applied_oc_closes"

func addAppliedOcClosesUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(appliedOcClosesCollection); err == nil {
		return nil
	}

	col := core.NewBaseCollection(appliedOcClosesCollection)
	col.Fields.Add(&core.TextField{Name: "dedupe_key", Required: true})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	// Unique on dedupe_key — the idempotency anchor. Same modeling as the
	// other controller audit collections (raw unique index, not a field attr).
	col.AddIndex("idx_applied_oc_closes_key", true, "dedupe_key", "")

	// All collection rules intentionally nil: only the aggregator writes
	// (via app.Save, which bypasses rules), and the rows are an internal
	// dedupe ledger never exposed over the API.

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", appliedOcClosesCollection, err)
	}
	return nil
}

func addAppliedOcClosesDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(appliedOcClosesCollection)
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete %s: %w", appliedOcClosesCollection, err)
	}
	return nil
}
