package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestHeartbeatRegistry_RecordBeat_Snapshot covers the basic in/out path.
func TestHeartbeatRegistry_RecordBeat_Snapshot(t *testing.T) {
	r := NewHeartbeatRegistry(nil)
	now := time.Now().UTC()
	r.RecordBeat("K01", now)
	r.RecordBeat("K02", now.Add(-time.Minute))

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len: want 2, got %d", len(snap))
	}
	if !snap["K01"].Equal(now) {
		t.Errorf("K01 ts: got %v want %v", snap["K01"], now)
	}
	// Snapshot must be a copy — mutating it shouldn't affect future
	// snapshots.
	snap["K01"] = time.Time{}
	if (r.Snapshot()["K01"]).IsZero() {
		t.Error("Snapshot returned a live reference instead of a copy")
	}
}

// TestHeartbeatRegistry_IsLikelyOnline gates on the freshness window.
func TestHeartbeatRegistry_IsLikelyOnline(t *testing.T) {
	r := NewHeartbeatRegistry(nil)
	now := time.Now().UTC()
	r.RecordBeat("FRESH", now)
	r.RecordBeat("STALE", now.Add(-2*time.Minute))

	if !r.IsLikelyOnline("FRESH", 90*time.Second) {
		t.Error("FRESH should be online")
	}
	if r.IsLikelyOnline("STALE", 90*time.Second) {
		t.Error("STALE should not be online (2min old, 90s window)")
	}
	if r.IsLikelyOnline("UNSEEN", 90*time.Second) {
		t.Error("UNSEEN kiosk has no beat — must not be reported online")
	}
}

// TestHeartbeatRegistry_HandleParsesPayload feeds a JSON beat through the
// NATS callback path and verifies the map is updated.
func TestHeartbeatRegistry_HandleParsesPayload(t *testing.T) {
	r := NewHeartbeatRegistry(nil)
	ts := time.Now().UTC().Truncate(time.Second)
	data, _ := json.Marshal(map[string]any{
		"ts":       ts,
		"code":     "K42",
		"location": "BAY1",
		"version":  "v1.2.3",
	})
	r.handle(&nats.Msg{Subject: "kiosk.K42.heartbeat", Data: data})

	got, ok := r.LastBeat("K42")
	if !ok {
		t.Fatal("K42 not recorded")
	}
	if !got.Equal(ts) {
		t.Errorf("K42 ts: got %v, want %v", got, ts)
	}
}

// TestHeartbeatRegistry_HandleLegacyPayload keeps beats from kiosk builds
// that predate the {code, location, ts} payload alignment working — they
// still publish kiosk_code/location_code. Drop with the fallback in handle.
func TestHeartbeatRegistry_HandleLegacyPayload(t *testing.T) {
	var calls []struct{ Code, Location string }
	touch := func(code, loc string) error {
		calls = append(calls, struct{ Code, Location string }{code, loc})
		return nil
	}
	r := NewHeartbeatRegistry(touch)
	data, _ := json.Marshal(map[string]any{
		"ts":            time.Now().UTC(),
		"kiosk_code":    "OLDIE",
		"location_code": "BAY9",
	})
	r.handle(&nats.Msg{Subject: "kiosk.OLDIE.heartbeat", Data: data})

	if _, ok := r.LastBeat("OLDIE"); !ok {
		t.Fatal("legacy kiosk_code payload not recorded")
	}
	if len(calls) != 1 || calls[0].Code != "OLDIE" || calls[0].Location != "BAY9" {
		t.Errorf("legacy touch: got %+v, want [{OLDIE BAY9}]", calls)
	}
}

// TestHeartbeatRegistry_AutoRegister verifies the touch callback fires on
// the FIRST beat from a previously-unknown kiosk, and only on the first.
func TestHeartbeatRegistry_AutoRegister(t *testing.T) {
	var calls []struct{ Code, Location string }
	touch := func(code, loc string) error {
		calls = append(calls, struct{ Code, Location string }{code, loc})
		return nil
	}
	r := NewHeartbeatRegistry(touch)
	data, _ := json.Marshal(map[string]any{
		"ts":       time.Now().UTC(),
		"code":     "NEWBIE",
		"location": "BAY3",
	})
	r.handle(&nats.Msg{Subject: "kiosk.NEWBIE.heartbeat", Data: data})
	r.handle(&nats.Msg{Subject: "kiosk.NEWBIE.heartbeat", Data: data}) // second beat
	r.handle(&nats.Msg{Subject: "kiosk.NEWBIE.heartbeat", Data: data}) // third

	if len(calls) != 1 {
		t.Errorf("touch fires: want 1 (first beat only), got %d", len(calls))
	}
	if len(calls) >= 1 && calls[0].Code != "NEWBIE" {
		t.Errorf("touch code: got %q, want NEWBIE", calls[0].Code)
	}
}

// TestHeartbeatRegistry_StartedAt is set at construction and immutable.
func TestHeartbeatRegistry_StartedAt(t *testing.T) {
	before := time.Now().UTC()
	r := NewHeartbeatRegistry(nil)
	after := time.Now().UTC()
	got := r.StartedAt()
	if got.Before(before) || got.After(after) {
		t.Errorf("StartedAt %v outside [%v, %v]", got, before, after)
	}
}

// TestHeartbeatRegistry_MalformedPayload doesn't crash and doesn't update.
func TestHeartbeatRegistry_MalformedPayload(t *testing.T) {
	r := NewHeartbeatRegistry(nil)
	r.handle(&nats.Msg{Subject: "kiosk.X.heartbeat", Data: []byte("not json")})
	r.handle(&nats.Msg{Subject: "kiosk.X.heartbeat", Data: []byte(`{"no_kiosk_code":1}`)})
	if len(r.Snapshot()) != 0 {
		t.Errorf("malformed payloads must not record beats; snapshot=%v", r.Snapshot())
	}
}
