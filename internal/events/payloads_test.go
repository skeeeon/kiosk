package events

import (
	"sort"
	"testing"
	"time"
)

// TestBuildTransactionCompletePayload_KeyShape pins the wire-format keys
// for the transaction.complete event. The controller's aggregator
// (internal/controller/consumer.go's EventPayload) JSON-decodes against
// these exact field names, so a typo or rename here would silently break
// the projection — JSON decode is best-effort and missing fields don't
// error.
//
// If you add a field to TransactionCompleteInput, also extend the wanted
// list below.
func TestBuildTransactionCompletePayload_KeyShape(t *testing.T) {
	in := TransactionCompleteInput{
		TransactionID: "tx-1",
		KioskCode:     "KIOSK-A",
		LocationCode:  "WEST",
		UserID:        "user-1",
		UserCode:      "EMP-1",
		UserName:      "Alice",
		UserGroup:     "electrical",
		StartedAt:     time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		CompletedAt:   time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC),
		LinesCount:    3,
		CheckedOut:    1,
		Returned:      1,
		Consumed:      1,
	}
	got := BuildTransactionCompletePayload(in)

	want := []string{
		"transaction_id", "kiosk_code", "location_code",
		"user_id", "user_code", "user_name", "user_group",
		"started_at", "completed_at",
		"lines_count", "checked_out", "returned", "consumed",
	}
	assertKeysEqual(t, "transaction.complete", got, want)

	// Spot-check a few values to confirm the input→output mapping.
	if got["transaction_id"] != "tx-1" {
		t.Errorf("transaction_id: got %v", got["transaction_id"])
	}
	if got["lines_count"] != 3 {
		t.Errorf("lines_count: got %v", got["lines_count"])
	}
	if got["completed_at"] != in.CompletedAt {
		t.Errorf("completed_at: got %v want %v", got["completed_at"], in.CompletedAt)
	}
}

// TestBuildItemActionPayload_KeyShape pins the wire-format keys for the
// item.{action} event.
func TestBuildItemActionPayload_KeyShape(t *testing.T) {
	in := ItemActionInput{
		TransactionID: "tx-1",
		LineID:        "line-1",
		KioskCode:     "KIOSK-A",
		LocationCode:  "WEST",
		UserID:        "user-1",
		UserCode:      "EMP-1",
		UserGroup:     "electrical",
		ItemID:        "item-1",
		ItemCode:      "HAMMER",
		ItemName:      "Hammer",
		Action:        "checkout",
		Qty:           2,
		Serial:        "SN-1",
		Uncorrelated:  false,
		CompletedAt:   time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC),
	}
	got := BuildItemActionPayload(in)

	want := []string{
		"transaction_id", "line_id",
		"kiosk_code", "location_code",
		"user_id", "user_code", "user_group",
		"item_id", "item_code", "item_name",
		"action", "qty", "serial", "uncorrelated",
		"original_checkout_user_code", "item_instance_id",
		"completed_at",
	}
	assertKeysEqual(t, "item.action", got, want)

	if got["item_code"] != "HAMMER" {
		t.Errorf("item_code: got %v", got["item_code"])
	}
	if got["qty"] != 2 {
		t.Errorf("qty: got %v", got["qty"])
	}
	if got["uncorrelated"] != false {
		t.Errorf("uncorrelated: got %v", got["uncorrelated"])
	}
}

// assertKeysEqual fails if the payload's key set doesn't match `want`
// exactly. Reports missing + extra separately so a regression's diagnosis
// is one line.
func assertKeysEqual(t *testing.T, label string, got map[string]any, want []string) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	wantSet := make(map[string]struct{}, len(wantSorted))
	for _, k := range wantSorted {
		wantSet[k] = struct{}{}
	}
	gotSet := make(map[string]struct{}, len(gotKeys))
	for _, k := range gotKeys {
		gotSet[k] = struct{}{}
	}
	var missing, extra []string
	for _, k := range wantSorted {
		if _, ok := gotSet[k]; !ok {
			missing = append(missing, k)
		}
	}
	for _, k := range gotKeys {
		if _, ok := wantSet[k]; !ok {
			extra = append(extra, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s: missing keys %v", label, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s: extra keys %v", label, extra)
	}
}
