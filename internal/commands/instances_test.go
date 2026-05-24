package commands

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/instances"
)

// Tests exercise the dispatcher's instance.* handlers end-to-end against
// a real PB app. Behavior split:
//   - create / decommission / reactivate: audit + lifecycle event;
//     idempotent on command_id.
//   - edit: no audit, no event; cosmetic-only.
//   - snapshot: read-only, no writes.

func seedSerializedItem(t *testing.T, app core.App, code string) string {
	t.Helper()
	items, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatalf("find items: %v", err)
	}
	item := core.NewRecord(items)
	item.Set("code", code)
	item.Set("name", "Test "+code)
	item.Set("type", "tool")
	item.Set("tracking_mode", "serialized")
	item.Set("active", true)
	if err := app.Save(item); err != nil {
		t.Fatalf("save item: %v", err)
	}
	return item.Id
}

func countAuditByCommandID(t *testing.T, app core.App, commandID string) int {
	t.Helper()
	rows, err := app.FindRecordsByFilter("instance_audit",
		"command_id = {:c}", "", 10, 0, dbx.Params{"c": commandID})
	if err != nil {
		t.Fatalf("list instance_audit: %v", err)
	}
	return len(rows)
}

func TestInstanceCreate_HappyPath(t *testing.T) {
	app := setupApp(t)
	seedSerializedItem(t, app, "DRILL")

	d := NewDispatcher(app, "KIOSK01")
	payload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-create-1",
		"controller_admin_id": "ctrl-admin",
		"item_code":           "DRILL",
		"code":                "DRILL-001",
		"serial":              "SN-001",
	})
	reply := d.handleInstanceCreate(context.Background(), payload)
	if !reply.Success {
		t.Fatalf("create failed: %q", reply.Error)
	}

	dataBytes, _ := json.Marshal(reply.Data)
	var out instances.InstanceResult
	if err := json.Unmarshal(dataBytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.InstanceCode != "DRILL-001" {
		t.Errorf("InstanceCode = %q, want DRILL-001", out.InstanceCode)
	}
	if !out.Active {
		t.Errorf("Active default should be true")
	}
	if n := countAuditByCommandID(t, app, "cmd-create-1"); n != 1 {
		t.Errorf("instance_audit rows for command_id: want 1, got %d", n)
	}
}

func TestInstanceCreate_IdempotentReplay(t *testing.T) {
	app := setupApp(t)
	seedSerializedItem(t, app, "DRILL")

	d := NewDispatcher(app, "KIOSK01")
	payload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-replay-1",
		"controller_admin_id": "ctrl-admin",
		"item_code":           "DRILL",
		"code":                "DRILL-002",
	})

	first := d.handleInstanceCreate(context.Background(), payload)
	if !first.Success {
		t.Fatalf("first create failed: %q", first.Error)
	}
	second := d.handleInstanceCreate(context.Background(), payload)
	if !second.Success {
		t.Fatalf("idempotent replay failed: %q", second.Error)
	}

	if n := countAuditByCommandID(t, app, "cmd-replay-1"); n != 1 {
		t.Errorf("replay must not create a second audit row: got %d rows", n)
	}
	// Both replies should reference the same instance.
	var a, b instances.InstanceResult
	ab, _ := json.Marshal(first.Data)
	bb, _ := json.Marshal(second.Data)
	_ = json.Unmarshal(ab, &a)
	_ = json.Unmarshal(bb, &b)
	if a.InstanceID != b.InstanceID {
		t.Errorf("replay returned a different instance: first=%s second=%s", a.InstanceID, b.InstanceID)
	}
}

func TestInstanceCreate_ValidationErrors(t *testing.T) {
	app := setupApp(t)
	d := NewDispatcher(app, "KIOSK01")
	cases := []struct {
		name    string
		payload map[string]any
		wantErr string
	}{
		{
			"missing command_id",
			map[string]any{"controller_admin_id": "ctrl", "item_code": "X", "code": "X-1"},
			"command_id",
		},
		{
			"missing controller_admin_id",
			map[string]any{"command_id": "c", "item_code": "X", "code": "X-1"},
			"controller_admin_id",
		},
		{
			"missing item_code",
			map[string]any{"command_id": "c", "controller_admin_id": "ctrl", "code": "X-1"},
			"item_code",
		},
		{
			"missing code",
			map[string]any{"command_id": "c", "controller_admin_id": "ctrl", "item_code": "X"},
			"code is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := json.Marshal(c.payload)
			reply := d.handleInstanceCreate(context.Background(), b)
			if reply.Success {
				t.Errorf("expected validation failure")
			}
			if reply.Error == "" {
				t.Errorf("expected error message")
			}
		})
	}
}

func TestInstanceEdit_CosmeticNoAudit(t *testing.T) {
	app := setupApp(t)
	seedSerializedItem(t, app, "DRILL")

	d := NewDispatcher(app, "KIOSK01")
	// First create an instance to edit.
	createPayload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-edit-setup",
		"controller_admin_id": "ctrl",
		"item_code":           "DRILL",
		"code":                "DRILL-EDIT",
	})
	if r := d.handleInstanceCreate(context.Background(), createPayload); !r.Success {
		t.Fatalf("setup create: %q", r.Error)
	}
	auditBefore := countAuditByCommandID(t, app, "cmd-edit-setup")

	newSerial := "SN-EDIT-001"
	editPayload, _ := json.Marshal(map[string]any{
		"instance_code": "DRILL-EDIT",
		"serial":        newSerial,
	})
	reply := d.handleInstanceEdit(context.Background(), editPayload)
	if !reply.Success {
		t.Fatalf("edit failed: %q", reply.Error)
	}

	// Verify serial was actually updated on the record.
	inst, err := app.FindFirstRecordByFilter("item_instances",
		"code = {:c}", dbx.Params{"c": "DRILL-EDIT"})
	if err != nil {
		t.Fatalf("find instance: %v", err)
	}
	if inst.GetString("serial") != newSerial {
		t.Errorf("serial: got %q, want %q", inst.GetString("serial"), newSerial)
	}

	// No new audit row (cosmetic edits don't audit).
	if auditAfter := countAuditByCommandID(t, app, "cmd-edit-setup"); auditAfter != auditBefore {
		t.Errorf("edit must not add audit rows: before=%d after=%d", auditBefore, auditAfter)
	}
}

func TestInstanceDecommission_HappyPath(t *testing.T) {
	app := setupApp(t)
	seedSerializedItem(t, app, "DRILL")

	d := NewDispatcher(app, "KIOSK01")
	createPayload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-decom-setup",
		"controller_admin_id": "ctrl",
		"item_code":           "DRILL",
		"code":                "DRILL-DEC",
	})
	if r := d.handleInstanceCreate(context.Background(), createPayload); !r.Success {
		t.Fatalf("setup: %q", r.Error)
	}

	decomPayload, _ := json.Marshal(map[string]any{
		"command_id":          "cmd-decom-1",
		"controller_admin_id": "ctrl",
		"instance_code":       "DRILL-DEC",
		"reason":              "broken handle",
	})
	reply := d.handleInstanceDecommission(context.Background(), decomPayload)
	if !reply.Success {
		t.Fatalf("decommission failed: %q", reply.Error)
	}

	inst, err := app.FindFirstRecordByFilter("item_instances",
		"code = {:c}", dbx.Params{"c": "DRILL-DEC"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if inst.GetBool("active") {
		t.Errorf("instance should be inactive after decommission")
	}

	// One audit row for the decommission action.
	rows, err := app.FindRecordsByFilter("instance_audit",
		"command_id = {:c} && action = 'decommission'", "", 10, 0,
		dbx.Params{"c": "cmd-decom-1"})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("decommission audit: want 1, got %d", len(rows))
	}
}

func TestInstanceReactivate_AfterDecommission(t *testing.T) {
	app := setupApp(t)
	seedSerializedItem(t, app, "DRILL")

	d := NewDispatcher(app, "KIOSK01")
	for _, cmd := range []map[string]any{
		{"command_id": "create", "controller_admin_id": "ctrl", "item_code": "DRILL", "code": "DRILL-RE"},
	} {
		b, _ := json.Marshal(cmd)
		if r := d.handleInstanceCreate(context.Background(), b); !r.Success {
			t.Fatalf("setup: %q", r.Error)
		}
	}
	decomPayload, _ := json.Marshal(map[string]any{
		"command_id":          "decom",
		"controller_admin_id": "ctrl",
		"instance_code":       "DRILL-RE",
		"reason":              "evaluating",
	})
	if r := d.handleInstanceDecommission(context.Background(), decomPayload); !r.Success {
		t.Fatalf("decom: %q", r.Error)
	}

	reactPayload, _ := json.Marshal(map[string]any{
		"command_id":          "react",
		"controller_admin_id": "ctrl",
		"instance_code":       "DRILL-RE",
		"reason":              "fixed",
	})
	reply := d.handleInstanceReactivate(context.Background(), reactPayload)
	if !reply.Success {
		t.Fatalf("reactivate failed: %q", reply.Error)
	}

	inst, _ := app.FindFirstRecordByFilter("item_instances",
		"code = {:c}", dbx.Params{"c": "DRILL-RE"})
	if !inst.GetBool("active") {
		t.Errorf("instance should be active after reactivate")
	}
}

func TestInstanceSnapshot_FilterByItem(t *testing.T) {
	app := setupApp(t)
	seedSerializedItem(t, app, "DRILL")
	seedSerializedItem(t, app, "SAW")

	d := NewDispatcher(app, "KIOSK01")
	for _, cmd := range []map[string]any{
		{"command_id": "a", "controller_admin_id": "ctrl", "item_code": "DRILL", "code": "D-1"},
		{"command_id": "b", "controller_admin_id": "ctrl", "item_code": "DRILL", "code": "D-2"},
		{"command_id": "c", "controller_admin_id": "ctrl", "item_code": "SAW", "code": "S-1"},
	} {
		b, _ := json.Marshal(cmd)
		if r := d.handleInstanceCreate(context.Background(), b); !r.Success {
			t.Fatalf("setup %s: %q", cmd["code"], r.Error)
		}
	}

	// Filtered: only DRILL instances.
	payload, _ := json.Marshal(map[string]any{"item_code": "DRILL"})
	reply := d.handleInstanceSnapshot(context.Background(), payload)
	if !reply.Success {
		t.Fatalf("snapshot failed: %q", reply.Error)
	}
	dataBytes, _ := json.Marshal(reply.Data)
	var out struct {
		Instances []instances.SnapshotRow `json:"instances"`
	}
	if err := json.Unmarshal(dataBytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Instances) != 2 {
		t.Errorf("filtered count: want 2, got %d", len(out.Instances))
	}

	// Unfiltered: all 3.
	reply = d.handleInstanceSnapshot(context.Background(), []byte("{}"))
	if !reply.Success {
		t.Fatalf("snapshot all failed: %q", reply.Error)
	}
	dataBytes, _ = json.Marshal(reply.Data)
	_ = json.Unmarshal(dataBytes, &out)
	if len(out.Instances) != 3 {
		t.Errorf("unfiltered count: want 3, got %d", len(out.Instances))
	}
}
