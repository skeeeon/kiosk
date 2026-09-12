package demoseed

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/instances"
)

// SeedReason is stamped on every audit row the appliers write, so an
// operator scrolling the stock-adjustment or instance history can tell
// seeded state from anything a human or the demo rules did.
const SeedReason = "Northwind demo seed"

// ApplyLocal installs one node's kiosk-local state in-process: reorder
// thresholds, quantity-on-hand, and the serialized units in its cabinets.
// No NATS involved — this is the standalone demo's whole second half, and
// it runs inside the kiosk binary against its own database.
//
// It calls exactly the functions the kiosk's own NATS command handlers
// call, which is the point: ApplyRemote sends those same values as
// commands, the handlers land on the same two functions, and the two
// appliers cannot drift.
//
// Idempotent by natural key throughout. A stock adjustment is skipped when
// the quantity already matches (so a re-run doesn't litter
// stock_adjustments with zero-delta rows), and a unit is skipped when its
// instance code already exists.
func ApplyLocal(app core.App, kioskCode string, log Logf) (*Result, error) {
	if log == nil {
		log = discard
	}
	node, err := NodeByCode(kioskCode)
	if err != nil {
		return nil, err
	}
	if node.Timeclock {
		return nil, fmt.Errorf("%s is the virtual timeclock terminal: it stocks nothing and has no local state to apply", kioskCode)
	}
	adminID, err := DemoAdminID(app)
	if err != nil {
		return nil, err
	}

	out := &Result{}
	if err := applyStockLocal(app, node, adminID, out, log); err != nil {
		return nil, err
	}
	if err := applyUnitsLocal(app, node, adminID, out, log); err != nil {
		return nil, err
	}
	return out, nil
}

func applyStockLocal(app core.App, node Node, adminID string, out *Result, log Logf) error {
	for _, s := range node.Stock {
		item, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": s.ItemCode})
		if err != nil {
			return fmt.Errorf("stock %s: find item: %w", s.ItemCode, err)
		}
		if item.GetString("tracking_mode") == "serialized" {
			// The fixture validator catches this too; the guard is here so
			// a hand-edited table fails with the reason rather than with
			// ErrSerializedNotAdjustable from three frames down.
			return fmt.Errorf("stock %s: serialized SKUs derive quantity_on_hand from their instance count and must not carry a Stock entry", s.ItemCode)
		}

		// reorder_threshold is a direct field write. It does not cross the
		// catalogue wire (internal/catalog/payload.go names it kiosk-local)
		// and there is no command that sets it, so this is the only way to
		// place it — see "What propagates, and what does not" in
		// docs/demo-plan.md. One line here beats widening a test-guarded
		// payload for a demo's convenience.
		if item.GetInt("reorder_threshold") != s.ReorderThreshold {
			item.Set("reorder_threshold", s.ReorderThreshold)
			if err := app.Save(item); err != nil {
				return fmt.Errorf("stock %s: set reorder_threshold: %w", s.ItemCode, err)
			}
			out.Applied++
			log("demo-seed: %s %s reorder_threshold=%d", node.Code, s.ItemCode, s.ReorderThreshold)
		}

		if item.GetInt("quantity_on_hand") == s.Quantity {
			out.Existing++
			continue
		}
		result, err := handlers.PerformStockAdjustment(app, item.Id, adminID,
			events.SourceLocal, "", "absolute", s.Quantity, SeedReason)
		if err != nil {
			return fmt.Errorf("stock %s: %w", s.ItemCode, err)
		}
		// Publish the same event the HTTP handler publishes. On a
		// standalone kiosk there is no publisher and this is a no-op; on a
		// kiosk that is managed but seeded locally, the controller's
		// inventory_audit sees the adjustment exactly as it would any
		// other.
		handlers.PublishInventoryAdjustEvent(app, result, events.SourceLocal, adminID,
			"absolute", s.Quantity, SeedReason)
		out.Applied++
		log("demo-seed: %s %s qty %d -> %d", node.Code, s.ItemCode, result.PrevQuantity, result.NewQuantity)
	}
	return nil
}

func applyUnitsLocal(app core.App, node Node, adminID string, out *Result, log Logf) error {
	for _, u := range node.Units {
		exists, err := instanceExists(app, u.Code)
		if err != nil {
			return fmt.Errorf("unit %s: %w", u.Code, err)
		}
		if exists {
			out.Existing++
			continue
		}
		outcome, err := instances.PerformCreate(app, instances.CreateInput{
			ItemCode:    u.ItemCode,
			Code:        u.Code,
			Serial:      u.Serial,
			RFIDEPC:     u.EPC(),
			EnclosureID: u.EnclosureID,
			Notes:       SeedReason,
			Source:      events.SourceLocal,
			AdminID:     adminID,
			// No CommandID: that column is the idempotency anchor for
			// controller commands crossing an unreliable bus. This path
			// has no bus, so existence-by-instance-code is its idempotency
			// and there is nothing for a command_id to dedupe.
		})
		if err != nil {
			return fmt.Errorf("unit %s: %w", u.Code, err)
		}
		// The item_instances record hooks already wrote the audit row and
		// recomputed items.quantity_on_hand — do NOT hand-write either.
		// Only the lifecycle event is ours to publish, mirroring what the
		// command handler does.
		instances.PublishLifecycle(app, outcome)
		out.Applied++
		log("demo-seed: %s unit %s (%s) in cabinet %s", node.Code, u.Code, u.ItemCode, orNone(u.EnclosureID))
	}
	return nil
}

func instanceExists(app core.App, code string) (bool, error) {
	_, err := app.FindFirstRecordByFilter("item_instances", "code = {:c}", dbx.Params{"c": code})
	if err == nil {
		return true, nil
	}
	if dberr.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
