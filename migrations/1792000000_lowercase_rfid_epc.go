package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

// Backfills item_instances.rfid_epc to lower-case (trimmed) hex.
//
// The LLRP reader emits lower-case hex (internal/rfid.epcFromTag), and the
// scan resolver + enclosure_diff matcher compare EPCs exactly. A row stored
// with any upper-case hex therefore never matches an observed tag — the tag
// is silently dropped in counter_scan, or treated as "left the enclosure" in
// enclosure_diff (synthesizing a spurious checkout line and missing a real
// return). New writes are normalized by the model hooks in
// internal/instances/hooks.go; this one-shot pass fixes any rows written
// before that landed (and any introduced by a manual DB edit).
//
// Shared migration: the controller's item_instances collection is always
// empty (instances are kiosk-local), so this is a no-op there.

func init() {
	m.Register(lowercaseRFIDEPCUp, lowercaseRFIDEPCDown)
}

func lowercaseRFIDEPCUp(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("item_instances"); err != nil {
		// Collection not present yet (out-of-order apply shouldn't happen given
		// timestamp ordering) — nothing to backfill.
		return nil
	}
	// Raw UPDATE rather than per-record Save: avoids re-triggering the
	// normalization hook and touches only the rows that actually differ.
	if _, err := app.DB().NewQuery(
		"UPDATE item_instances SET rfid_epc = lower(trim(rfid_epc)) " +
			"WHERE rfid_epc IS NOT NULL AND rfid_epc != '' " +
			"AND rfid_epc != lower(trim(rfid_epc))",
	).Execute(); err != nil {
		return fmt.Errorf("backfill lower-case rfid_epc: %w", err)
	}
	return nil
}

func lowercaseRFIDEPCDown(app core.App) error {
	// Irreversible: the original casing isn't recoverable. No-op so a down
	// migration doesn't fail.
	return nil
}
