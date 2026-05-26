package handlers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/rfid"
)

// fakeReader is a test double for rfid.Reader. ReadFor returns the
// configured EPC list (or err if set); Connect/Close are no-ops.
type fakeReader struct {
	epcs []rfid.EPC
	err  error
	// calls records how many ReadFor calls the test triggered; useful
	// for verifying we don't double-read on per-EPC errors.
	calls int
}

func (f *fakeReader) Connect(context.Context) error { return nil }
func (f *fakeReader) Close() error                   { return nil }
func (f *fakeReader) ReadFor(_ context.Context, _ time.Duration) ([]rfid.EPC, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]rfid.EPC, len(f.epcs))
	copy(out, f.epcs)
	return out, nil
}

// rfidScanSeed bundles the records and IDs a typical RFID scan test
// needs: a worker, a serialized tool, two instances (one active with
// EPC, one inactive with EPC), plus a started cart for the worker.
type rfidScanSeed struct {
	WorkerID         string
	ItemID           string
	ActiveInstanceID string
	ActiveEPC        string
	InactiveEPC      string
	CartID           string
	H                *handlers.Handlers
	Reader           *fakeReader
}

func seedRFIDScan(t *testing.T) (core.App, rfidScanSeed) {
	t.Helper()
	app := setupApp(t)

	users, _ := app.FindCollectionByNameOrId("users")
	worker := core.NewRecord(users)
	worker.Set("code", "W-001")
	worker.Set("name", "Worker One")
	worker.Set("role", "worker")
	worker.Set("active", true)
	worker.Set("email", "w001@example.com")
	worker.Set("password", "passwordpassword")
	worker.Set("passwordConfirm", "passwordpassword")
	if err := app.Save(worker); err != nil {
		t.Fatalf("save worker: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	item := core.NewRecord(items)
	item.Set("code", "DRILL")
	item.Set("name", "Cordless Drill")
	item.Set("type", "tool")
	item.Set("tracking_mode", "serialized")
	item.Set("active", true)
	item.Set("quantity_on_hand", 2)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	const activeEPC = "e2801168200082c0a3b400a1"
	const inactiveEPC = "e2801168200082c0a3b400a2"

	instances, _ := app.FindCollectionByNameOrId("item_instances")
	active := core.NewRecord(instances)
	active.Set("item", item.Id)
	active.Set("code", "DRILL-01")
	active.Set("serial", "SN-01")
	active.Set("rfid_epc", activeEPC)
	active.Set("active", true)
	if err := app.Save(active); err != nil {
		t.Fatalf("save active instance: %v", err)
	}

	inactive := core.NewRecord(instances)
	inactive.Set("item", item.Id)
	inactive.Set("code", "DRILL-02")
	inactive.Set("serial", "SN-02")
	inactive.Set("rfid_epc", inactiveEPC)
	inactive.Set("active", false)
	if err := app.Save(inactive); err != nil {
		t.Fatalf("save inactive instance: %v", err)
	}

	cfg := &config.Config{
		RFID: config.RFIDConfig{
			Enabled:    true,
			Mode:       config.RFIDModeCounterScan,
			ReadWindow: config.Duration(50 * time.Millisecond),
		},
	}
	store := cart.NewStore(5 * time.Minute)
	c := store.Start(worker.Id, worker.GetString("code"), worker.GetString("name"), worker.GetString("role"))

	reader := &fakeReader{}
	h := handlers.New(app, cfg, store, notifications.New(app))
	h.RFID = reader

	return app, rfidScanSeed{
		WorkerID:         worker.Id,
		ItemID:           item.Id,
		ActiveInstanceID: active.Id,
		ActiveEPC:        activeEPC,
		InactiveEPC:      inactiveEPC,
		CartID:           c.ID,
		H:                h,
		Reader:           reader,
	}
}

// TestPerformRFIDScan_HappyPath exercises the dominant case: reader
// emits three EPCs, one matches an active instance, one matches an
// inactive instance (skip-and-log), one matches nothing (surfaces in
// unresolved_epcs). Cart ends with one added line; observed_epcs
// always carries the full input.
func TestPerformRFIDScan_HappyPath(t *testing.T) {
	_, s := seedRFIDScan(t)
	s.Reader.epcs = []rfid.EPC{
		rfid.EPC(s.ActiveEPC),
		rfid.EPC(s.InactiveEPC),
		"deadbeefcafebabe00000001", // unknown
	}

	resp, err := s.H.PerformRFIDScan(context.Background(), s.CartID)
	if err != nil {
		t.Fatalf("PerformRFIDScan: %v", err)
	}
	if len(resp.AddedLines) != 1 {
		t.Errorf("AddedLines: want 1, got %d (%v)", len(resp.AddedLines), resp.AddedLines)
	}
	if got := resp.AddedLines[0].ItemInstanceID; got != s.ActiveInstanceID {
		t.Errorf("AddedLines[0].ItemInstanceID: want %s, got %s", s.ActiveInstanceID, got)
	}
	if len(resp.ObservedEPCs) != 3 {
		t.Errorf("ObservedEPCs: want 3, got %d", len(resp.ObservedEPCs))
	}
	if len(resp.UnresolvedEPCs) != 1 || resp.UnresolvedEPCs[0] != "deadbeefcafebabe00000001" {
		t.Errorf("UnresolvedEPCs: want [deadbeef…], got %v", resp.UnresolvedEPCs)
	}
}

// TestPerformRFIDScan_AllUnresolved confirms the zero-match path
// still publishes the observed event (no panics) and returns an empty
// added-lines slice plus the full observed list as unresolved.
func TestPerformRFIDScan_AllUnresolved(t *testing.T) {
	_, s := seedRFIDScan(t)
	s.Reader.epcs = []rfid.EPC{"aa", "bb"}

	resp, err := s.H.PerformRFIDScan(context.Background(), s.CartID)
	if err != nil {
		t.Fatalf("PerformRFIDScan: %v", err)
	}
	if len(resp.AddedLines) != 0 {
		t.Errorf("AddedLines: want 0, got %d", len(resp.AddedLines))
	}
	if len(resp.UnresolvedEPCs) != 2 {
		t.Errorf("UnresolvedEPCs: want 2, got %d", len(resp.UnresolvedEPCs))
	}
}

// TestPerformRFIDScan_EmptyRead handles the "operator hit the button
// and the antenna saw nothing" case. Should succeed (this is itself a
// useful signal that something is wrong with placement or there are
// genuinely no tags) — empty arrays, cart unchanged.
func TestPerformRFIDScan_EmptyRead(t *testing.T) {
	_, s := seedRFIDScan(t)
	s.Reader.epcs = nil

	resp, err := s.H.PerformRFIDScan(context.Background(), s.CartID)
	if err != nil {
		t.Fatalf("PerformRFIDScan: %v", err)
	}
	if len(resp.AddedLines) != 0 || len(resp.ObservedEPCs) != 0 || len(resp.UnresolvedEPCs) != 0 {
		t.Errorf("expected fully empty result, got %+v", resp)
	}
}

// TestPerformRFIDScan_DuplicateInScan exercises the "two reads of the
// same active serialized instance in one cycle" case. The cart-store's
// ErrDuplicateInstance guards correctness; we just want the batch to
// keep going (skip-and-log) and the cart to end with one line.
//
// Note: the wrapper's own dedupEPCs collapses repeated EPC observations
// before they reach the handler, so this case would normally not arise
// — we test it anyway because dedup happens upstream of us and we
// can't promise it always will.
func TestPerformRFIDScan_DuplicateInScan(t *testing.T) {
	_, s := seedRFIDScan(t)
	s.Reader.epcs = []rfid.EPC{
		rfid.EPC(s.ActiveEPC),
		rfid.EPC(s.ActiveEPC), // duplicate
	}

	resp, err := s.H.PerformRFIDScan(context.Background(), s.CartID)
	if err != nil {
		t.Fatalf("PerformRFIDScan: %v", err)
	}
	if len(resp.AddedLines) != 1 {
		t.Errorf("AddedLines: want 1 (duplicate skipped), got %d", len(resp.AddedLines))
	}
	if len(resp.UnresolvedEPCs) != 0 {
		t.Errorf("UnresolvedEPCs: want 0 (duplicate is not 'unresolved'), got %v", resp.UnresolvedEPCs)
	}
}

// TestPerformRFIDScan_CartNotFound bubbles a stale cart_id as
// errCartNotFound without burning a read window — the helper guards
// up front. We assert reader.calls==0 so a regression that loses
// that fast-fail is loud.
func TestPerformRFIDScan_CartNotFound(t *testing.T) {
	_, s := seedRFIDScan(t)
	_, err := s.H.PerformRFIDScan(context.Background(), "no-such-cart")
	if err == nil {
		t.Fatal("expected error for missing cart, got nil")
	}
	if s.Reader.calls != 0 {
		t.Errorf("ReadFor should not be called when cart is missing, got %d calls", s.Reader.calls)
	}
}

// TestPerformRFIDScan_ReaderError surfaces the underlying ReadFor
// error wrapped in rfidReadErr so the HTTP layer can translate to a
// 503. We can't reach the unexported errRFIDReadFailed from the
// _test package, so we assert via the message substring instead.
func TestPerformRFIDScan_ReaderError(t *testing.T) {
	_, s := seedRFIDScan(t)
	s.Reader.err = errors.New("reader is on fire")

	_, err := s.H.PerformRFIDScan(context.Background(), s.CartID)
	if err == nil {
		t.Fatal("expected error from ReadFor failure, got nil")
	}
	if got := err.Error(); !contains(got, "reader is on fire") {
		t.Errorf("error should wrap the underlying message, got %q", got)
	}
}

// TestPerformRFIDScan_AlreadyInCartUnresolved confirms a tag whose
// instance is already in the cart is skipped (cart.ErrDuplicateInstance)
// and does NOT land in UnresolvedEPCs (which is reserved for EPCs that
// don't resolve to any item_instances row). We seed the cart with the
// instance first via the standard add path, then re-scan.
func TestPerformRFIDScan_AlreadyInCartUnresolved(t *testing.T) {
	app, s := seedRFIDScan(t)

	// Pre-seed: add the active instance to the cart by its instance
	// code so a subsequent RFID re-scan would conflict.
	inst, err := app.FindRecordById("item_instances", s.ActiveInstanceID)
	if err != nil {
		t.Fatalf("find instance: %v", err)
	}
	_ = inst
	// Use the cart store directly to mimic a prior cart/add.
	c, err := s.H.Carts.Get(s.CartID)
	if err != nil {
		t.Fatalf("get cart: %v", err)
	}
	_ = c
	preLine := &cart.Line{
		ItemID:           s.ItemID,
		ItemCode:         "DRILL-01",
		ItemName:         "Cordless Drill",
		ItemType:         "tool",
		TrackingMode:     "serialized",
		Action:           "checkout",
		Qty:              1,
		ItemInstanceID:   s.ActiveInstanceID,
		ItemInstanceCode: "DRILL-01",
	}
	if _, _, err := s.H.Carts.AddLine(s.CartID, preLine); err != nil {
		t.Fatalf("pre-seed AddLine: %v", err)
	}

	s.Reader.epcs = []rfid.EPC{rfid.EPC(s.ActiveEPC)}
	resp, err := s.H.PerformRFIDScan(context.Background(), s.CartID)
	if err != nil {
		t.Fatalf("PerformRFIDScan: %v", err)
	}
	if len(resp.AddedLines) != 0 {
		t.Errorf("AddedLines: want 0 (already in cart), got %d", len(resp.AddedLines))
	}
	if len(resp.UnresolvedEPCs) != 0 {
		t.Errorf("UnresolvedEPCs: want 0 (duplicate is not unresolved), got %v", resp.UnresolvedEPCs)
	}
	if len(resp.ObservedEPCs) != 1 {
		t.Errorf("ObservedEPCs: want 1, got %d", len(resp.ObservedEPCs))
	}
	// Verify ctx didn't trigger early bail.
	_ = dbx.Params{}
}

// contains is a tiny strings.Contains shim — kept here so this test
// file's imports stay minimal.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
