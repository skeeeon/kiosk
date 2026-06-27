package handlers_test

import (
	"testing"

	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/handlers"
)

// ReaderByID is the resolution the counter_scan ?reader= URL param relies on
// (Phase 3): a single-reader node resolves implicitly (id=""), a multi-reader
// node requires an explicit id, and an unknown id misses. These are pure map
// lookups on *Handlers, so no PB app is needed.

func TestReaderByID_SingleReaderImplicit(t *testing.T) {
	sole := &handlers.ReaderHandle{Mode: config.RFIDModeCounterScan}
	h := &handlers.Handlers{Readers: map[string]*handlers.ReaderHandle{"counter": sole}}

	rd, ok := h.ReaderByID("")
	if !ok || rd != sole {
		t.Fatalf("empty id on a single-reader node should resolve to the sole reader; ok=%v rd=%p sole=%p", ok, rd, sole)
	}
	// An explicit, matching id still works.
	if rd, ok := h.ReaderByID("counter"); !ok || rd != sole {
		t.Fatalf("explicit id should resolve the named reader; ok=%v", ok)
	}
}

func TestReaderByID_MultiReaderSelection(t *testing.T) {
	a := &handlers.ReaderHandle{Mode: config.RFIDModeCounterScan}
	b := &handlers.ReaderHandle{Mode: config.RFIDModeCounterScan}
	h := &handlers.Handlers{Readers: map[string]*handlers.ReaderHandle{
		"window-a": a,
		"window-b": b,
	}}

	// Explicit selection picks exactly the named reader — this is what
	// ?reader=window-b drives.
	if rd, ok := h.ReaderByID("window-b"); !ok || rd != b {
		t.Errorf("?reader=window-b should select reader b; ok=%v rd=%p b=%p", ok, rd, b)
	}
	if rd, ok := h.ReaderByID("window-a"); !ok || rd != a {
		t.Errorf("?reader=window-a should select reader a; ok=%v", ok)
	}
	// Empty id is ambiguous with more than one reader — the SPA must send
	// ?reader= here (the button is gated on the param being present).
	if _, ok := h.ReaderByID(""); ok {
		t.Errorf("empty id with multiple readers should be ambiguous (ok=false)")
	}
	// Unknown id misses.
	if _, ok := h.ReaderByID("nope"); ok {
		t.Errorf("unknown reader id should miss (ok=false)")
	}
}

func TestReaderByID_NoReaders(t *testing.T) {
	h := &handlers.Handlers{}
	if _, ok := h.ReaderByID(""); ok {
		t.Errorf("a node with no readers should never resolve one")
	}
	if _, ok := h.ReaderByID("anything"); ok {
		t.Errorf("a node with no readers should never resolve one")
	}
}
