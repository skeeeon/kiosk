package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestItemPayload_RoundTrip(t *testing.T) {
	in := ItemPayload{
		Code:         "WRENCH-10",
		Name:         "10mm Wrench",
		Type:         "tool",
		Unit:         "ea",
		TrackingMode: "quantity",
		Category:     "hand-tools",
		Active:       true,
		Notes:        "lives on pegboard 3",
		// Non-zero on purpose: this test compares structs, so a field left
		// at its zero value here round-trips vacuously and proves nothing.
		RequiresMaintenanceOnReturn: true,
	}
	data, err := MarshalItem(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnmarshalItem(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestItemPayload_ExcludesSystemFields(t *testing.T) {
	in := ItemPayload{Code: "X", Name: "x", Type: "tool", TrackingMode: "quantity", Active: true}
	data, err := MarshalItem(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Decode into a generic map and assert we don't accidentally serialize
	// fields that shouldn't cross the wire. If someone adds an exported
	// field to ItemPayload, this test forces them to think about whether
	// it's appropriate to sync.
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("decode generic: %v", err)
	}
	for _, banned := range []string{
		"id", "created", "updated",
		"quantity_on_hand", "reorder_threshold",
		"rfid_epc", "serial",
		"collectionId", "collectionName",
	} {
		if _, ok := generic[banned]; ok {
			t.Errorf("serialized payload contains banned field %q (full payload: %s)", banned, data)
		}
	}
}

// TestItemPayload_CarriesPerSKUPolicy is the inverse of the banned-field
// test above, and it is the one that would have caught the bug it was
// written for: requires_maintenance_on_return existed on the items
// collection and was read by commit.Commit, but never reached the wire, so
// the flag did nothing at any managed kiosk in the fleet. An absent field
// decodes to its zero value in silence — nothing errors, nothing logs, the
// feature is simply off.
//
// Keep this list in step with the fields kiosk-side logic reads. A field
// here must also be set by itemPayloadFrom on the controller and by
// Watcher.upsertItem on the kiosk; this test only proves the middle hop.
func TestItemPayload_CarriesPerSKUPolicy(t *testing.T) {
	// Every field non-zero, so `omitempty` can't hide an omission.
	in := ItemPayload{
		Code:                        "X",
		Name:                        "x",
		Type:                        "tool",
		Unit:                        "ea",
		TrackingMode:                "serialized",
		Category:                    "power-tools",
		Active:                      true,
		Notes:                       "n",
		RequiresMaintenanceOnReturn: true,
	}
	data, err := MarshalItem(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("decode generic: %v", err)
	}
	for _, required := range []string{
		"code", "name", "type", "unit", "tracking_mode",
		"category", "active", "notes",
		"requires_maintenance_on_return",
	} {
		if _, ok := generic[required]; !ok {
			t.Errorf("serialized payload is missing %q — a managed kiosk would silently see its zero value (full payload: %s)", required, data)
		}
	}
}

// TestItemPayload_MaintenanceFlagSurvivesFalse pins the other direction:
// clearing the flag on the controller must clear it at the kiosk. Without
// an explicit `false` on the wire, a kiosk that already projected `true`
// would keep routing that SKU to the bench forever.
func TestItemPayload_MaintenanceFlagSurvivesFalse(t *testing.T) {
	data, err := MarshalItem(ItemPayload{Code: "X", Name: "x", Type: "tool", TrackingMode: "serialized"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("decode generic: %v", err)
	}
	got, ok := generic["requires_maintenance_on_return"]
	if !ok {
		t.Fatalf("false must be carried explicitly, not omitted (full payload: %s)", data)
	}
	if got != false {
		t.Fatalf("requires_maintenance_on_return = %v, want false", got)
	}
}

func TestItemPayload_RejectsEmptyCode(t *testing.T) {
	_, err := MarshalItem(ItemPayload{Name: "no code"})
	if err == nil {
		t.Fatal("expected error when code is empty")
	}
	if !strings.Contains(err.Error(), "code") {
		t.Errorf("error %q does not mention 'code'", err)
	}

	_, err = UnmarshalItem([]byte(`{"name":"no code"}`))
	if err == nil {
		t.Fatal("expected error when decoded payload has no code")
	}
}

func TestUserPayload_RoundTrip(t *testing.T) {
	in := UserPayload{
		Code:      "BADGE-007",
		Name:      "Jane Doe",
		Email:     "jane@example.com",
		Role:      "worker",
		GroupCode: "electrical",
		Active:    true,
	}
	data, err := MarshalUser(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnmarshalUser(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestUserPayload_ExcludesAuthFields(t *testing.T) {
	in := UserPayload{Code: "X", Name: "x", Role: "worker", Active: true}
	data, err := MarshalUser(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("decode generic: %v", err)
	}
	for _, banned := range []string{"password", "passwordHash", "tokenKey", "verified", "emailVisibility"} {
		if _, ok := generic[banned]; ok {
			t.Errorf("serialized user payload contains banned field %q (full payload: %s)", banned, data)
		}
	}
}

func TestUserPayload_RejectsEmptyCode(t *testing.T) {
	if _, err := MarshalUser(UserPayload{Name: "no code"}); err == nil {
		t.Fatal("expected error when code is empty")
	}
	if _, err := UnmarshalUser([]byte(`{"name":"no code"}`)); err == nil {
		t.Fatal("expected error when decoded payload has no code")
	}
}

func TestGroupPayload_RoundTrip(t *testing.T) {
	in := GroupPayload{
		Code:         "ACME",
		Name:         "Acme Subcontracting",
		ContactEmail: "foreman@acme.example",
		ContactPhone: "+1-555-0100",
		Notes:        "NET-30",
		Active:       true,
	}
	data, err := MarshalGroup(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnmarshalGroup(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestGroupPayload_ExcludesSystemFields(t *testing.T) {
	in := GroupPayload{Code: "X", Name: "x", Active: true}
	data, err := MarshalGroup(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("decode generic: %v", err)
	}
	for _, banned := range []string{"id", "created", "updated", "collectionId", "collectionName"} {
		if _, ok := generic[banned]; ok {
			t.Errorf("serialized group payload contains banned field %q (full payload: %s)", banned, data)
		}
	}
}

func TestGroupPayload_RejectsEmptyCode(t *testing.T) {
	if _, err := MarshalGroup(GroupPayload{Name: "no code"}); err == nil {
		t.Fatal("expected error when code is empty")
	}
	if _, err := UnmarshalGroup([]byte(`{"name":"no code"}`)); err == nil {
		t.Fatal("expected error when decoded payload has no code")
	}
}
