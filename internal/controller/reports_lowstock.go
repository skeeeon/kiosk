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
// ledger (`ledger.ReplayOpenRows`) rather than over the wire,
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
	online, offline, err := h.targetKiosks(reg, kioskCodeFilter)
	if err != nil {
		return nil, nil, err
	}

	var (
		rows []lowStockEntry
		errs []lowStockKioskError
	)
	// Offline kiosks have no controller-side fallback — the controller never
	// projects per-kiosk on-hand — so they're surfaced as unavailable rather
	// than silently dropped from the report.
	for _, code := range offline {
		errs = append(errs, lowStockKioskError{KioskCode: code, Error: "kiosk_offline"})
	}

	if len(online) > 0 {
		idToCode, ierr := loadItemIDToCodeMap(h.App)
		if ierr != nil {
			return nil, nil, ierr
		}
		for _, inv := range fanoutInventorySnapshots(nc, online) {
			if inv.err != nil {
				errs = append(errs, lowStockKioskError{KioskCode: inv.kioskCode, Error: inv.err.Error()})
				continue
			}
			kioskRows, perr := lowStockRowsForKiosk(h.App, inv.kioskCode, inv.rawData, idToCode)
			if perr != nil {
				errs = append(errs, lowStockKioskError{KioskCode: inv.kioskCode, Error: perr.Error()})
				continue
			}
			rows = append(rows, kioskRows...)
		}
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


// fanoutInventorySnapshots fires `inventory.snapshot` at each kiosk in
// parallel and gathers the replies.
func fanoutInventorySnapshots(nc *nats.Conn, kioskCodes []string) []snapshotInvocation {
	return fanoutSnapshots(nc, kioskCodes, events.InventorySnapshotCommandSubject)
}

// fanoutSnapshots fires the per-kiosk snapshot command built by subjectFn at
// each kiosk in parallel and gathers the {success,error,data} replies into
// one snapshotInvocation per kiosk. Per-kiosk timeout is the existing
// commandTimeout; total wall-clock is bounded by it because all requests run
// concurrently. Goroutine count == fleet size, which is fine in any realistic
// fleet. Shared by the low-stock (inventory.snapshot) and maintenance-digest
// (instance.snapshot) fan-outs — only the subject differs.
func fanoutSnapshots(nc *nats.Conn, kioskCodes []string, subjectFn func(string) string) []snapshotInvocation {
	results := make([]snapshotInvocation, len(kioskCodes))
	var wg sync.WaitGroup
	for i, code := range kioskCodes {
		wg.Add(1)
		go func(idx int, kc string) {
			defer wg.Done()
			results[idx].kioskCode = kc

			msg, err := nc.Request(subjectFn(kc), []byte("{}"), commandTimeout)
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

// lowStockRowsForKiosk decodes the snapshot reply and returns the rows whose
// `available` is at or below the per-item threshold. Both the threshold and the
// out-count come from the snapshot — the kiosk is the source of truth for its
// own reorder_threshold and (now) its own out-count, read from its
// open_checkouts. Transitional fallback: a kiosk that predates the `out` field
// is detected by its absence in the raw reply, and the count is derived by
// replaying the controller's projected ledger so a mid-rollout fleet still
// reports correctly.
func lowStockRowsForKiosk(app core.App, kioskCode string, rawData json.RawMessage, idToCode map[string]string) ([]lowStockEntry, error) {
	var reply struct {
		Items []struct {
			ItemCode         string `json:"item_code"`
			ItemName         string `json:"item_name"`
			QuantityOnHand   int    `json:"quantity_on_hand"`
			ReorderThreshold int    `json:"reorder_threshold"`
			TrackingMode     string `json:"tracking_mode"`
			Active           bool   `json:"active"`
			Out              int    `json:"out"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rawData, &reply); err != nil {
		return nil, err
	}

	var fallbackOut map[string]int
	if !snapshotItemsHaveOut(rawData) {
		openRows, err := ledger.ReplayOpenRows(app, kioskCode)
		if err != nil {
			return nil, err
		}
		fallbackOut = make(map[string]int, len(openRows))
		for _, r := range openRows {
			// Skip rows for items deleted on the controller after a
			// transaction referenced them — can't map id→code to fabricate one.
			if code, ok := idToCode[r.Item]; ok {
				fallbackOut[code]++
			}
		}
	}

	out := make([]lowStockEntry, 0)
	for _, item := range reply.Items {
		if !item.Active || item.ReorderThreshold <= 0 {
			continue
		}
		openCount := item.Out
		if fallbackOut != nil {
			openCount = fallbackOut[item.ItemCode]
		}
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

// snapshotItemsHaveOut reports whether an inventory.snapshot reply carries the
// per-item `out` field (new-protocol kiosk) vs omits it (pre-rollout kiosk).
// A typed decode can't distinguish absent from a genuine 0, so we probe the
// raw key presence on the first item.
func snapshotItemsHaveOut(rawData json.RawMessage) bool {
	var probe struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rawData, &probe); err != nil || len(probe.Items) == 0 {
		return false
	}
	_, ok := probe.Items[0]["out"]
	return ok
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
