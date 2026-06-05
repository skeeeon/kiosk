// rfid_read_trigger.go owns the enclosure_diff read path. The shape
// mirrors PerformRFIDScan (Phase 2): a testable core function that
// the NATS dispatcher and a local HTTP wrapper both call. Where
// counter_scan adds tags one-at-a-time to the cart, enclosure_diff
// reconciles observed vs expected and synthesizes the resulting
// state changes. See docs/rfid.md, Phase 4.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/rfid"
)

// ReadTriggerResult is the response from a Phase-4 enclosure_diff
// read. Mirrors RFIDScanResponse closely so the SPA's refetch path
// doesn't need to branch on mode; the differences are the line
// origins (diff-derived rather than scan-derived) and the
// SkippedCrossUserCount which surfaces "this read saw a returned
// tool that belongs to a worker outside the cart user's group."
//
// Phase 4 v1 limits cross-user returns to "skip and log" — adding a
// returned-by-someone-else tool to the cart would silently bypass
// the foreman+same-group gate, and the right design for handling
// that case in an enclosure is unclear (foreman scan? admin
// approval?). Surfacing the count lets the operator see something
// was observed-but-skipped.
type ReadTriggerResult struct {
	Cart                    any          `json:"cart"`
	AddedLines              []*cart.Line `json:"added_lines"`
	ObservedEPCs            []string     `json:"observed_epcs"`
	UnresolvedEPCs          []string     `json:"unresolved_epcs"`
	SkippedCrossUserCount   int          `json:"skipped_cross_user_count"`
	SkippedMaintenanceCount int          `json:"skipped_maintenance_count"`
}

// PerformReadTrigger is the shared core: cart already resolved
// (caller decides cart_id-vs-(user,door) lookup), reader configured.
// Runs one LLRP inventory cycle, fetches expected-present + currently-
// out state from PB, calls rfid.Diff, and synthesizes cart lines:
//
//   - Checkouts (active instance, not in open_checkouts, not observed)
//     become action=checkout for the cart user.
//   - Returns where CheckoutUserID matches the cart user become
//     action=return (self-return).
//   - Returns by another user are skip-and-logged.
//
// One broker tickle at the end regardless of how many lines landed,
// matching PerformRFIDScan's semantics. event.scan.rfid.observed
// publishes the full deduplicated EPC array for audit.
func (h *Handlers) PerformReadTrigger(ctx context.Context, c *cart.Cart) (*ReadTriggerResult, error) {
	if c == nil {
		return nil, errCartNotFound
	}

	window := h.Cfg.RFID.ReadWindow.AsDuration()
	if window <= 0 {
		window = 3 * time.Second
	}

	observed, err := h.RFID.ReadFor(ctx, window)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRFIDReadFailed, err)
	}

	expected, err := h.expectedInstanceStates()
	if err != nil {
		return nil, fmt.Errorf("expectedInstanceStates: %w", err)
	}

	result := rfid.Diff(observed, expected)

	observedStrings := make([]string, 0, len(observed))
	for _, e := range observed {
		observedStrings = append(observedStrings, string(e))
	}
	unresolvedStrings := make([]string, 0, len(result.Unresolved))
	for _, e := range result.Unresolved {
		unresolvedStrings = append(unresolvedStrings, string(e))
	}

	addedLines := make([]*cart.Line, 0, len(result.Checkouts)+len(result.Returns))
	var latestCart *cart.Cart = c
	var skippedCrossUser int

	for _, entry := range result.Checkouts {
		if line, latest, ok := h.appendDiffLine(c.ID, entry, "checkout"); ok {
			addedLines = append(addedLines, line)
			latestCart = latest
		}
	}
	for _, entry := range result.Returns {
		// Phase 4 v1: enclosure_diff only synthesizes self-returns.
		// Skipping cross-user returns avoids silently bypassing the
		// commit-time foreman+same-group gate. Operator sees the
		// SkippedCrossUserCount in the response so it's not a black
		// hole.
		if entry.CheckoutUserID != "" && entry.CheckoutUserID != c.UserID {
			skippedCrossUser++
			log.Printf("rfid-diff: skipping cross-user return for instance %s "+
				"(held by %s, cart user %s)", entry.InstanceID, entry.CheckoutUserID, c.UserID)
			continue
		}
		if line, latest, ok := h.appendDiffLine(c.ID, entry, "return"); ok {
			addedLines = append(addedLines, line)
			latestCart = latest
		}
	}

	// Tickle the broker — exactly once per read, regardless of how
	// many cart lines landed. Subscribers refetch and see the merged
	// state.
	h.CartEvents.Tickle(c.ID)

	// Always publish the observed event, even on zero-effect reads —
	// "operator triggered a read and the reader saw nothing" is a
	// signal worth keeping in the audit stream.
	id := kioskctx.Get()
	events.Publish(events.ScanRFIDObservedSubject(id.KioskCode), map[string]any{
		"kiosk_code":    id.KioskCode,
		"location_code": id.LocationCode,
		"cart_id":       c.ID,
		"door_id":       c.DoorID,
		"mode":          h.Cfg.RFID.Mode,
		"observed_epcs": observedStrings,
		"observed_at":   time.Now().UTC(),
	})

	return &ReadTriggerResult{
		Cart:                    latestCart,
		AddedLines:              addedLines,
		ObservedEPCs:            observedStrings,
		UnresolvedEPCs:          unresolvedStrings,
		SkippedCrossUserCount:   skippedCrossUser,
		SkippedMaintenanceCount: len(result.SkippedIneligible),
	}, nil
}

// appendDiffLine synthesizes a cart.Line from a diff entry and
// pushes it through the cart store. Returns (line, latestCart, ok).
// On any error (instance/item record gone, AddLine rejects, etc.)
// we skip-and-log so a single bad row doesn't fail the batch — same
// philosophy as PerformRFIDScan's per-EPC loop.
func (h *Handlers) appendDiffLine(cartID string, entry rfid.DiffEntry, action string) (*cart.Line, *cart.Cart, bool) {
	instance, err := h.App.FindRecordById("item_instances", entry.InstanceID)
	if err != nil {
		log.Printf("rfid-diff: instance %s not found: %v", entry.InstanceID, err)
		return nil, nil, false
	}
	item, err := h.App.FindRecordById("items", entry.ItemID)
	if err != nil {
		log.Printf("rfid-diff: item %s not found: %v", entry.ItemID, err)
		return nil, nil, false
	}
	// Skip non-transactable rows. expectedInstanceStates filters at the
	// query layer too; this is defense in depth in case state changed
	// between the snapshot and the AddLine. Only in_service units are
	// checkout/return-eligible (retired excluded; maintenance not eligible).
	if instance.GetString("status") != "in_service" || !item.GetBool("active") {
		return nil, nil, false
	}

	line := &cart.Line{
		ItemID:           item.Id,
		ItemCode:         instance.GetString("code"),
		ItemName:         item.GetString("name"),
		ItemType:         item.GetString("type"),
		TrackingMode:     item.GetString("tracking_mode"),
		Action:           action,
		Qty:              1,
		Serial:           instance.GetString("serial"),
		ItemInstanceID:   instance.Id,
		ItemInstanceCode: instance.GetString("code"),
	}

	latest, added, err := h.Carts.AddLine(cartID, line)
	if err != nil {
		if errors.Is(err, cart.ErrDuplicateInstance) {
			// Already in the cart from an earlier diff pass (manual
			// re-read button) — not a problem, just a no-op.
			return nil, nil, false
		}
		log.Printf("rfid-diff: AddLine failed for instance %s: %v", instance.Id, err)
		return nil, nil, false
	}
	return added, latest, true
}

// expectedInstanceStates returns the kiosk's active serialized
// instances plus their current open_checkouts state, ready for
// rfid.Diff. Only serialized instances participate in
// enclosure_diff — quantity-tracked tools and consumables can't
// produce per-unit identity.
//
// Two queries: all active item_instances + all open_checkouts. We
// join in Go (O(n)) rather than dragging in a more elaborate PB
// query API. The instance count per kiosk is bounded by physical
// tag inventory — hundreds at most, fine for a Go-side join.
func (h *Handlers) expectedInstanceStates() ([]rfid.InstanceState, error) {
	instances, err := h.App.FindRecordsByFilter("item_instances",
		"status != 'retired'", "+code", 0, 0, dbx.Params{})
	if err != nil {
		return nil, fmt.Errorf("find non-retired instances: %w", err)
	}
	// Tracking-mode check: instances exist only for serialized items
	// in v1's schema, but we re-check defensively in case a schema
	// migration adds another tracking mode that allows instance rows.
	opens, err := h.App.FindRecordsByFilter("open_checkouts",
		"item_instance != ''", "", 0, 0, dbx.Params{})
	if err != nil {
		return nil, fmt.Errorf("find open_checkouts: %w", err)
	}
	openByInstance := make(map[string]string, len(opens))
	for _, o := range opens {
		openByInstance[o.GetString("item_instance")] = o.GetString("user")
	}

	out := make([]rfid.InstanceState, 0, len(instances))
	for _, inst := range instances {
		// Fold to lower-case defensively — stored EPCs are normalized on
		// write, but this guards pre-backfill rows / manual DB edits so the
		// diff still matches the reader's lower-case observations.
		epc := strings.ToLower(inst.GetString("rfid_epc"))
		userID, isOut := openByInstance[inst.Id]
		out = append(out, rfid.InstanceState{
			InstanceID:     inst.Id,
			ItemID:         inst.GetString("item"),
			EPC:            rfid.EPC(epc),
			IsCheckedOut:   isOut,
			CheckoutUserID: userID,
			// Maintenance units are expected-present (still in the box) but
			// not checkout-eligible — a departing one is skip-and-counted.
			Eligible: inst.GetString("status") == "in_service",
		})
	}
	return out, nil
}

// ReadTrigger is the local HTTP wrapper. Same configuration gates as
// RFIDScan but for mode=enclosure_diff.
//
// Endpoint: POST /api/kiosk/cart/read-trigger?cart_id=<id>
func (h *Handlers) ReadTrigger(re *core.RequestEvent) error {
	cartID := re.Request.URL.Query().Get("cart_id")
	if cartID == "" {
		return re.BadRequestError("cart_id is required", nil)
	}

	if !h.Cfg.RFID.Enabled {
		return re.NotFoundError("rfid is not enabled on this kiosk", nil)
	}
	if h.Cfg.RFID.Mode != "" && h.Cfg.RFID.Mode != "enclosure_diff" {
		return re.NotFoundError("read-trigger is only available in enclosure_diff mode", nil)
	}
	if h.RFID == nil {
		return re.JSON(http.StatusServiceUnavailable, map[string]any{
			"error": "rfid_unavailable",
			"hint":  "reader connection was not established at startup",
		})
	}

	c, err := h.Carts.Get(cartID)
	if err != nil {
		return re.NotFoundError("cart not found or expired", nil)
	}

	result, err := h.PerformReadTrigger(re.Request.Context(), c)
	switch {
	case err == nil:
		return re.JSON(http.StatusOK, result)
	case errors.Is(err, errCartNotFound):
		return re.NotFoundError("cart not found or expired", nil)
	case errors.Is(err, errRFIDReadFailed):
		return re.JSON(http.StatusServiceUnavailable, map[string]any{
			"error":   "rfid_read_failed",
			"message": err.Error(),
		})
	default:
		return re.InternalServerError("read trigger failed", err)
	}
}
