package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
)

// TestCartCommit_StampsRequestDoorID covers the manual-flow seam: the cart
// store's Start never sets DoorID (only the RFID enclosure_diff StartByExternal
// path does), so a badge/scan terminal supplies its door on the commit request
// and the handler injects it into the snapshot. commit_test proves cart.DoorID
// reaches the ledger; this proves the request body reaches cart.DoorID (trimmed).
func TestCartCommit_StampsRequestDoorID(t *testing.T) {
	app := setupApp(t)
	// commit rejects a transaction with no kiosk identity; set the process-global.
	kioskctx.Set(kioskctx.Identity{KioskCode: "TEST", LocationCode: "T"})

	users, _ := app.FindCollectionByNameOrId("users")
	u := core.NewRecord(users)
	u.Set("email", "worker@test.local")
	u.Set("name", "Worker")
	u.Set("code", "EMP-9")
	u.Set("role", "worker")
	u.Set("active", true)
	u.SetPassword("worker-password-123")
	if err := app.Save(u); err != nil {
		t.Fatalf("save user: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	item := core.NewRecord(items)
	item.Set("code", "WIDGET")
	item.Set("name", "Widget")
	item.Set("type", "consumable")
	item.Set("tracking_mode", "quantity")
	item.Set("active", true)
	item.Set("quantity_on_hand", 100)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	store := cart.NewStore(5 * time.Minute)
	c := store.Start(u.Id, "EMP-9", "Worker", "worker")
	if _, _, err := store.AddLine(c.ID, &cart.Line{
		ItemID: item.Id, ItemCode: "WIDGET", ItemName: "Widget",
		ItemType: "consumable", TrackingMode: "quantity",
		Action: "consume", Qty: 1,
	}); err != nil {
		t.Fatalf("add line: %v", err)
	}

	// Managed mode routes the receipt to events.Publish (slog-only with NATS
	// off) instead of the SMTP notifier — keeps the test free of mail I/O.
	h := handlers.New(app, &config.Config{
		Controller: config.ControllerConfig{Enabled: true},
	}, store, notifications.New(app))

	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/cart/commit",
		strings.NewReader(`{"cart_id":"`+c.ID+`","door_id":"  DOOR-B  "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e := new(core.RequestEvent)
	e.App = app
	e.Request = req
	e.Response = rec

	if err := h.CartCommit(e); err != nil {
		t.Fatalf("CartCommit: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	txs, err := app.FindRecordsByFilter("transactions", "user = {:u}", "", 0, 0, dbx.Params{"u": u.Id})
	if err != nil {
		t.Fatalf("find transactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("transactions: want 1, got %d", len(txs))
	}
	// Surrounding whitespace is trimmed by the handler before stamping.
	if got := txs[0].GetString("door_id"); got != "DOOR-B" {
		t.Errorf("door_id: want DOOR-B, got %q", got)
	}
}
