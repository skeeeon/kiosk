package handlers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/cartevents"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/rfid"
)

// rfidDiffSeed sets up the enclosure_diff scenario: one worker, one
// serialized item with three active instances:
//   - "stays": currently in the enclosure, will be observed
//   - "leaves": currently in the enclosure, will NOT be observed → checkout
//   - "returning": currently checked out to the worker, will be observed → return
//
// Plus one unknown EPC for the unresolved path.
type rfidDiffSeed struct {
	WorkerID     string
	StayingID    string
	LeavingID    string
	ReturningID  string
	StayingEPC   string
	LeavingEPC   string
	ReturningEPC string
	UnknownEPC   string
	Cart         *cart.Cart
	H            *handlers.Handlers
	Reader       *fakeReader
	Handle       *handlers.ReaderHandle
}

func seedRFIDDiff(t *testing.T) (core.App, rfidDiffSeed) {
	t.Helper()
	app := setupApp(t)

	users, _ := app.FindCollectionByNameOrId("users")
	worker := core.NewRecord(users)
	worker.Set("code", "W-100")
	worker.Set("name", "Worker Hundred")
	worker.Set("role", "worker")
	worker.Set("active", true)
	worker.Set("email", "w100@example.com")
	worker.Set("password", "passwordpassword")
	worker.Set("passwordConfirm", "passwordpassword")
	if err := app.Save(worker); err != nil {
		t.Fatalf("save worker: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	item := core.NewRecord(items)
	item.Set("code", "WRENCH")
	item.Set("name", "Pipe Wrench")
	item.Set("type", "tool")
	item.Set("tracking_mode", "serialized")
	item.Set("active", true)
	item.Set("quantity_on_hand", 3)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	const stayingEPC = "300833b2ddd9014035050000"
	const leavingEPC = "300833b2ddd9014035050001"
	const returningEPC = "300833b2ddd9014035050002"
	const unknownEPC = "deadbeefcafebabe00000099"

	instances, _ := app.FindCollectionByNameOrId("item_instances")
	staying := core.NewRecord(instances)
	staying.Set("item", item.Id)
	staying.Set("code", "WRENCH-A")
	staying.Set("serial", "SN-A")
	staying.Set("rfid_epc", stayingEPC)
	staying.Set("status", "in_service")
	if err := app.Save(staying); err != nil {
		t.Fatalf("save staying instance: %v", err)
	}

	leaving := core.NewRecord(instances)
	leaving.Set("item", item.Id)
	leaving.Set("code", "WRENCH-B")
	leaving.Set("serial", "SN-B")
	leaving.Set("rfid_epc", leavingEPC)
	leaving.Set("status", "in_service")
	if err := app.Save(leaving); err != nil {
		t.Fatalf("save leaving instance: %v", err)
	}

	returning := core.NewRecord(instances)
	returning.Set("item", item.Id)
	returning.Set("code", "WRENCH-C")
	returning.Set("serial", "SN-C")
	returning.Set("rfid_epc", returningEPC)
	returning.Set("status", "in_service")
	if err := app.Save(returning); err != nil {
		t.Fatalf("save returning instance: %v", err)
	}

	// Pre-populate open_checkouts: the "returning" instance is currently
	// out to the worker — observing it should produce a self-return.
	// open_checkouts has FKs to a real transaction_line, so we seed one
	// transaction + line + open row together. Direct inserts bypass
	// commit's tx wrapper which is fine for setup.
	txCol, _ := app.FindCollectionByNameOrId("transactions")
	tx := core.NewRecord(txCol)
	tx.Set("kiosk_code", "TEST")
	tx.Set("location_code", "TEST-LOC")
	tx.Set("user", worker.Id)
	tx.Set("status", "completed")
	tx.Set("started_at", time.Now().UTC())
	tx.Set("completed_at", time.Now().UTC())
	if err := app.Save(tx); err != nil {
		t.Fatalf("save tx: %v", err)
	}
	lineCol, _ := app.FindCollectionByNameOrId("transaction_lines")
	line := core.NewRecord(lineCol)
	line.Set("transaction", tx.Id)
	line.Set("item", item.Id)
	line.Set("item_instance", returning.Id)
	line.Set("action", "checkout")
	line.Set("qty", 1)
	if err := app.Save(line); err != nil {
		t.Fatalf("save line: %v", err)
	}
	openCol, _ := app.FindCollectionByNameOrId("open_checkouts")
	row := core.NewRecord(openCol)
	row.Set("user", worker.Id)
	row.Set("item", item.Id)
	row.Set("item_instance", returning.Id)
	row.Set("transaction_line", line.Id)
	row.Set("checked_out_at", time.Now().UTC().Format("2006-01-02 15:04:05.000Z"))
	if err := app.Save(row); err != nil {
		t.Fatalf("save open_checkout: %v", err)
	}

	cfg := &config.Config{
		RFID: config.RFIDConfig{
			Enabled:    true,
			ReadWindow: config.Duration(50 * time.Millisecond),
		},
	}
	store := cart.NewStore(5 * time.Minute)
	c, _ := store.StartByExternal(worker.Id, worker.GetString("code"),
		worker.GetString("name"), worker.GetString("role"), "BAY-A")

	reader := &fakeReader{}
	h := handlers.New(app, cfg, store, notifications.New(app))
	rd := &handlers.ReaderHandle{Reader: reader, Mode: config.RFIDModeEnclosureDiff, EnclosureID: "BAY-A"}
	h.Readers = map[string]*handlers.ReaderHandle{"cabinet": rd}

	return app, rfidDiffSeed{
		WorkerID:     worker.Id,
		StayingID:    staying.Id,
		LeavingID:    leaving.Id,
		ReturningID:  returning.Id,
		StayingEPC:   stayingEPC,
		LeavingEPC:   leavingEPC,
		ReturningEPC: returningEPC,
		UnknownEPC:   unknownEPC,
		Cart:         c,
		H:            h,
		Reader:       reader,
		Handle:       rd,
	}
}

// TestPerformReadTrigger_HappyPath: the dominant case. Observed EPCs
// cover one staying tag + one returning tag + one unknown. Expected
// outcome: 1 checkout (leaving wasn't seen), 1 return (returning
// was seen), 1 unresolved (unknown), 4 observed.
func TestPerformReadTrigger_HappyPath(t *testing.T) {
	_, s := seedRFIDDiff(t)
	s.Reader.epcs = []rfid.EPC{
		rfid.EPC(s.StayingEPC),
		rfid.EPC(s.ReturningEPC),
		rfid.EPC(s.UnknownEPC),
	}

	resp, err := s.H.PerformReadTrigger(context.Background(), s.Cart, s.Handle)
	if err != nil {
		t.Fatalf("PerformReadTrigger: %v", err)
	}

	// 1 checkout: the "leaving" instance (expected present, not observed).
	// 1 return: the "returning" instance (was out, observed back).
	if got := len(resp.AddedLines); got != 2 {
		t.Errorf("AddedLines: want 2, got %d (%v)", got, resp.AddedLines)
	}
	var checkouts, returns int
	for _, l := range resp.AddedLines {
		switch l.Action {
		case "checkout":
			checkouts++
			if l.ItemInstanceID != s.LeavingID {
				t.Errorf("checkout line should be for leaving instance, got %q", l.ItemInstanceID)
			}
		case "return":
			returns++
			if l.ItemInstanceID != s.ReturningID {
				t.Errorf("return line should be for returning instance, got %q", l.ItemInstanceID)
			}
		}
	}
	if checkouts != 1 || returns != 1 {
		t.Errorf("want 1 checkout + 1 return, got %d/%d", checkouts, returns)
	}
	if len(resp.UnresolvedEPCs) != 1 || resp.UnresolvedEPCs[0] != s.UnknownEPC {
		t.Errorf("UnresolvedEPCs: want [%s], got %v", s.UnknownEPC, resp.UnresolvedEPCs)
	}
	if resp.SkippedCrossUserCount != 0 {
		t.Errorf("SkippedCrossUserCount: want 0, got %d", resp.SkippedCrossUserCount)
	}
}

// TestPerformReadTrigger_EmptyRead: the reader saw nothing in a
// non-empty enclosure → the diff says every expected-present
// instance left. Worth covering because the inner observedSet
// initialization with 0 entries is a classic off-by-one trap.
func TestPerformReadTrigger_EmptyRead(t *testing.T) {
	_, s := seedRFIDDiff(t)
	s.Reader.epcs = nil

	resp, err := s.H.PerformReadTrigger(context.Background(), s.Cart, s.Handle)
	if err != nil {
		t.Fatalf("PerformReadTrigger: %v", err)
	}
	// staying + leaving were both expected present → both become
	// checkouts. returning was already out → stays out.
	if got := len(resp.AddedLines); got != 2 {
		t.Errorf("AddedLines: want 2 checkouts, got %d", got)
	}
	for _, l := range resp.AddedLines {
		if l.Action != "checkout" {
			t.Errorf("all lines should be checkouts in an empty-read batch, got %q", l.Action)
		}
	}
	if len(resp.ObservedEPCs) != 0 {
		t.Errorf("ObservedEPCs: want 0, got %d", len(resp.ObservedEPCs))
	}
}

// TestPerformReadTrigger_AllObserved: every expected-present and
// every checked-out tag is in this read → no diff effects, just a
// fully visible enclosure. Useful sanity check for the "operator
// triggered a read but didn't actually do anything" path.
func TestPerformReadTrigger_AllObserved(t *testing.T) {
	_, s := seedRFIDDiff(t)
	s.Reader.epcs = []rfid.EPC{
		rfid.EPC(s.StayingEPC),
		rfid.EPC(s.LeavingEPC),
		// returning is already out; "observed back" → return
		rfid.EPC(s.ReturningEPC),
	}

	resp, err := s.H.PerformReadTrigger(context.Background(), s.Cart, s.Handle)
	if err != nil {
		t.Fatalf("PerformReadTrigger: %v", err)
	}
	// staying observed → no-op; leaving observed (expected present
	// and observed) → no-op; returning observed → return.
	if got := len(resp.AddedLines); got != 1 {
		t.Errorf("AddedLines: want 1 (return only), got %d", got)
	}
	if len(resp.AddedLines) > 0 && resp.AddedLines[0].Action != "return" {
		t.Errorf("single added line should be a return, got %q", resp.AddedLines[0].Action)
	}
}

// TestPerformReadTrigger_CrossUserReturnSkipped covers the v1
// policy: an observed return whose open_checkouts row belongs to a
// different worker is skipped (with a count). This avoids silently
// bypassing the commit-time foreman+same-group gate.
func TestPerformReadTrigger_CrossUserReturnSkipped(t *testing.T) {
	app, s := seedRFIDDiff(t)

	// Move the returning instance's open_checkouts row to a different
	// worker. Direct DB update bypasses the commit machinery — fine
	// for setup.
	users, _ := app.FindCollectionByNameOrId("users")
	other := core.NewRecord(users)
	other.Set("code", "W-OTHER")
	other.Set("name", "Other Worker")
	other.Set("role", "worker")
	other.Set("active", true)
	other.Set("email", "other@example.com")
	other.Set("password", "passwordpassword")
	other.Set("passwordConfirm", "passwordpassword")
	if err := app.Save(other); err != nil {
		t.Fatalf("save other: %v", err)
	}
	rows, _ := app.FindRecordsByFilter("open_checkouts",
		"item_instance = {:i}", "", 1, 0, map[string]any{"i": s.ReturningID})
	if len(rows) != 1 {
		t.Fatalf("expected 1 open_checkout row, got %d", len(rows))
	}
	rows[0].Set("user", other.Id)
	if err := app.Save(rows[0]); err != nil {
		t.Fatalf("re-assign open_checkout: %v", err)
	}

	// Observe all three EPCs so the staying/leaving instances stay
	// no-ops; the only diff effect should be the cross-user return,
	// which we expect to be skipped.
	s.Reader.epcs = []rfid.EPC{
		rfid.EPC(s.StayingEPC),
		rfid.EPC(s.LeavingEPC),
		rfid.EPC(s.ReturningEPC),
	}

	resp, err := s.H.PerformReadTrigger(context.Background(), s.Cart, s.Handle)
	if err != nil {
		t.Fatalf("PerformReadTrigger: %v", err)
	}
	if got := len(resp.AddedLines); got != 0 {
		t.Errorf("cross-user return should be skipped; got %d added lines (%v)", got, resp.AddedLines)
	}
	if resp.SkippedCrossUserCount != 1 {
		t.Errorf("SkippedCrossUserCount: want 1, got %d", resp.SkippedCrossUserCount)
	}
}

// TestPerformReadTrigger_FiresBrokerTickle: after a successful read,
// exactly one tickle should reach subscribers — even if zero cart
// lines landed. This is the SPA's refetch signal.
func TestPerformReadTrigger_FiresBrokerTickle(t *testing.T) {
	_, s := seedRFIDDiff(t)
	s.Reader.epcs = nil // zero-effect read

	ch, unsub := s.H.CartEvents.Subscribe(s.Cart.ID)
	defer unsub()

	if _, err := s.H.PerformReadTrigger(context.Background(), s.Cart, s.Handle); err != nil {
		t.Fatalf("PerformReadTrigger: %v", err)
	}

	select {
	case sig := <-ch:
		if sig.Kind != cartevents.EventUpdated {
			t.Errorf("expected EventUpdated, got %q", sig.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no broker tickle after PerformReadTrigger")
	}
}

// TestPerformReadTrigger_NilCart: the helper guards against a nil
// cart up front so a bug in the caller surfaces clearly instead of
// nil-deref'ing.
func TestPerformReadTrigger_NilCart(t *testing.T) {
	_, s := seedRFIDDiff(t)
	_, err := s.H.PerformReadTrigger(context.Background(), nil, s.Handle)
	if err == nil {
		t.Fatal("expected error for nil cart")
	}
}

// TestPerformReadTrigger_ReaderError surfaces the underlying ReadFor
// error wrapped so the HTTP layer can translate to a 503.
func TestPerformReadTrigger_ReaderError(t *testing.T) {
	_, s := seedRFIDDiff(t)
	s.Reader.err = errors.New("reader unplugged")

	_, err := s.H.PerformReadTrigger(context.Background(), s.Cart, s.Handle)
	if err == nil {
		t.Fatal("expected error from ReadFor failure")
	}
	if !contains(err.Error(), "reader unplugged") {
		t.Errorf("error should wrap the underlying message; got %q", err.Error())
	}
}
