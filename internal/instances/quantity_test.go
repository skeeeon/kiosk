package instances

import (
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// itemQty re-reads an item and returns its stored quantity_on_hand.
func itemQty(t *testing.T, app core.App, itemID string) int {
	t.Helper()
	rec, err := app.FindRecordById("items", itemID)
	if err != nil {
		t.Fatalf("find item %s: %v", itemID, err)
	}
	return rec.GetInt("quantity_on_hand")
}

// addInstance creates an item_instances row via the DAO (the REST/superuser
// path shape) so the model after-success hook fires. `active` maps to the
// status enum: true → in_service (counts toward qty), false → retired
// (excluded from the non-retired count).
func addInstance(t *testing.T, app core.App, itemID, code string, active bool) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("item_instances")
	if err != nil {
		t.Fatalf("find item_instances: %v", err)
	}
	status := StatusInService
	if !active {
		status = StatusRetired
	}
	inst := core.NewRecord(col)
	inst.Set("item", itemID)
	inst.Set("code", code)
	inst.Set("status", status)
	if err := app.Save(inst); err != nil {
		t.Fatalf("save instance %s: %v", code, err)
	}
	return inst
}

// TestRecompute_CreateTracksActiveCount verifies that creating active
// instances drives quantity_on_hand up, while an inactive instance doesn't
// count.
func TestRecompute_CreateTracksActiveCount(t *testing.T) {
	app := setupApp(t)
	New().Register(app)
	itemID, _ := seedItemWithInstance(t, app) // one active instance

	if got := itemQty(t, app, itemID); got != 1 {
		t.Fatalf("after 1 active instance: want qty 1, got %d", got)
	}
	addInstance(t, app, itemID, "DRILL-A-2", true)
	if got := itemQty(t, app, itemID); got != 2 {
		t.Fatalf("after 2 active instances: want qty 2, got %d", got)
	}
	addInstance(t, app, itemID, "DRILL-A-3", false)
	if got := itemQty(t, app, itemID); got != 2 {
		t.Fatalf("after adding an inactive instance: want qty 2 (unchanged), got %d", got)
	}
}

// TestRecompute_RetireUnretire exercises the command-bus mutation path
// (PerformSetStatus to retired, then back to in_service) and asserts the
// derived quantity follows the non-retired count.
func TestRecompute_RetireUnretire(t *testing.T) {
	app := setupApp(t)
	New().Register(app)
	itemID, inst := seedItemWithInstance(t, app)
	addInstance(t, app, itemID, "DRILL-A-2", true)
	if got := itemQty(t, app, itemID); got != 2 {
		t.Fatalf("setup: want qty 2, got %d", got)
	}

	if _, err := PerformSetStatus(app, ToggleInput{
		InstanceCode: inst.GetString("code"),
		Reason:       "broken",
		Source:       events.SourceLocal,
	}, StatusRetired); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got := itemQty(t, app, itemID); got != 1 {
		t.Fatalf("after retire: want qty 1, got %d", got)
	}

	if _, err := PerformSetStatus(app, ToggleInput{
		InstanceCode: inst.GetString("code"),
		Reason:       "fixed",
		Source:       events.SourceLocal,
	}, StatusInService); err != nil {
		t.Fatalf("unretire: %v", err)
	}
	if got := itemQty(t, app, itemID); got != 2 {
		t.Fatalf("after unretire: want qty 2, got %d", got)
	}
}

// TestRecompute_MaintenanceCountsTowardQty verifies a unit in maintenance still
// counts toward quantity_on_hand (it's owned, just parked) — only retired
// drops the count.
func TestRecompute_MaintenanceCountsTowardQty(t *testing.T) {
	app := setupApp(t)
	New().Register(app)
	itemID, inst := seedItemWithInstance(t, app)
	if got := itemQty(t, app, itemID); got != 1 {
		t.Fatalf("setup: want qty 1, got %d", got)
	}
	if _, err := PerformSetStatus(app, ToggleInput{
		InstanceCode: inst.GetString("code"),
		Reason:       "needs service",
		Source:       events.SourceLocal,
	}, StatusMaintenance); err != nil {
		t.Fatalf("to maintenance: %v", err)
	}
	if got := itemQty(t, app, itemID); got != 1 {
		t.Errorf("after maintenance: want qty 1 (still counts), got %d", got)
	}
}

// TestRecompute_DeleteLowersQuantity verifies a hard delete of an instance
// drops the derived quantity.
func TestRecompute_DeleteLowersQuantity(t *testing.T) {
	app := setupApp(t)
	New().Register(app)
	itemID, inst := seedItemWithInstance(t, app)
	if got := itemQty(t, app, itemID); got != 1 {
		t.Fatalf("setup: want qty 1, got %d", got)
	}
	if err := app.Delete(inst); err != nil {
		t.Fatalf("delete instance: %v", err)
	}
	if got := itemQty(t, app, itemID); got != 0 {
		t.Fatalf("after delete: want qty 0, got %d", got)
	}
}

// TestRecompute_NonSerializedUntouched proves the recompute guard: a
// quantity-tracked item's stored stock is never clobbered, even if an
// item_instances row somehow references it.
func TestRecompute_NonSerializedUntouched(t *testing.T) {
	app := setupApp(t)
	New().Register(app)

	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	qtyItem := core.NewRecord(items)
	qtyItem.Set("code", "GLOVES")
	qtyItem.Set("name", "Gloves")
	qtyItem.Set("type", "consumable")
	qtyItem.Set("tracking_mode", "quantity")
	qtyItem.Set("active", true)
	qtyItem.Set("quantity_on_hand", 5)
	if err := app.Save(qtyItem); err != nil {
		t.Fatalf("save quantity item: %v", err)
	}

	addInstance(t, app, qtyItem.Id, "GLOVES-X", true)

	if got := itemQty(t, app, qtyItem.Id); got != 5 {
		t.Errorf("non-serialized quantity_on_hand: want 5 (untouched), got %d", got)
	}
}

// TestRecompute_CascadeDeleteParentNoError ensures deleting the parent item
// (which cascade-deletes its instances) doesn't error out of the recompute
// hook — the item is gone, so there's nothing to recompute.
func TestRecompute_CascadeDeleteParentNoError(t *testing.T) {
	app := setupApp(t)
	New().Register(app)
	itemID, _ := seedItemWithInstance(t, app)
	addInstance(t, app, itemID, "DRILL-A-2", true)

	item, err := app.FindRecordById("items", itemID)
	if err != nil {
		t.Fatalf("find item: %v", err)
	}
	if err := app.Delete(item); err != nil {
		t.Fatalf("cascade delete parent item: %v", err)
	}
	// Instances are gone too.
	rows, err := app.FindRecordsByFilter("item_instances",
		"item = {:i}", "", 0, 0, dbx.Params{"i": itemID})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("instances after cascade delete: want 0, got %d", len(rows))
	}
}

// TestRecompute_IdempotentReplayNoDoubleCount verifies that a replayed
// command_id (the controller-retry path) doesn't double-apply: the derived
// quantity reflects the single decommission.
func TestRecompute_IdempotentReplayNoDoubleCount(t *testing.T) {
	app := setupApp(t)
	New().Register(app)
	itemID, inst := seedItemWithInstance(t, app)
	addInstance(t, app, itemID, "DRILL-A-2", true)

	in := ToggleInput{
		InstanceCode:      inst.GetString("code"),
		Reason:            "broken",
		Source:            events.SourceController,
		ControllerAdminID: "ctrl-admin",
		CommandID:         "cmd-decom-1",
	}
	if _, err := PerformSetStatus(app, in, StatusRetired); err != nil {
		t.Fatalf("first retire: %v", err)
	}
	if _, err := PerformSetStatus(app, in, StatusRetired); err != nil {
		t.Fatalf("replay retire: %v", err)
	}
	if got := itemQty(t, app, itemID); got != 1 {
		t.Errorf("after replayed retire: want qty 1 (applied once), got %d", got)
	}
}
