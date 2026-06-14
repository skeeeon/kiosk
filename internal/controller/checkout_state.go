package controller

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/skeeeon/kiosk/internal/ledger"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// Open-checkouts state broadcast. Sibling of the punch_state writer
// (timeclock_consumer.go): after the controller projects a line that changes a
// user's open set, recompute that user's fleet-wide outstanding rows and
// publish them to the open_checkouts_state bucket. Managed kiosks + the virtual
// terminal watch it for the cross-kiosk clock-out gate. Advisory — failures log
// and never block the ack; the replica self-heals on the next line.

// refreshOpenCheckoutsState recomputes one user's fleet-wide open checkouts and
// writes them to the open_checkouts_state bucket (key = user_code). It ALWAYS
// writes, including the empty case, so a return clears the gate rather than
// leaving a stale row behind. userCode "" or an absent KV bucket is a no-op.
func (a *Aggregator) refreshOpenCheckoutsState(ctx context.Context, userCode string) {
	if a.checkoutKV == nil || userCode == "" {
		return
	}
	user, err := a.findUserByCode(userCode)
	if err != nil {
		slog.Warn("controller.aggregator.open_checkouts_state.user_lookup_failed",
			"user_code", userCode, "error", err)
		return
	}
	if user == nil {
		// Catalog hasn't caught up — skip; the next line for this user re-runs
		// the recompute. Don't fail the ack over the advisory replica.
		return
	}

	payload, err := a.buildOpenCheckoutsPayload(user.Id, userCode)
	if err != nil {
		slog.Warn("controller.aggregator.open_checkouts_state.build_failed",
			"user_code", userCode, "error", err)
		return
	}

	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("controller.aggregator.open_checkouts_state.marshal_failed",
			"user_code", userCode, "error", err)
		return
	}
	if _, err := a.checkoutKV.Put(ctx, userCode, data); err != nil {
		slog.Warn("controller.aggregator.open_checkouts_state.put_failed",
			"user_code", userCode, "error", err)
	}
}

// buildOpenCheckoutsPayload recomputes a user's fleet-wide open checkouts from
// the projected ledger and maps them onto the flat replica payload. Pure-DB
// (no KV), so it's the testable seam: the empty case yields an empty Rows
// slice (which clears the gate downstream).
func (a *Aggregator) buildOpenCheckoutsPayload(userID, userCode string) (timeclock.OpenCheckoutsStatePayload, error) {
	payload := timeclock.OpenCheckoutsStatePayload{UserCode: userCode}
	rows, err := ledger.ReplayOpenRowsForUser(a.app, userID)
	if err != nil {
		return payload, err
	}
	dtos, err := ledger.Hydrate(a.app, rows)
	if err != nil {
		return payload, err
	}
	payload.Rows = make([]timeclock.OpenCheckoutRow, 0, len(dtos))
	for _, d := range dtos {
		row := timeclock.OpenCheckoutRow{Serial: d.Serial, KioskCode: d.KioskCode}
		if d.Expand.Item != nil {
			row.ItemCode = d.Expand.Item.Code
			row.ItemName = d.Expand.Item.Name
		}
		payload.Rows = append(payload.Rows, row)
	}
	return payload, nil
}

// refreshOpenCheckoutsForLine recomputes every user a projected line could have
// changed: the transacting user, and (for foreman returns / admin closes) the
// holder named as original_checkout_user. Deduped so a self-line recomputes
// once. Triggered on LINE projection — not transaction.complete — because lines
// always project after their parent, and the last line's recompute yields the
// transaction's final open set.
func (a *Aggregator) refreshOpenCheckoutsForLine(ctx context.Context, p EventPayload) {
	seen := map[string]struct{}{}
	for _, code := range []string{p.UserCode, p.OriginalCheckoutUserCode} {
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		a.refreshOpenCheckoutsState(ctx, code)
	}
}
