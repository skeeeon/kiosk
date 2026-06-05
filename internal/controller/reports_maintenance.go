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
		rows, offline := gatherMaintenanceUnits(nc, reg, rowKioskCode)
		ctx := notifications.MaintenanceDigestContext{
			Kiosk:         notifications.KioskInfo{Code: rowKioskCode},
			GeneratedAt:   time.Now().UTC(),
			Rows:          rows,
			RowsCount:     len(rows),
			OfflineKiosks: offline,
		}
		return notifications.EventTypeMaintenanceDigest, ctx, nil
	}
}

// gatherMaintenanceUnits fans out instance snapshots to the online kiosks (or
// the single scoped kiosk) and reduces them to the units in maintenance plus
// the list of kiosks that didn't contribute. Deterministically ordered so the
// digest body is stable across runs.
func gatherMaintenanceUnits(nc *nats.Conn, reg *HeartbeatRegistry, kioskCodeFilter string) ([]notifications.MaintenanceDigestRow, []string) {
	targets := onlineKiosks(reg, heartbeatFreshness)
	if kioskCodeFilter != "" {
		targets = filterToCode(targets, kioskCodeFilter)
	}
	if len(targets) == 0 {
		return nil, nil
	}

	results := fanoutSnapshots(nc, targets, events.InstanceSnapshotCommandSubject)

	var (
		rows    []notifications.MaintenanceDigestRow
		offline []string
	)
	for _, inv := range results {
		if inv.err != nil {
			offline = append(offline, inv.kioskCode)
			continue
		}
		kioskRows, perr := maintenanceRowsForKiosk(inv.kioskCode, inv.rawData)
		if perr != nil {
			offline = append(offline, inv.kioskCode)
			continue
		}
		rows = append(rows, kioskRows...)
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
	sort.Strings(offline)
	return rows, offline
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
