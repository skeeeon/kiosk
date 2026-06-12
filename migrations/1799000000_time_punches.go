package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// time_punches is the append-only timeclock ledger: one row per clock-in /
// clock-out punch. Same invariants as transactions/transaction_lines:
//
//   - Rows are written only by the in-process punch funnel
//     (internal/timeclock.PerformPunch) — the PB REST API is read-only for
//     admins and closed for everyone else. No update, no delete, ever;
//     corrections are new punches with a reason.
//   - "Is this user clocked in" is DERIVED state — the latest punch per user
//     by occurred_at (created breaks ties for same-timestamp corrections).
//     There is deliberately no materialized open_shifts table to keep honest.
//   - occurred_at is the business timestamp and may be backdated by admin
//     punches; `created` (autodate) is when the row was recorded. Live
//     (self/foreman) punches always have occurred_at stamped server-side.
//
// Actor fields mirror the stock_adjustments pattern: exactly one of
// (recorded_by_admin, recorded_by_user, controller_admin_id) is populated
// for non-self sources. controller_admin_id is plain text because controller
// admins live in the controller's PB DB, not the kiosk's. command_id is the
// idempotency anchor for command-bus punches (unique when non-empty).
//
// Shared kiosk migration → the collection also exists in the controller DB,
// where the aggregator projects the fleet's punch events into it (a
// controller-only migration adds source_punch_id for that).

func init() {
	m.Register(addTimePunchesUp, addTimePunchesDown)
}

const timePunchesCollection = "time_punches"

func addTimePunchesUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(timePunchesCollection); err == nil {
		return nil
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}
	admins, err := app.FindCollectionByNameOrId("admins")
	if err != nil {
		return fmt.Errorf("find admins: %w", err)
	}

	col := core.NewBaseCollection(timePunchesCollection)
	col.Fields.Add(&core.RelationField{
		Name:         "user",
		CollectionId: users.Id,
		Required:     true,
		MaxSelect:    1,
	})
	col.Fields.Add(&core.TextField{Name: "user_code", Required: true})
	col.Fields.Add(&core.SelectField{
		Name:      "direction",
		Values:    []string{"in", "out"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.DateField{Name: "occurred_at", Required: true})
	col.Fields.Add(&core.SelectField{
		Name:      "source",
		Values:    []string{"self", "foreman", "admin", "controller_admin"},
		Required:  true,
		MaxSelect: 1,
	})
	col.Fields.Add(&core.RelationField{
		Name:         "recorded_by_admin",
		CollectionId: admins.Id,
		MaxSelect:    1,
	})
	col.Fields.Add(&core.RelationField{
		Name:         "recorded_by_user",
		CollectionId: users.Id,
		MaxSelect:    1,
	})
	col.Fields.Add(&core.TextField{Name: "controller_admin_id"})
	col.Fields.Add(&core.TextField{Name: "reason"})
	col.Fields.Add(&core.BoolField{Name: "force"})
	col.Fields.Add(&core.TextField{Name: "kiosk_code", Required: true})
	col.Fields.Add(&core.TextField{Name: "location_code"})
	col.Fields.Add(&core.TextField{Name: "command_id"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	// (user, occurred_at) backs the hot "latest punch for user X" query that
	// every clocked-in check runs; occurred_at alone backs date-range report
	// scans; command_id unique-when-non-empty is the idempotency anchor for
	// replayed command-bus punches (same pattern as stock_adjustments).
	col.AddIndex("idx_time_punches_user_occurred", false, "[user], occurred_at", "")
	col.AddIndex("idx_time_punches_occurred", false, "occurred_at", "")
	col.AddIndex("idx_time_punches_command_id", true, "command_id", "command_id != ''")

	rule := adminRule
	col.ListRule = &rule
	col.ViewRule = &rule
	// Create/Update/Delete intentionally nil — append-only, funnel-only.

	if err := app.Save(col); err != nil {
		return fmt.Errorf("save %s: %w", timePunchesCollection, err)
	}
	return nil
}

func addTimePunchesDown(app core.App) error {
	col, err := app.FindCollectionByNameOrId(timePunchesCollection)
	if err != nil {
		return nil
	}
	if err := app.Delete(col); err != nil {
		return fmt.Errorf("delete %s: %w", timePunchesCollection, err)
	}
	return nil
}
