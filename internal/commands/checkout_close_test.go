package commands

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// seedCheckoutScenario plants the rows needed for a checkout.close command
// to succeed: an admins row (audit FK), a users row (the worker holding the
// tool), an items row, a transactions + transaction_lines row, and an
// open_checkouts row referencing the line.
//
// Returns (transaction_line_id, expected_user_id).
func seedCheckoutScenario(t *testing.T, app core.App) (string, string) {
	t.Helper()
	kioskctx.Set(kioskctx.Identity{KioskCode: "KIOSK01", LocationCode: "WEST"})

	users, _ := app.FindCollectionByNameOrId("users")
	worker := core.NewRecord(users)
	worker.Set("email", "worker@test.local")
	worker.Set("name", "Worker One")
	worker.Set("code", "EMP-100")
	worker.Set("role", "worker")
	worker.Set("active", true)
	worker.SetPassword("worker-password-123")
	if err := app.Save(worker); err != nil {
		t.Fatalf("save worker: %v", err)
	}

	items, _ := app.FindCollectionByNameOrId("items")
	item := core.NewRecord(items)
	item.Set("code", "WRENCH-1")
	item.Set("name", "Wrench")
	item.Set("type", "tool")
	item.Set("tracking_mode", "quantity")
	item.Set("active", true)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}

	now := time.Now().UTC()
	txCol, _ := app.FindCollectionByNameOrId("transactions")
	txRec := core.NewRecord(txCol)
	txRec.Set("kiosk_code", "KIOSK01")
	txRec.Set("location_code", "WEST")
	txRec.Set("user", worker.Id)
	txRec.Set("status", "completed")
	txRec.Set("started_at", now)
	txRec.Set("completed_at", now)
	txRec.Set("lines_count", 1)
	if err := app.Save(txRec); err != nil {
		t.Fatalf("save transaction: %v", err)
	}

	linesCol, _ := app.FindCollectionByNameOrId("transaction_lines")
	lineRec := core.NewRecord(linesCol)
	lineRec.Set("transaction", txRec.Id)
	lineRec.Set("item", item.Id)
	lineRec.Set("action", "checkout")
	lineRec.Set("qty", 1)
	if err := app.Save(lineRec); err != nil {
		t.Fatalf("save transaction_line: %v", err)
	}

	openCol, _ := app.FindCollectionByNameOrId("open_checkouts")
	openRec := core.NewRecord(openCol)
	openRec.Set("item", item.Id)
	openRec.Set("user", worker.Id)
	openRec.Set("checked_out_at", now)
	openRec.Set("transaction_line", lineRec.Id)
	if err := app.Save(openRec); err != nil {
		t.Fatalf("save open_checkout: %v", err)
	}

	return lineRec.Id, worker.Id
}

func TestCheckoutClose_HappyPath(t *testing.T) {
	app := setupApp(t)
	lineID, workerID := seedCheckoutScenario(t, app)

	d := NewDispatcher(app, "KIOSK01")

	payload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-close-1",
		"controller_admin_id": "ctrl-admin-1",
		"transaction_line_id": lineID,
		"reason":              "lost",
		"notes":               "left on jobsite",
	})
	reply := d.handleCheckoutClose(context.Background(), payload)

	if !reply.Success {
		t.Fatalf("expected success, got error %q", reply.Error)
	}

	// The open_checkouts row must be gone.
	rows, _ := app.FindRecordsByFilter("open_checkouts",
		"transaction_line = {:l}", "", 0, 0,
		dbx.Params{"l": lineID})
	if len(rows) != 0 {
		t.Errorf("open_checkouts after close: want 0, got %d", len(rows))
	}

	// A new admin_close transaction_line must exist for the affected worker.
	closeLines, _ := app.FindRecordsByFilter("transaction_lines",
		"action = 'admin_close' && original_checkout_user = {:u}",
		"", 0, 0, dbx.Params{"u": workerID})
	if len(closeLines) != 1 {
		t.Errorf("admin_close lines: want 1, got %d", len(closeLines))
	} else {
		if got := closeLines[0].GetString("closure_reason"); got != "lost" {
			t.Errorf("closure_reason: got %q, want lost", got)
		}
	}
}

func TestCheckoutClose_ValidationErrors(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "missing command_id",
			body: map[string]any{
				"controller_admin_id": "x", "transaction_line_id": "y", "reason": "lost",
			},
			want: "command_id",
		},
		{
			name: "missing controller_admin_id",
			body: map[string]any{
				"command_id": "x", "transaction_line_id": "y", "reason": "lost",
			},
			want: "controller_admin_id",
		},
		{
			name: "missing transaction_line_id",
			body: map[string]any{
				"command_id": "x", "controller_admin_id": "y", "reason": "lost",
			},
			want: "transaction_line_id",
		},
		{
			name: "missing reason",
			body: map[string]any{
				"command_id": "x", "controller_admin_id": "y", "transaction_line_id": "z",
			},
			want: "reason",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(tc.body)
			reply := d.handleCheckoutClose(context.Background(), payload)
			if reply.Success {
				t.Errorf("expected error reply, got success")
			}
			if !strings.Contains(reply.Error, tc.want) {
				t.Errorf("error %q did not contain %q", reply.Error, tc.want)
			}
		})
	}
}

func TestCheckoutClose_MissingOpenCheckout_ReturnsError(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")

	payload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-x",
		"controller_admin_id": "ctrl-admin",
		"transaction_line_id": "no-such-line",
		"reason":              "lost",
	})
	reply := d.handleCheckoutClose(context.Background(), payload)
	if reply.Success {
		t.Fatalf("expected error reply for missing line, got success")
	}
	if !strings.Contains(reply.Error, "already") && !strings.Contains(reply.Error, "no open_checkout") {
		t.Errorf("error should describe missing row, got %q", reply.Error)
	}
}
