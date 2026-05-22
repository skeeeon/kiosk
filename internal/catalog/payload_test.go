package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestItemPayload_RoundTrip(t *testing.T) {
	in := ItemPayload{
		Code:         "WRENCH-10",
		RFIDEPC:      "E2806894...",
		Name:         "10mm Wrench",
		Type:         "tool",
		Unit:         "ea",
		TrackingMode: "quantity",
		Category:     "hand-tools",
		Active:       true,
		Notes:        "lives on pegboard 3",
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
		"collectionId", "collectionName",
	} {
		if _, ok := generic[banned]; ok {
			t.Errorf("serialized payload contains banned field %q (full payload: %s)", banned, data)
		}
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
		Code:   "BADGE-007",
		Name:   "Jane Doe",
		Email:  "jane@example.com",
		Role:   "worker",
		Active: true,
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
