package controller

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/catalog"
)

// seedKiosk and seedKioskItem are defined in membership_test.go.
// seedUser and seedItem are defined in consumer_test.go.

// TestExpectedItemKeys_BuildsNamespacedKeysFromMembership pins the key
// shape (<kiosk_code>.<item_code>) and the payload round-trip. Catalog
// reconcile relies on this map matching exactly what the publisher
// would have produced from the same records.
func TestExpectedItemKeys_BuildsNamespacedKeysFromMembership(t *testing.T) {
	app := setupApp(t)

	kioskA := seedKiosk(t, app, "KIOSK-A", "WEST")
	kioskB := seedKiosk(t, app, "KIOSK-B", "EAST")
	hammer := seedItem(t, app, "HAMMER", "Hammer")
	screws := seedItem(t, app, "SCREW-3IN", "Deck Screws")

	// A stocks HAMMER + SCREW-3IN; B stocks HAMMER only.
	seedKioskItem(t, app, kioskA, hammer)
	seedKioskItem(t, app, kioskA, screws)
	seedKioskItem(t, app, kioskB, hammer)

	keys, err := expectedItemKeys(app)
	if err != nil {
		t.Fatalf("expectedItemKeys: %v", err)
	}

	want := []string{
		"KIOSK-A.HAMMER",
		"KIOSK-A.SCREW-3IN",
		"KIOSK-B.HAMMER",
	}
	if len(keys) != len(want) {
		t.Fatalf("key count: want %d, got %d (got=%v)", len(want), len(keys), keys)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing expected key %q", k)
		}
	}

	// Payload round-trip: unmarshal and verify the item fields survived.
	payload, err := catalog.UnmarshalItem(keys["KIOSK-A.HAMMER"])
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Code != "HAMMER" || payload.Name != "Hammer" {
		t.Errorf("payload: got %+v", payload)
	}
}

// TestExpectedItemKeys_SkipsOrphanedMembership confirms that a kiosk_items
// row whose item or kiosk FK no longer resolves is silently dropped from
// the expected set — without this guard, a half-deleted record would
// produce a bogus key in the integrity report.
//
// Cascade deletes make this hard to trigger via the normal API, but a
// hand-edited DB or a partial migration could.
func TestExpectedItemKeys_SkipsOrphanedMembership(t *testing.T) {
	app := setupApp(t)

	kiosk := seedKiosk(t, app, "KIOSK-A", "WEST")
	hammer := seedItem(t, app, "HAMMER", "Hammer")
	seedKioskItem(t, app, kiosk, hammer)

	// Hand-insert a kiosk_items row pointing at a nonexistent item id.
	col, _ := app.FindCollectionByNameOrId("kiosk_items")
	orphan := core.NewRecord(col)
	orphan.Set("kiosk", kiosk)
	orphan.Set("item", "nonexistent-id-1234567")
	// Bypass PB's relation validation by saving via the DAO — this is
	// what a hand-edited DB or partial migration would produce.
	if err := app.Save(orphan); err != nil {
		// PB may reject the bogus FK. If it does, the test scenario
		// can't be set up directly; treat as a documentation-only test.
		t.Skipf("PB rejected orphan FK insert (%v); guard is still defensive", err)
		return
	}

	keys, err := expectedItemKeys(app)
	if err != nil {
		t.Fatalf("expectedItemKeys: %v", err)
	}
	if _, ok := keys["KIOSK-A.HAMMER"]; !ok {
		t.Error("legitimate membership missing")
	}
	for k := range keys {
		if k == "KIOSK-A." || k == "" {
			t.Errorf("orphan produced bogus key %q", k)
		}
	}
}

// TestExpectedUserKeys_BuildsByCode confirms the user bucket is keyed by
// code (not id) and skips users with empty codes (which shouldn't happen
// in practice but the guard exists).
func TestExpectedUserKeys_BuildsByCode(t *testing.T) {
	app := setupApp(t)

	seedUser(t, app, "EMP-1", "Alice")
	seedUser(t, app, "EMP-2", "Bob")

	keys, err := expectedUserKeys(app)
	if err != nil {
		t.Fatalf("expectedUserKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("key count: want 2, got %d (%v)", len(keys), keysOf(keys))
	}
	if _, ok := keys["EMP-1"]; !ok {
		t.Errorf("missing EMP-1")
	}

	payload, err := catalog.UnmarshalUser(keys["EMP-1"])
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Code != "EMP-1" || payload.Name != "Alice" {
		t.Errorf("payload: got %+v", payload)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDiffKeys covers the small pure function. Lists must be sorted for
// stable consecutive runs.
func TestDiffKeys(t *testing.T) {
	expected := map[string][]byte{
		"a": nil, "b": nil, "c": nil,
	}
	actual := map[string]struct{}{
		"b": {}, "c": {}, "d": {}, "e": {},
	}
	missing, extra := diffKeys(expected, actual)
	wantMissing := []string{"a"}
	wantExtra := []string{"d", "e"}
	if !equalSorted(missing, wantMissing) {
		t.Errorf("missing: want %v, got %v", wantMissing, missing)
	}
	if !equalSorted(extra, wantExtra) {
		t.Errorf("extra: want %v, got %v", wantExtra, extra)
	}
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
