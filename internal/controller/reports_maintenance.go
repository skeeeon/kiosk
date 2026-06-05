package controller

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/notifications"
)

// Fleet-wide maintenance digest. The controller's local item_instances table
// is empty (instances live on the kiosks), so the standalone scheduler runner
// can't answer "what's in maintenance across the fleet." This runner replaces
// it via the same snapshot fan-out the live low-stock report uses: one
// `instance.snapshot` NATS request per online kiosk, in parallel, filtered to
// status=maintenance. Offline kiosks are surfaced in OfflineKiosks so the
// operator knows the digest is partial — same trade-off as low-stock.
//
// Wired in cmd/controller/main.go via scheduler.RegisterRunner("maintenance",
// h.MaintenanceDigestRunner(nc, reg)) once the NATS conn + heartbeat registry
// exist; the runner closure captures both, which the scheduler's
// (app, row)-only runner signature can't carry.

// MaintenanceDigestRunner returns a scheduler report runner that builds a
// MaintenanceDigestContext from a live fleet-wide instance-snapshot fan-out.
// The row's kiosk_code scopes the fan-out to a single kiosk when set; empty
// means fleet-wide.
func (h *Handlers) MaintenanceDigestRunner(nc *nats.Conn, reg *HeartbeatRegistry) func(app core.App, row *core.Record) (string, any, error) {
	return func(_ core.App, row *core.Record) (string, any, error) {
		rowKioskCode := row.GetString("kiosk_code")
		rows, prov := h.gatherMaintenanceUnits(nc, reg, rowKioskCode)
		return notifications.EventTypeMaintenanceDigest, notifications.MaintenanceDigestContext{
			Kiosk:           notifications.KioskInfo{Code: rowKioskCode},
			GeneratedAt:     time.Now().UTC(),
			Rows:            rows,
			RowsCount:       len(rows),
			KioskProvenance: prov,
		}, nil
	}
}

// gatherMaintenanceUnits collects the units in maintenance fleet-wide (or for a
// single scoped kiosk) NATS-first: online kiosks answer from their live
// item_instances via instance.snapshot; offline kiosks fall back to
// reconstructing current status from the controller's projected
// instance_lifecycle_audit (last-known) — see replayInstanceStatuses — rather
// than being silently dropped as before. A kiosk we believe online that fails
// to answer is reported Unavailable, not replayed. Deterministically ordered so
// the digest body is stable across runs, with a provenance breakdown so the
// digest can flag partial results.
func (h *Handlers) gatherMaintenanceUnits(nc *nats.Conn, reg *HeartbeatRegistry, kioskCodeFilter string) ([]notifications.MaintenanceDigestRow, notifications.KioskProvenance) {
	var (
		rows []notifications.MaintenanceDigestRow
		prov notifications.KioskProvenance
	)
	online, offline, err := h.targetKiosks(reg, kioskCodeFilter)
	if err != nil {
		return rows, prov
	}

	if len(online) > 0 {
		for _, inv := range fanoutSnapshots(nc, online, events.InstanceSnapshotCommandSubject) {
			if inv.err != nil {
				prov.UnavailableKiosks = append(prov.UnavailableKiosks, inv.kioskCode)
				continue
			}
			kioskRows, perr := maintenanceRowsForKiosk(inv.kioskCode, inv.rawData)
			if perr != nil {
				prov.UnavailableKiosks = append(prov.UnavailableKiosks, inv.kioskCode)
				continue
			}
			rows = append(rows, kioskRows...)
			prov.LiveKiosks = append(prov.LiveKiosks, inv.kioskCode)
		}
	}

	for _, code := range offline {
		kioskRows, rerr := replayInstanceStatuses(h.App, code)
		if rerr != nil {
			prov.UnavailableKiosks = append(prov.UnavailableKiosks, code)
			continue
		}
		rows = append(rows, kioskRows...)
		prov.LastKnownKiosks = append(prov.LastKnownKiosks, code)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].KioskCode != rows[j].KioskCode {
			return rows[i].KioskCode < rows[j].KioskCode
		}
		if rows[i].ItemCode != rows[j].ItemCode {
			return rows[i].ItemCode < rows[j].ItemCode
		}
		return rows[i].InstanceCode < rows[j].InstanceCode
	})
	sortProvenance(&prov)
	return rows, prov
}

// maintenanceRowsForKiosk decodes an instance-snapshot reply and returns the
// units in maintenance, tagged with their origin kiosk. The kiosk is the
// source of truth for its own instance statuses, so no controller-side
// projection is consulted.
func maintenanceRowsForKiosk(kioskCode string, rawData json.RawMessage) ([]notifications.MaintenanceDigestRow, error) {
	var reply struct {
		Instances []struct {
			InstanceCode string `json:"instance_code"`
			ItemCode     string `json:"item_code"`
			ItemName     string `json:"item_name"`
			Serial       string `json:"serial"`
			Status       string `json:"status"`
			Notes        string `json:"notes"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(rawData, &reply); err != nil {
		return nil, err
	}
	out := make([]notifications.MaintenanceDigestRow, 0)
	for _, inst := range reply.Instances {
		if inst.Status != "maintenance" {
			continue
		}
		out = append(out, notifications.MaintenanceDigestRow{
			ItemCode:     inst.ItemCode,
			ItemName:     inst.ItemName,
			InstanceCode: inst.InstanceCode,
			Serial:       inst.Serial,
			Notes:        inst.Notes,
			KioskCode:    kioskCode,
		})
	}
	return out, nil
}
