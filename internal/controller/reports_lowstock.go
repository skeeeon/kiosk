package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/ledger"
)

// Fleet-wide low-stock report. Implemented as a snapshot fan-out — one
// `inventory.snapshot` NATS request per currently-online kiosk, executed in
// parallel — rather than a controller-side qty projection. Rationale:
//
//   - Live values, no projection drift to reconcile.
//   - No new schema or migration.
//   - Reuses the existing command bus already wired for the per-kiosk
//     inventory panel.
//
// Out-counts are computed locally from the controller's projected
// open_checkouts table (`ledger.ReadOpenRows`) rather than over the wire,
// so the snapshot command's payload doesn't need to grow. The two sources
// are joined by item_code (the cross-fleet stable identifier).
//
// Trade-off: offline kiosks are excluded from the result and surfaced in
// the `errors` section so the operator sees a partial-report indicator.
// Fan-out cost is O(fleet) NATS round-trips per page load; fine well into
// the dozens. If/when it becomes painful, swap this for a persistent
// `kiosk_inventory` projection consuming the same events the aggregator
// already filters on.

// lowStockEntry is one (kiosk, item) row in the fleet-wide low-stock
// response. Flat shape so the SPA can drive a single DataTable.
type lowStockEntry struct {
	KioskCode        string `json:"kiosk_code"`
	ItemCode         string `json:"item_code"`
	ItemName         string `json:"item_name"`
	TrackingMode     string `json:"tracking_mode"`
	QuantityOnHand   int    `json:"quantity_on_hand"`
	Out              int    `json:"out"`
	Available        int    `json:"available"`
	ReorderThreshold int    `json:"reorder_threshold"`
}

// lowStockKioskError annotates kiosks that didn't contribute to the report
// — offline, timed out, or replied with an error envelope. Operators need
// to know the data is partial.
type lowStockKioskError struct {
	KioskCode string `json:"kiosk_code"`
	Error     string `json:"error"`
}

type lowStockResponse struct {
	Rows   []lowStockEntry      `json:"rows"`
	Errors []lowStockKioskError `json:"errors,omitempty"`
}

// snapshotInvocation is the (kiosk, response, error) triplet collected from
// each fan-out goroutine before reduction. Keeping the wire decode in the
// reducer (single-threaded) avoids leaking sync.Pool / encoding state across
// goroutines.
type snapshotInvocation struct {
	kioskCode string
	rawData   json.RawMessage // env.Data from the kiosk's reply envelope
	err       error
}

// ReportLowStock returns GET /api/controller/reports/low-stock.
// Admin-gated; requires NATS for the snapshot fan-out.
//
// The optional ?kiosk_code= query param scopes the fan-out to a single
// kiosk so the page-level kiosk filter on the SPA's Reports view doesn't
// pay for a full-fleet round-trip every change. Unknown codes produce an
// empty result rather than an error — same shape as the all-kiosks case
// with zero rows.
func (h *Handlers) ReportLowStock(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		rows, errs, err := h.gatherLowStock(nc, reg, re.Request.URL.Query().Get("kiosk_code"))
		if err != nil {
			return re.InternalServerError("gather low-stock", err)
		}
		if rows == nil {
			rows = []lowStockEntry{}
		}
		return re.JSON(http.StatusOK, lowStockResponse{Rows: rows, Errors: errs})
	}
}

// ReportLowStockCSV is the CSV companion. The errors set (offline kiosks)
// is intentionally not embedded — the JSON endpoint already surfaces it for
// on-screen rendering; the CSV is data-only.
func (h *Handlers) ReportLowStockCSV(nc *nats.Conn, reg *HeartbeatRegistry) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		rows, _, err := h.gatherLowStock(nc, reg, re.Request.URL.Query().Get("kiosk_code"))
		if err != nil {
			return re.InternalServerError("gather low-stock", err)
		}
		out := make([]exports.LowStockRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, exports.LowStockRow{
				KioskCode:        r.KioskCode,
				ItemCode:         r.ItemCode,
				ItemName:         r.ItemName,
				TrackingMode:     r.TrackingMode,
				QuantityOnHand:   r.QuantityOnHand,
				Out:              r.Out,
				Available:        r.Available,
				ReorderThreshold: r.ReorderThreshold,
			})
		}

		w := re.Response
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			"attachment; filename=\"controller-low-stock-%s.csv\"",
			time.Now().UTC().Format("20060102-150405"),
		))
		return exports.WriteLowStockCSV(w, out)
	}
}

// gatherLowStock is the shared fan-out + reduce used by both the JSON and
// CSV endpoints. Returns the deterministically-ordered rows plus the
// per-kiosk error list; only catalog-load errors are fatal (returned as
// err) because they mean we couldn't bridge the snapshot codes back to
// item_ids — any individual kiosk error is just a row in errs.
func (h *Handlers) gatherLowStock(nc *nats.Conn, reg *HeartbeatRegistry, kioskCodeFilter string) ([]lowStockEntry, []lowStockKioskError, error) {
	targets := onlineKiosks(reg, heartbeatFreshness)
	if kioskCodeFilter != "" {
		targets = filterToCode(targets, kioskCodeFilter)
	}
	if len(targets) == 0 {
		return nil, nil, nil
	}

	results := fanoutInventorySnapshots(nc, targets)
	idToCode, err := loadItemIDToCodeMap(h.App)
	if err != nil {
		return nil, nil, err
	}

	var (
		rows []lowStockEntry
		errs []lowStockKioskError
	)
	for _, inv := range results {
		if inv.err != nil {
			errs = append(errs, lowStockKioskError{
				KioskCode: inv.kioskCode,
				Error:     inv.err.Error(),
			})
			continue
		}
		kioskRows, perr := lowStockRowsForKiosk(h.App, inv.kioskCode, inv.rawData, idToCode)
		if perr != nil {
			errs = append(errs, lowStockKioskError{
				KioskCode: inv.kioskCode,
				Error:     perr.Error(),
			})
			continue
		}
		rows = append(rows, kioskRows...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ItemCode != rows[j].ItemCode {
			return rows[i].ItemCode < rows[j].ItemCode
		}
		return rows[i].KioskCode < rows[j].KioskCode
	})
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].KioskCode < errs[j].KioskCode })
	return rows, errs, nil
}

// filterToCode reduces the kiosks list to a single code if present. Used
// to honor the SPA's page-level kiosk filter without a separate code path.
func filterToCode(kioskCodes []string, want string) []string {
	for _, c := range kioskCodes {
		if c == want {
			return []string{c}
		}
	}
	return nil
}

// onlineKiosks returns kiosks whose last heartbeat is within the freshness
// window. Pure filter over the registry snapshot — no NATS calls.
func onlineKiosks(reg *HeartbeatRegistry, freshness time.Duration) []string {
	cutoff := time.Now().Add(-freshness)
	beats := reg.Snapshot()
	out := make([]string, 0, len(beats))
	for code, beat := range beats {
		if beat.After(cutoff) {
			out = append(out, code)
		}
	}
	sort.Strings(out)
	return out
}

// fanoutInventorySnapshots fires `inventory.snapshot` at each kiosk in
// parallel and gathers the replies. Per-kiosk timeout is the existing
// commandTimeout; total wall-clock is bounded by it because all requests
// run concurrently. Goroutine count == fleet size, which is fine in any
// realistic fleet.
func fanoutInventorySnapshots(nc *nats.Conn, kioskCodes []string) []snapshotInvocation {
	results := make([]snapshotInvocation, len(kioskCodes))
	var wg sync.WaitGroup
	for i, code := range kioskCodes {
		wg.Add(1)
		go func(idx int, kc string) {
			defer wg.Done()
			results[idx].kioskCode = kc

			subject := events.InventorySnapshotCommandSubject(kc)
			msg, err := nc.Request(subject, []byte("{}"), commandTimeout)
			if err != nil {
				if errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrNoResponders) {
					results[idx].err = errors.New("kiosk_offline")
					return
				}
				results[idx].err = err
				return
			}
			var env kioskCommandEnvelope
			if jerr := json.Unmarshal(msg.Data, &env); jerr != nil {
				results[idx].err = jerr
				return
			}
			if !env.Success {
				if env.Error == "" {
					results[idx].err = errors.New("kiosk_error")
				} else {
					results[idx].err = errors.New(env.Error)
				}
				return
			}
			results[idx].rawData = env.Data
		}(i, code)
	}
	wg.Wait()
	return results
}

// lowStockRowsForKiosk decodes the snapshot reply, computes per-item
// out-counts from the controller's projected ledger, and returns the rows
// whose `available` is at or below the per-item threshold. The threshold
// itself comes from the snapshot — the kiosk is the source of truth for
// its own `reorder_threshold`, since admins can adjust it locally.
func lowStockRowsForKiosk(app core.App, kioskCode string, rawData json.RawMessage, idToCode map[string]string) ([]lowStockEntry, error) {
	var reply struct {
		Items []struct {
			ItemCode         string `json:"item_code"`
			ItemName         string `json:"item_name"`
			QuantityOnHand   int    `json:"quantity_on_hand"`
			ReorderThreshold int    `json:"reorder_threshold"`
			TrackingMode     string `json:"tracking_mode"`
			Active           bool   `json:"active"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rawData, &reply); err != nil {
		return nil, err
	}

	// Out-count per item_code. ReplayOpenRows reconstructs the open rows from
	// the projected transaction_lines ledger (the controller no longer
	// materializes open_checkouts); the map through the catalog joins
	// item_id → item_code so the count pairs with the snapshot's item_code.
	openRows, err := ledger.ReplayOpenRows(app, kioskCode)
	if err != nil {
		return nil, err
	}
	outByCode := make(map[string]int, len(openRows))
	for _, r := range openRows {
		code, ok := idToCode[r.Item]
		if !ok {
			// Item was deleted on the controller after a transaction
			// referenced it. Counts toward "out" only if we can match
			// — skip it rather than fabricate a code.
			continue
		}
		outByCode[code]++
	}

	out := make([]lowStockEntry, 0)
	for _, item := range reply.Items {
		if !item.Active {
			continue
		}
		if item.ReorderThreshold <= 0 {
			continue
		}
		openCount := outByCode[item.ItemCode]
		available := item.QuantityOnHand - openCount
		if available < 0 {
			available = 0
		}
		if available > item.ReorderThreshold {
			continue
		}
		out = append(out, lowStockEntry{
			KioskCode:        kioskCode,
			ItemCode:         item.ItemCode,
			ItemName:         item.ItemName,
			TrackingMode:     item.TrackingMode,
			QuantityOnHand:   item.QuantityOnHand,
			Out:              openCount,
			Available:        available,
			ReorderThreshold: item.ReorderThreshold,
		})
	}
	return out, nil
}

// loadItemIDToCodeMap builds the id→code translation used to bridge the
// ledger (id-keyed) with the snapshot reply (code-keyed). One pass over
// `items` per request — the controller's catalog is typically small
// enough that this is cheaper than caching with invalidation.
func loadItemIDToCodeMap(app core.App) (map[string]string, error) {
	rows, err := app.FindRecordsByFilter("items", "", "", 0, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Id] = r.GetString("code")
	}
	return out, nil
}
