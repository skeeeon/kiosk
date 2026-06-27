package config

import (
	"strings"
	"testing"
	"time"
)

// readers is a tiny helper to build the rfid.readers map in test cases.
func readers(m map[string]RFIDReaderConfig) map[string]RFIDReaderConfig { return m }

// TestValidateRFID covers the cross-field invariants on the RFID config
// block. When disabled, everything is irrelevant; when enabled, at least one
// reader is required and each reader needs a valid mode + endpoint;
// enclosure_diff readers additionally require an enclosure_id; the shared
// read_window defaults to 3s and is capped when any reader is enclosure_diff.
func TestValidateRFID(t *testing.T) {
	cases := []struct {
		name      string
		in        RFIDConfig
		wantErr   string // substring; empty = expect success
		wantApply func(t *testing.T, out RFIDConfig)
	}{
		{
			name: "disabled — everything else ignored",
			in:   RFIDConfig{Enabled: false, Readers: readers(map[string]RFIDReaderConfig{"x": {Mode: "bogus"}})},
		},
		{
			name: "enabled, one counter reader, read_window set — preserved",
			in: RFIDConfig{
				Enabled:    true,
				ReadWindow: Duration(5 * time.Second),
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "10.0.0.50", Port: 5084},
				}),
			},
			wantApply: func(t *testing.T, out RFIDConfig) {
				if out.ReadWindow.AsDuration() != 5*time.Second {
					t.Errorf("read_window should be preserved at 5s, got %v", out.ReadWindow.AsDuration())
				}
			},
		},
		{
			name: "enabled, counter reader, read_window unset — defaults to 3s",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "10.0.0.50", Port: 5084},
				}),
			},
			wantApply: func(t *testing.T, out RFIDConfig) {
				if out.ReadWindow.AsDuration() != 3*time.Second {
					t.Errorf("read_window should default to 3s, got %v", out.ReadWindow.AsDuration())
				}
			},
		},
		{
			name: "enabled, enclosure reader with enclosure_id — ok",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"cabinet": {Mode: RFIDModeEnclosureDiff, Host: "h", Port: 5084, EnclosureID: "BAY-A"},
				}),
			},
		},
		{
			name: "mixed counter + enclosure on one node — ok",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "h", Port: 5084},
					"cabinet": {Mode: RFIDModeEnclosureDiff, Host: "h2", Port: 5084, EnclosureID: "BAY-A"},
				}),
			},
		},
		{
			name: "enclosure read_window over cap — error",
			in: RFIDConfig{
				Enabled:    true,
				ReadWindow: Duration(5 * time.Second),
				Readers: readers(map[string]RFIDReaderConfig{
					"cabinet": {Mode: RFIDModeEnclosureDiff, Host: "h", Port: 5084, EnclosureID: "BAY-A"},
				}),
			},
			wantErr: "too long with an enclosure_diff reader",
		},
		{
			name: "enclosure read_window at cap — ok",
			in: RFIDConfig{
				Enabled:    true,
				ReadWindow: Duration(MaxEnclosureReadWindow),
				Readers: readers(map[string]RFIDReaderConfig{
					"cabinet": {Mode: RFIDModeEnclosureDiff, Host: "h", Port: 5084, EnclosureID: "BAY-A"},
				}),
			},
			wantApply: func(t *testing.T, out RFIDConfig) {
				if out.ReadWindow.AsDuration() != MaxEnclosureReadWindow {
					t.Errorf("read_window at cap should be preserved, got %v", out.ReadWindow.AsDuration())
				}
			},
		},
		{
			name: "counter-only long read_window — ok (not capped)",
			in: RFIDConfig{
				Enabled:    true,
				ReadWindow: Duration(10 * time.Second),
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "h", Port: 5084},
				}),
			},
		},
		{
			name:    "enabled with no readers — error",
			in:      RFIDConfig{Enabled: true},
			wantErr: "rfid.readers must have at least one entry",
		},
		{
			name: "reader with no mode — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{"counter": {Host: "h", Port: 5084}}),
			},
			wantErr: "mode is required",
		},
		{
			name: "reader with unknown mode — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{"counter": {Mode: "bulk_scan", Host: "h", Port: 5084}}),
			},
			wantErr: `mode must be "counter_scan" or "enclosure_diff"`,
		},
		{
			name: "reader with no host — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{"counter": {Mode: RFIDModeCounterScan, Port: 5084}}),
			},
			wantErr: "host is required",
		},
		{
			name: "reader with no port — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{"counter": {Mode: RFIDModeCounterScan, Host: "h"}}),
			},
			wantErr: "port is required",
		},
		{
			name: "enclosure reader without enclosure_id — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"cabinet": {Mode: RFIDModeEnclosureDiff, Host: "h", Port: 5084},
				}),
			},
			wantErr: "enclosure_id is required",
		},
		{
			name: "antennas — distinct ids and powers — ok",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "h", Port: 5084, Antennas: []RFIDAntennaConfig{
						{ID: 1, TxPowerDBm: 25.0},
						{ID: 3, TxPowerDBm: 20.5},
					}},
				}),
			},
		},
		{
			name: "antennas — zero id — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "h", Port: 5084, Antennas: []RFIDAntennaConfig{{ID: 0, TxPowerDBm: 25.0}}},
				}),
			},
			wantErr: "antennas[0].id must be >= 1",
		},
		{
			name: "antennas — duplicate id — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "h", Port: 5084, Antennas: []RFIDAntennaConfig{
						{ID: 1, TxPowerDBm: 25.0},
						{ID: 1, TxPowerDBm: 20.0},
					}},
				}),
			},
			wantErr: "duplicate id 1",
		},
		{
			name: "antennas — non-positive tx_power_dbm — error",
			in: RFIDConfig{
				Enabled: true,
				Readers: readers(map[string]RFIDReaderConfig{
					"counter": {Mode: RFIDModeCounterScan, Host: "h", Port: 5084, Antennas: []RFIDAntennaConfig{{ID: 1, TxPowerDBm: 0}}},
				}),
			},
			wantErr: "tx_power_dbm must be > 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.in
			err := validateRFID(&r)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.wantApply != nil {
					tc.wantApply(t, r)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestRFIDEnvOverrides verifies the top-level KIOSK_RFID_* env vars override
// file values. Per-reader fields live in the rfid.readers map and are
// YAML-only — there's no flat env path into a map entry — so only Enabled and
// ReadWindow have env overrides.
func TestRFIDEnvOverrides(t *testing.T) {
	t.Run("KIOSK_RFID_ENABLED=true flips disabled to enabled", func(t *testing.T) {
		c := &Config{}
		t.Setenv("KIOSK_RFID_ENABLED", "true")
		applyEnvOverrides(c)
		if !c.RFID.Enabled {
			t.Errorf("expected Enabled=true, got false")
		}
	})

	t.Run("KIOSK_RFID_READ_WINDOW parses durations", func(t *testing.T) {
		c := &Config{}
		t.Setenv("KIOSK_RFID_READ_WINDOW", "7s")
		applyEnvOverrides(c)
		if c.RFID.ReadWindow.AsDuration() != 7*time.Second {
			t.Errorf("expected ReadWindow=7s, got %v", c.RFID.ReadWindow.AsDuration())
		}
	})
}

// TestEnclosureIDs verifies the distinct-sorted cabinet set the admin SPA uses
// for enclosure-assignment suggestions: only enclosure_diff readers with a
// non-empty enclosure_id count, dedup'd and sorted; counter_scan readers and
// empty ids are ignored.
func TestEnclosureIDs(t *testing.T) {
	cfg := RFIDConfig{Readers: readers(map[string]RFIDReaderConfig{
		"counter": {Mode: RFIDModeCounterScan},
		"cab-b":   {Mode: RFIDModeEnclosureDiff, EnclosureID: "BAY-B"},
		"cab-a":   {Mode: RFIDModeEnclosureDiff, EnclosureID: "BAY-A"},
		"cab-a2":  {Mode: RFIDModeEnclosureDiff, EnclosureID: "BAY-A"}, // dup
		"cab-x":   {Mode: RFIDModeEnclosureDiff, EnclosureID: ""},      // empty ignored
	})}
	got := cfg.EnclosureIDs()
	want := []string{"BAY-A", "BAY-B"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("EnclosureIDs() = %v; want %v", got, want)
	}

	if ids := (RFIDConfig{}).EnclosureIDs(); len(ids) != 0 {
		t.Errorf("empty config EnclosureIDs() = %v; want none", ids)
	}
}
