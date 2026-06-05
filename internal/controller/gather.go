package controller

import (
	"encoding/json"
	"sort"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/ledger"
	"github.com/skeeeon/kiosk/internal/notifications"
)

// targetKiosks resolves the fan-out universe for a fleet read and splits it by
// liveness. An explicit kioskCodeFilter scopes to one kiosk; empty means the
// whole registry (the kiosks collection — the authoritative fleet list, which
// includes kiosks not currently beating). online == heartbeat-fresh.
func (h *Handlers) targetKiosks(reg *HeartbeatRegistry, kioskCodeFilter string) (online, offline []string, err error) {
	var codes []string
	if kioskCodeFilter != "" {
		codes = []string{kioskCodeFilter}
	} else {
		recs, e := h.App.FindRecordsByFilter("kiosks", "", "kiosk_code", 0, 0)
		if e != nil {
			return nil, nil, e
		}
		for _, r := range recs {
			if c := r.GetString("kiosk_code"); c != "" {
				codes = append(codes, c)
			}
		}
	}
	for _, c := range codes {
		if reg.IsLikelyOnline(c, heartbeatFreshness) {
			online = append(online, c)
		} else {
			offline = append(offline, c)
		}
	}
	return online, offline, nil
}

// gatherOpenCheckouts computes the fleet (or single-kiosk) currently-out set
// NATS-first: online kiosks answer from their own open_checkouts via the
// checkout.snapshot command; offline kiosks fall back to replaying the
// controller's projected ledger (last-known). A kiosk we believe online that
// fails to answer is reported Unavailable rather than silently replayed — we
// don't serve stale data for a kiosk that should have been reachable. The
// provenance breakdown lets callers (the digest) flag partial results so an
// offline kiosk is never silently dropped.
func (h *Handlers) gatherOpenCheckouts(nc *nats.Conn, reg *HeartbeatRegistry, kioskCodeFilter string) ([]ledger.OpenCheckoutDTO, notifications.KioskProvenance, error) {
	online, offline, err := h.targetKiosks(reg, kioskCodeFilter)
	if err != nil {
		return nil, notifications.KioskProvenance{}, err
	}
	out := make([]ledger.OpenCheckoutDTO, 0)
	var prov notifications.KioskProvenance

	if len(online) > 0 {
		for _, inv := range fanoutSnapshots(nc, online, events.CheckoutSnapshotCommandSubject) {
			if inv.err != nil {
				prov.UnavailableKiosks = append(prov.UnavailableKiosks, inv.kioskCode)
				continue
			}
			var reply struct {
				OpenCheckouts []ledger.OpenCheckoutDTO `json:"open_checkouts"`
			}
			if jerr := json.Unmarshal(inv.rawData, &reply); jerr != nil {
				prov.UnavailableKiosks = append(prov.UnavailableKiosks, inv.kioskCode)
				continue
			}
			out = append(out, reply.OpenCheckouts...)
			prov.LiveKiosks = append(prov.LiveKiosks, inv.kioskCode)
		}
	}

	for _, code := range offline {
		rows, rerr := ledger.ReplayOpenRows(h.App, code)
		if rerr != nil {
			prov.UnavailableKiosks = append(prov.UnavailableKiosks, code)
			continue
		}
		dtos, herr := ledger.Hydrate(h.App, rows)
		if herr != nil {
			prov.UnavailableKiosks = append(prov.UnavailableKiosks, code)
			continue
		}
		out = append(out, dtos...)
		prov.LastKnownKiosks = append(prov.LastKnownKiosks, code)
	}

	sortProvenance(&prov)
	return out, prov, nil
}

// replayInstanceStatuses reconstructs each instance's current status for one
// kiosk from the projected instance_lifecycle_audit (latest transition per
// instance wins) and returns the units currently in maintenance as digest
// rows. This is the offline fallback for the maintenance digest — the
// controller's analogue of the kiosk's live instance.snapshot. The audit
// doesn't carry serial, so offline rows omit it.
func replayInstanceStatuses(app core.App, kioskCode string) ([]notifications.MaintenanceDigestRow, error) {
	recs, err := app.FindRecordsByFilter("instance_lifecycle_audit",
		"kiosk_code = {:kc}", "occurred_at", 0, 0, dbx.Params{"kc": kioskCode})
	if err != nil {
		return nil, err
	}
	type latest struct{ status, instanceCode, itemCode, itemName string }
	byInstance := make(map[string]latest, len(recs))
	for _, r := range recs { // ascending occurred_at → last write wins = current status
		byInstance[r.GetString("instance_id")] = latest{
			status:       r.GetString("new_status"),
			instanceCode: r.GetString("instance_code"),
			itemCode:     r.GetString("item_code"),
			itemName:     r.GetString("item_name"),
		}
	}
	out := make([]notifications.MaintenanceDigestRow, 0)
	for _, l := range byInstance {
		if l.status != "maintenance" {
			continue
		}
		out = append(out, notifications.MaintenanceDigestRow{
			ItemCode:     l.itemCode,
			ItemName:     l.itemName,
			InstanceCode: l.instanceCode,
			KioskCode:    kioskCode,
		})
	}
	return out, nil
}

func sortProvenance(p *notifications.KioskProvenance) {
	sort.Strings(p.LiveKiosks)
	sort.Strings(p.LastKnownKiosks)
	sort.Strings(p.UnavailableKiosks)
}
