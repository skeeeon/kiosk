package events

import (
	"testing"
)

// recordingPublisher captures every PublishBytes call so the test can
// inspect what would have hit NATS. The captured Data is the
// already-marshaled JSON bytes events.Publish produced — tests that need
// to assert on payload contents can json.Unmarshal them.
type recordingPublisher struct {
	calls []struct {
		Subject string
		Data    []byte
	}
}

func (r *recordingPublisher) PublishBytes(subject string, data []byte) error {
	// Copy the slice so tests inspecting r.calls later aren't affected if
	// the production caller reuses its buffer.
	cp := append([]byte(nil), data...)
	r.calls = append(r.calls, struct {
		Subject string
		Data    []byte
	}{subject, cp})
	return nil
}

func (r *recordingPublisher) Close() {}

func TestPublish_NoPublisher_DoesNotPanic(t *testing.T) {
	// Reset just in case some other test left a publisher set.
	SetPublisher(nil)
	// Should run cleanly — only slog logs, no NATS publish.
	Publish("kiosk.TEST.event.item.checkout", map[string]any{"x": 1})
}

func TestPublish_WithPublisher_InvokesIt(t *testing.T) {
	rp := &recordingPublisher{}
	SetPublisher(rp)
	defer SetPublisher(nil)

	Publish("kiosk.TEST.event.transaction.complete", map[string]any{"lines": 3})

	if len(rp.calls) != 1 {
		t.Fatalf("publisher calls: want 1, got %d", len(rp.calls))
	}
	if rp.calls[0].Subject != "kiosk.TEST.event.transaction.complete" {
		t.Errorf("subject: got %q", rp.calls[0].Subject)
	}
}

func TestSetPublisher_NilDeactivates(t *testing.T) {
	rp := &recordingPublisher{}
	SetPublisher(rp)
	SetPublisher(nil)

	Publish("kiosk.TEST.event.item.consume", map[string]any{})
	if len(rp.calls) != 0 {
		t.Errorf("after SetPublisher(nil) publisher should not be invoked, got %d calls", len(rp.calls))
	}
}
