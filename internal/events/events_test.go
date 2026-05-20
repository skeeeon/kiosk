package events

import (
	"testing"
)

// recordingPublisher captures every PublishJSON call so the test can
// inspect what would have hit NATS.
type recordingPublisher struct {
	calls []struct {
		Subject string
		Payload any
	}
}

func (r *recordingPublisher) PublishJSON(subject string, payload any) error {
	r.calls = append(r.calls, struct {
		Subject string
		Payload any
	}{subject, payload})
	return nil
}

func (r *recordingPublisher) Close() {}

func TestPublish_NoPublisher_DoesNotPanic(t *testing.T) {
	// Reset just in case some other test left a publisher set.
	SetPublisher(nil)
	// Should run cleanly — only slog logs, no NATS publish.
	Publish("kiosk.TEST.item.checkout", map[string]any{"x": 1})
}

func TestPublish_WithPublisher_InvokesIt(t *testing.T) {
	rp := &recordingPublisher{}
	SetPublisher(rp)
	defer SetPublisher(nil)

	Publish("kiosk.TEST.transaction.complete", map[string]any{"lines": 3})

	if len(rp.calls) != 1 {
		t.Fatalf("publisher calls: want 1, got %d", len(rp.calls))
	}
	if rp.calls[0].Subject != "kiosk.TEST.transaction.complete" {
		t.Errorf("subject: got %q", rp.calls[0].Subject)
	}
}

func TestSetPublisher_NilDeactivates(t *testing.T) {
	rp := &recordingPublisher{}
	SetPublisher(rp)
	SetPublisher(nil)

	Publish("kiosk.TEST.item.consume", map[string]any{})
	if len(rp.calls) != 0 {
		t.Errorf("after SetPublisher(nil) publisher should not be invoked, got %d calls", len(rp.calls))
	}
}
