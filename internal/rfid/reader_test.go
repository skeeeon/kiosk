package rfid

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skeeeon/kiosk/internal/config"
)

// TestNew_RejectsDisabled guards the "you forgot to gate the caller"
// case: New on a disabled config block should fail loud rather than
// silently produce a reader that never works. Cross-field validation
// at the config layer should already prevent this, but defense in
// depth is cheap here.
func TestNew_RejectsDisabled(t *testing.T) {
	_, err := New(config.RFIDConfig{Enabled: false})
	if err == nil {
		t.Fatal("expected error from New with enabled=false, got nil")
	}
}

// TestNew_RejectsMissingEndpoint catches a config that passed cross-
// field validation in a surprising way (a future refactor, perhaps) but
// still doesn't have the network endpoint we need to dial.
func TestNew_RejectsMissingEndpoint(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.RFIDConfig
	}{
		{"no host", config.RFIDConfig{Enabled: true, Reader: config.RFIDReaderConfig{Port: 5084}}},
		{"no port", config.RFIDConfig{Enabled: true, Reader: config.RFIDReaderConfig{Host: "h"}}},
		{"neither", config.RFIDConfig{Enabled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestNew_OkWhenConfigured confirms New returns a Reader (not nil) and
// nil error for a sane config. We do not call Connect — that's
// integration-scope, deferred to Phase 2 where the LLRP simulator
// covers it.
func TestNew_OkWhenConfigured(t *testing.T) {
	r, err := New(config.RFIDConfig{
		Enabled:    true,
		Mode:       config.RFIDModeCounterScan,
		Reader:     config.RFIDReaderConfig{Host: "127.0.0.1", Port: 5084},
		ReadWindow: config.Duration(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil Reader")
	}
}

// TestReadFor_NotConnected confirms the wrapper refuses to attempt an
// LLRP exchange before Connect has run. This is the only ReadFor case
// covered by unit tests; the real LLRP message dance lives behind an
// LLRP simulator integration test gated on RFID_SIM=1 (see
// reader_sim_test.go when that lands).
func TestReadFor_NotConnected(t *testing.T) {
	r, err := New(config.RFIDConfig{
		Enabled: true,
		Mode:    config.RFIDModeCounterScan,
		Reader:  config.RFIDReaderConfig{Host: "127.0.0.1", Port: 5084},
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	epcs, err := r.ReadFor(context.Background(), time.Second)
	if !errors.Is(err, ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
	if epcs != nil {
		t.Errorf("expected nil EPC slice, got %v", epcs)
	}
}

// TestDedupEPCs covers the pure helper that collapses repeated tag
// observations into one entry per distinct EPC, preserving first-seen
// order so a transcript stays readable.
func TestDedupEPCs(t *testing.T) {
	cases := []struct {
		name string
		in   []EPC
		want []EPC
	}{
		{"empty", nil, nil},
		{"single", []EPC{"aa"}, []EPC{"aa"}},
		{"all dups", []EPC{"aa", "aa", "aa"}, []EPC{"aa"}},
		{"order preserved", []EPC{"aa", "bb", "aa", "cc", "bb"}, []EPC{"aa", "bb", "cc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupEPCs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("at %d: got %q, want %q (full: got=%v want=%v)", i, got[i], tc.want[i], got, tc.want)
				}
			}
		})
	}
}

// TestClose_NeverConnected is a no-op safety net: Close on a Reader
// that was constructed but never Connected must not panic or error.
// This matches the OnTerminate path in main.go which closes
// unconditionally when a Reader exists.
func TestClose_NeverConnected(t *testing.T) {
	r, err := New(config.RFIDConfig{
		Enabled: true,
		Mode:    config.RFIDModeCounterScan,
		Reader:  config.RFIDReaderConfig{Host: "127.0.0.1", Port: 5084},
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close on never-connected reader should be a no-op, got %v", err)
	}
	// Idempotent.
	if err := r.Close(); err != nil {
		t.Errorf("second Close should also be a no-op, got %v", err)
	}
}
