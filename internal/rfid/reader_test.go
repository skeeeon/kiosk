package rfid

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edgexfoundry/device-rfid-llrp-go/pkg/llrp"

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

// TestNew_RejectsAntennaOutOfRange catches an antenna ID that the
// config layer let through (e.g. a future refactor that widens the
// type) but won't fit on the LLRP wire.
func TestNew_RejectsAntennaOutOfRange(t *testing.T) {
	_, err := New(config.RFIDConfig{
		Enabled: true,
		Mode:    config.RFIDModeCounterScan,
		Reader: config.RFIDReaderConfig{
			Host: "h", Port: 5084,
			Antennas: []config.RFIDAntennaConfig{{ID: 70000, TxPowerDBm: 25}},
		},
	})
	if err == nil {
		t.Fatal("expected error for antenna id > uint16, got nil")
	}
}

// TestNearestPowerIndex covers the dBm → wire-index resolver, which
// has to (a) prefer the highest entry at or below the request, never
// silently exceed, (b) clamp upward when every entry is above the
// request (since the reader can't go lower) and (c) handle the typical
// half-dBm-step Impinj power table without rounding chaos.
func TestNearestPowerIndex(t *testing.T) {
	// Mimics an FCC-region Impinj table: index 1 = 10 dBm, stepping
	// 0.25 dBm up to index 81 = 30 dBm. We use a coarse subset here.
	table := []llrp.TransmitPowerLevelTableEntry{
		{Index: 1, TransmitPowerValue: 1000},  // 10.00 dBm
		{Index: 2, TransmitPowerValue: 1500},  // 15.00 dBm
		{Index: 3, TransmitPowerValue: 2000},  // 20.00 dBm
		{Index: 4, TransmitPowerValue: 2500},  // 25.00 dBm
		{Index: 5, TransmitPowerValue: 2575},  // 25.75 dBm
		{Index: 6, TransmitPowerValue: 3000},  // 30.00 dBm
	}
	cases := []struct {
		name     string
		want     float64
		wantIdx  uint16
		wantDBm  float64
	}{
		{"exact match — 20 dBm", 20.0, 3, 20.0},
		{"between 25 and 25.75 → floor to 25", 25.5, 4, 25.0},
		{"above max → floor to max", 35.0, 6, 30.0},
		{"below min → clamp up to min", 5.0, 1, 10.0},
		{"exact min", 10.0, 1, 10.0},
		{"between 10 and 15 → floor to 10", 12.5, 1, 10.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, dBm := nearestPowerIndex(table, tc.want)
			if idx != tc.wantIdx {
				t.Errorf("idx: got %d, want %d", idx, tc.wantIdx)
			}
			if dBm != tc.wantDBm {
				t.Errorf("dBm: got %v, want %v", dBm, tc.wantDBm)
			}
		})
	}
}

// TestPickFrequency exercises the three regulatory shapes we care
// about: a hopping region with a populated hop table, a fixed region
// with a populated channel table, and the degenerate "neither" case
// where we hand zeros back and let the reader use its own defaults.
func TestPickFrequency(t *testing.T) {
	t.Run("hopping with hop table — uses first hop id, channel 0", func(t *testing.T) {
		hop, ch := pickFrequency(llrp.FrequencyInformation{
			Hopping: true,
			FrequencyHopTables: []llrp.FrequencyHopTable{
				{HopTableID: 7, Frequencies: []llrp.Kilohertz{902000, 928000}},
			},
		})
		if hop != 7 || ch != 0 {
			t.Errorf("got (hop=%d ch=%d), want (hop=7 ch=0)", hop, ch)
		}
	})
	t.Run("hopping but empty table — both zero", func(t *testing.T) {
		hop, ch := pickFrequency(llrp.FrequencyInformation{Hopping: true})
		if hop != 0 || ch != 0 {
			t.Errorf("got (hop=%d ch=%d), want (0,0)", hop, ch)
		}
	})
	t.Run("fixed with channels — channel 1, hop 0", func(t *testing.T) {
		hop, ch := pickFrequency(llrp.FrequencyInformation{
			Hopping: false,
			FixedFrequencyTable: &llrp.FixedFrequencyTable{
				Frequencies: []llrp.Kilohertz{866000},
			},
		})
		if hop != 0 || ch != 1 {
			t.Errorf("got (hop=%d ch=%d), want (0,1)", hop, ch)
		}
	})
	t.Run("fixed with no table — both zero", func(t *testing.T) {
		hop, ch := pickFrequency(llrp.FrequencyInformation{Hopping: false})
		if hop != 0 || ch != 0 {
			t.Errorf("got (hop=%d ch=%d), want (0,0)", hop, ch)
		}
	})
}

// TestBuildROSpec_NoTxCfg confirms the default-shape behavior: when
// the operator hasn't configured antennas, the ROSpec uses
// AntennaIDs={0} ("all antennas, reader's own baseline") and emits no
// per-antenna AntennaConfiguration override.
func TestBuildROSpec_NoTxCfg(t *testing.T) {
	spec := buildROSpec(3*time.Second, nil)
	if got := spec.AISpecs[0].AntennaIDs; len(got) != 1 || got[0] != 0 {
		t.Errorf("AntennaIDs: got %v, want [0]", got)
	}
	inv := spec.AISpecs[0].InventoryParameterSpecs[0]
	if len(inv.AntennaConfigurations) != 0 {
		t.Errorf("expected no AntennaConfigurations, got %d", len(inv.AntennaConfigurations))
	}
	if spec.ROBoundarySpec.StopTrigger.DurationTriggerValue != 3000 {
		t.Errorf("DurationTriggerValue: got %d, want 3000",
			spec.ROBoundarySpec.StopTrigger.DurationTriggerValue)
	}
}

// TestBuildROSpec_WithTxCfg confirms the antenna-tuned path: each
// configured antenna is listed in AntennaIDs *and* carries its own
// AntennaConfiguration with the resolved TransmitPowerIndex.
func TestBuildROSpec_WithTxCfg(t *testing.T) {
	cfg := &txConfig{
		hopTableID:   1,
		channelIndex: 0,
		antennas: []resolvedAntenna{
			{id: 1, powerIndex: 41},
			{id: 3, powerIndex: 27},
		},
	}
	spec := buildROSpec(2*time.Second, cfg)

	gotIDs := spec.AISpecs[0].AntennaIDs
	if len(gotIDs) != 2 || gotIDs[0] != 1 || gotIDs[1] != 3 {
		t.Errorf("AntennaIDs: got %v, want [1 3]", gotIDs)
	}

	inv := spec.AISpecs[0].InventoryParameterSpecs[0]
	if len(inv.AntennaConfigurations) != 2 {
		t.Fatalf("AntennaConfigurations: got %d, want 2", len(inv.AntennaConfigurations))
	}
	for i, ac := range inv.AntennaConfigurations {
		want := cfg.antennas[i]
		if uint16(ac.AntennaID) != want.id {
			t.Errorf("[%d] AntennaID: got %d, want %d", i, ac.AntennaID, want.id)
		}
		if ac.RFTransmitter == nil {
			t.Fatalf("[%d] RFTransmitter nil", i)
		}
		if ac.RFTransmitter.TransmitPowerIndex != want.powerIndex {
			t.Errorf("[%d] TransmitPowerIndex: got %d, want %d",
				i, ac.RFTransmitter.TransmitPowerIndex, want.powerIndex)
		}
		if ac.RFTransmitter.HopTableID != cfg.hopTableID {
			t.Errorf("[%d] HopTableID: got %d, want %d",
				i, ac.RFTransmitter.HopTableID, cfg.hopTableID)
		}
		if ac.RFTransmitter.ChannelIndex != cfg.channelIndex {
			t.Errorf("[%d] ChannelIndex: got %d, want %d",
				i, ac.RFTransmitter.ChannelIndex, cfg.channelIndex)
		}
	}
}
