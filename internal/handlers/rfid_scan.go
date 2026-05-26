// rfid_scan.go owns the counter_scan endpoint that translates one
// LLRP inventory cycle into cart lines. See docs/rfid.md for the
// design and Phase 2 plan.
package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// RFIDScanResponse is what the SPA receives back from a successful
// rfid-scan. AddedLines is the order in which lines landed (and stacked
// onto existing rows where applicable — same merge semantics as
// cart/add). UnresolvedEPCs are tags the reader observed but that we
// couldn't tie to an active item_instance — we surface them so the SPA
// can show a "3 unknown tags observed" hint instead of pretending they
// never happened. ObservedEPCs is the deduplicated full set the reader
// emitted, exactly what we publish on event.scan.rfid.observed for
// downstream audit.
type RFIDScanResponse struct {
	Cart           any          `json:"cart"`
	AddedLines     []*cart.Line `json:"added_lines"`
	ObservedEPCs   []string     `json:"observed_epcs"`
	UnresolvedEPCs []string     `json:"unresolved_epcs"`
}

// RFIDScan is the HTTP wrapper. The business logic lives in
// PerformRFIDScan; this method handles request-shape concerns
// (query-string parsing, config gating, status-code mapping).
//
// Endpoint: POST /api/kiosk/cart/rfid-scan?cart_id=<id>
func (h *Handlers) RFIDScan(re *core.RequestEvent) error {
	cartID := re.Request.URL.Query().Get("cart_id")
	if cartID == "" {
		return re.BadRequestError("cart_id is required", nil)
	}

	if !h.Cfg.RFID.Enabled {
		return re.NotFoundError("rfid is not enabled on this kiosk", nil)
	}
	if h.Cfg.RFID.Mode != "" && h.Cfg.RFID.Mode != "counter_scan" {
		// enclosure_diff has its own NATS-driven entry; the operator
		// shouldn't be able to fire this from the touchscreen.
		return re.NotFoundError("rfid scan button is only available in counter_scan mode", nil)
	}
	if h.RFID == nil {
		return re.JSON(http.StatusServiceUnavailable, map[string]any{
			"error": "rfid_unavailable",
			"hint":  "reader connection was not established at startup",
		})
	}

	resp, err := h.PerformRFIDScan(re.Request.Context(), cartID)
	switch {
	case err == nil:
		return re.JSON(http.StatusOK, resp)
	case errors.Is(err, errCartNotFound):
		return re.NotFoundError("cart not found or expired", nil)
	case errors.Is(err, errRFIDReadFailed):
		return re.JSON(http.StatusServiceUnavailable, map[string]any{
			"error":   "rfid_read_failed",
			"message": errors.Unwrap(err).Error(),
		})
	default:
		return re.InternalServerError("rfid scan failed", err)
	}
}

// errRFIDReadFailed wraps the underlying LLRP error from ReadFor so
// the HTTP layer can distinguish reader-unreachable from other
// failures while preserving the underlying message for the operator.
type rfidReadErr struct{ err error }

func (e *rfidReadErr) Error() string { return "rfid read failed: " + e.err.Error() }
func (e *rfidReadErr) Unwrap() error { return e.err }
func (*rfidReadErr) Is(target error) bool {
	_, ok := target.(*rfidReadErr)
	return ok
}

var errRFIDReadFailed = &rfidReadErr{err: errors.New("placeholder")}

// PerformRFIDScan is the testable core: runs one LLRP inventory cycle
// scoped to the given cart, resolves each observed EPC against
// item_instances, and adds matched instances via the shared
// addCodeToCart path. Per-EPC errors (inactive instance, duplicate in
// cart, etc.) are skip-and-logged so a single bad tag doesn't fail
// the batch — the SPA shows the rest as added and the operator can
// retry. After the batch we publish event.scan.rfid.observed with
// the full EPC array for downstream observability.
//
// Caller (the HTTP wrapper) is responsible for the config-enabled +
// reader-connected pre-checks; this method assumes h.RFID is non-nil
// and uses h.Cfg.RFID.ReadWindow as the inventory-cycle duration.
func (h *Handlers) PerformRFIDScan(ctx context.Context, cartID string) (*RFIDScanResponse, error) {
	// Fail fast on a stale cart before burning a 3-second read window.
	// addCodeToCart re-resolves per EPC anyway; this just turns a
	// stale cart_id into a clean error without the round-trip.
	if _, err := h.Carts.Get(cartID); err != nil {
		return nil, errCartNotFound
	}

	window := h.Cfg.RFID.ReadWindow.AsDuration()
	if window <= 0 {
		window = 3 * time.Second
	}

	observed, err := h.RFID.ReadFor(ctx, window)
	if err != nil {
		return nil, &rfidReadErr{err: err}
	}

	// Translate to plain strings once for both response and event.
	observedStrings := make([]string, 0, len(observed))
	for _, e := range observed {
		observedStrings = append(observedStrings, string(e))
	}

	var (
		addedLines     = make([]*cart.Line, 0, len(observed))
		unresolvedEPCs = make([]string, 0)
		latestCart     any
	)
	for _, epc := range observed {
		c, added, err := h.addCodeToCart(cartID, string(epc))
		switch {
		case err == nil:
			addedLines = append(addedLines, added)
			latestCart = c
		case errors.Is(err, errCodeNotFound):
			// Surface unknown tags so the SPA can render "3 unknown
			// observed" rather than silently dropping.
			unresolvedEPCs = append(unresolvedEPCs, string(epc))
		case errors.Is(err, errCartNotFound):
			return nil, errCartNotFound
		default:
			// Skip-and-log per-tag — the full observed set still rides
			// the event for audit so nothing is truly lost.
			log.Printf("rfid-scan: skip EPC %s: %v", epc, err)
		}
	}

	// Always publish, even on zero tags — "operator hit the button and
	// the antenna saw nothing" is itself a useful signal.
	id := kioskctx.Get()
	events.Publish(events.ScanRFIDObservedSubject(id.KioskCode), map[string]any{
		"kiosk_code":    id.KioskCode,
		"location_code": id.LocationCode,
		"cart_id":       cartID,
		"mode":          h.Cfg.RFID.Mode,
		"observed_epcs": observedStrings,
		"observed_at":   time.Now().UTC(),
	})

	if latestCart == nil {
		c, _ := h.Carts.Get(cartID)
		latestCart = c
	}

	// One broker tickle for the whole batch (not per-EPC). The SPA
	// refetches once and sees the merged state regardless of how many
	// lines landed. Fires even on zero-add reads so subscribers still
	// know the operator hit the button.
	h.CartEvents.Tickle(cartID)

	return &RFIDScanResponse{
		Cart:           latestCart,
		AddedLines:     addedLines,
		ObservedEPCs:   observedStrings,
		UnresolvedEPCs: unresolvedEPCs,
	}, nil
}
