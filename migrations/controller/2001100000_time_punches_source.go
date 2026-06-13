package controllermigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Equips the (kiosk-migration-created) time_punches collection for its
// controller role: the aggregator projects every fleet timeclock.punch event
// into it.
//
//   - source_punch_id — the kiosk-side time_punches.id. Unique when
//     non-empty: the idempotency anchor that makes JetStream redelivery and
//     timeclock.republish no-ops (same pattern as source_adjustment_id /
//     source_audit_id on the other audit projections).
//   - source_actor — collapsed kiosk-side actor for source=admin rows (the
//     kiosk's admins record id, which doesn't exist in the controller's DB so
//     the recorded_by_admin FK can't resolve). Foreman recorders resolve via
//     recorded_by_user against the org-wide synced users; controller admins
//     already ride the controller_admin_id text column.
//   - (kiosk_code, occurred_at) index backs the fleet history report's
//     per-kiosk window scans.
//
// Mirrors how 2000000000_controller_collections.go added source_* columns to
// transactions/transaction_lines.

func init() {
	m.Register(addTimePunchesSourceUp, addTimePunchesSourceDown)
}

func addTimePunchesSourceUp(app core.App) error {
	col, err := app.FindCollectionByNameOrId("time_punches")
	if err != nil {
		// Fresh DB where the kiosk migration hasn't run yet — the runner
		// applies in timestamp order so this shouldn't happen; no-op rather
		// than fail (1787 precedent).
		return nil
	}

	if col.Fields.GetByName("source_punch_id") == nil {
		col.Fields.Add(&core.TextField{Name: "source_punch_id"})
	}
	if col.Fields.GetByName("source_actor") == nil {
		col.Fields.Add(&core.TextField{Name: "source_actor"})
	}
	if !hasIndex(col, "idx_time_punches_source") {
		col.AddIndex("idx_time_punches_source", true,
			"source_punch_id", "source_punch_id != ''")
	}
	if !hasIndex(col, "idx_time_punches_kiosk_occurred") {
		col.AddIndex("idx_time_punches_kiosk_occurred", false,
			"kiosk_code, occurred_at", "")
	}

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save time_punches: %w", err)
	}
	return nil
}

func addTimePunchesSourceDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId("time_punches")
	if err != nil {
		return nil
	}
	removeIndex(col, "idx_time_punches_source")
	removeIndex(col, "idx_time_punches_kiosk_occurred")
	for _, name := range []string{"source_punch_id", "source_actor"} {
		if col.Fields.GetByName(name) != nil {
			col.Fields.RemoveByName(name)
		}
	}
	if err := app.Save(col); err != nil {
		return fmt.Errorf("save time_punches: %w", err)
	}
	return nil
}
