package controller

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/timeclock"
)

// Timeclock punch projection + punch_state KV broadcast. Sibling of the
// inventory_audit / instance_lifecycle_audit projections: idempotent on a
// unique-when-non-empty source id, ack on terminal cases, retry on DB
// hiccups. The KV write rides AFTER a successful projection and is
// advisory — its failure never blocks the ledger.

// handleTimeclockPunch is the JetStream-side dispatcher for timeclock.punch.
func (a *Aggregator) handleTimeclockPunch(ctx context.Context, msg jetstream.Msg, p EventPayload) {
	switch a.ProjectTimePunch(p) {
	case projectAck:
		a.writePunchState(ctx, p)
		_ = msg.Ack()
	case projectRetry:
		_ = msg.Nak()
	}
}

// ProjectTimePunch upserts one fleet punch into the controller's own
// time_punches collection. Idempotent via the unique source_punch_id index —
// JetStream redelivery and timeclock.republish overlap are no-ops.
func (a *Aggregator) ProjectTimePunch(p EventPayload) projectOutcome {
	if p.PunchID == "" {
		slog.Warn("controller.aggregator.timeclock_punch.missing_punch_id",
			"kiosk_code", p.KioskCode, "user_code", p.UserCode)
		return projectAck
	}

	existing, err := a.findTimePunchBySourceID(p.PunchID)
	if err != nil {
		slog.Warn("controller.aggregator.time_punches.lookup_failed", "error", err)
		return projectRetry
	}
	if existing != nil {
		return projectAck
	}

	user, err := a.findUserByCode(p.UserCode)
	if err != nil {
		slog.Warn("controller.aggregator.time_punches.user_lookup_failed",
			"user_code", p.UserCode, "error", err)
		return projectRetry
	}
	if user == nil {
		// Catalog hasn't caught up. Ack — same posture as ProjectTransaction.
		slog.Warn("controller.aggregator.timeclock_punch.unknown_user",
			"user_code", p.UserCode, "kiosk_code", p.KioskCode)
		return projectAck
	}

	col, err := a.app.FindCollectionByNameOrId(timeclock.Collection)
	if err != nil {
		slog.Warn("controller.aggregator.time_punches.collection_missing", "error", err)
		return projectRetry
	}

	rec := core.NewRecord(col)
	rec.Set("user", user.Id)
	rec.Set("user_code", p.UserCode)
	rec.Set("direction", p.Direction)
	rec.Set("occurred_at", p.OccurredAt)
	rec.Set("source", p.Source)
	switch p.Source {
	case timeclock.SourceForeman:
		// Best-effort FK resolve against the org-wide synced users; an
		// unknown recorder code drops the FK rather than failing projection.
		if u, uerr := a.findUserByCode(p.RecordedByUserCode); uerr == nil && u != nil {
			rec.Set("recorded_by_user", u.Id)
		}
	case timeclock.SourceAdmin:
		// The kiosk-local admins record id can't resolve here — keep it as
		// opaque text so the fleet CSV still carries a traceable actor.
		rec.Set("source_actor", p.AdminID)
	case timeclock.SourceControllerAdmin:
		rec.Set("controller_admin_id", p.ControllerAdminID)
	}
	if p.Reason != "" {
		rec.Set("reason", p.Reason)
	}
	if p.Force {
		rec.Set("force", true)
	}
	if p.JobCode != "" {
		rec.Set("job_code", p.JobCode)
	}
	rec.Set("kiosk_code", p.KioskCode)
	rec.Set("location_code", p.LocationCode)
	rec.Set("command_id", p.CommandID)
	rec.Set("source_punch_id", p.PunchID)
	if err := a.app.Save(rec); err != nil {
		if isUniqueViolation(err) {
			return projectAck
		}
		slog.Warn("controller.aggregator.time_punches.save_failed", "error", err)
		return projectRetry
	}
	return projectAck
}

func (a *Aggregator) findTimePunchBySourceID(sourcePunchID string) (*core.Record, error) {
	rec, err := a.app.FindFirstRecordByFilter(timeclock.Collection,
		"source_punch_id = {:id}",
		dbx.Params{"id": sourcePunchID})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

// shouldReplacePunchState is the monotonic guard for the punch_state bucket:
// only a strictly newer occurred_at overwrites, so redelivery and republish
// of old punches can never drag a user's broadcast state backwards. Pure
// function — tested without JetStream.
func shouldReplacePunchState(existing, incoming timeclock.PunchStatePayload) bool {
	return incoming.OccurredAt.After(existing.OccurredAt)
}

// writePunchState broadcasts the user's latest punch state to the
// punch_state bucket (key = user_code). Read-compare-write rather than CAS:
// the single durable consumer is the only writer, so the monotonic compare
// can't race itself. Failures log and the event still acks — the replica is
// advisory by design.
func (a *Aggregator) writePunchState(ctx context.Context, p EventPayload) {
	if a.punchKV == nil || p.UserCode == "" || p.OccurredAt.IsZero() {
		return
	}
	incoming := timeclock.PunchStatePayload{
		UserCode:      p.UserCode,
		ClockedIn:     p.Direction == timeclock.DirectionIn,
		OccurredAt:    p.OccurredAt,
		SourcePunchID: p.PunchID,
	}
	if entry, err := a.punchKV.Get(ctx, p.UserCode); err == nil {
		var cur timeclock.PunchStatePayload
		if json.Unmarshal(entry.Value(), &cur) == nil && !shouldReplacePunchState(cur, incoming) {
			return
		}
	}
	data, err := json.Marshal(incoming)
	if err != nil {
		slog.Warn("controller.aggregator.punch_state.marshal_failed", "error", err)
		return
	}
	if _, err := a.punchKV.Put(ctx, p.UserCode, data); err != nil {
		slog.Warn("controller.aggregator.punch_state.put_failed",
			"user_code", p.UserCode, "error", err)
	}
}
