package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateRFID covers the cross-field invariants on the RFID
// config block. The cases mirror docs/rfid.md: when disabled,
// everything is irrelevant; when enabled, mode + reader endpoint are
// required; enclosure_diff additionally requires enclosure_id;
// read_window defaults to 3s.
func TestValidateRFID(t *testing.T) {
	cases := []struct {
		name      string
		in        RFIDConfig
		wantErr   string // substring; empty = expect success
		wantApply func(t *testing.T, out RFIDConfig)
	}{
		{
			name: "disabled — everything else ignored",
			in:   RFIDConfig{Enabled: false, Mode: "bogus"},
		},
		{
			name: "enabled, counter_scan, full config — ok, no defaults applied to set fields",
			in: RFIDConfig{
				Enabled:    true,
				Mode:       RFIDModeCounterScan,
				Reader:     RFIDReaderConfig{Host: "10.0.0.50", Port: 5084},
				ReadWindow: Duration(5 * time.Second),
			},
			wantApply: func(t *testing.T, out RFIDConfig) {
				if out.ReadWindow.AsDuration() != 5*time.Second {
					t.Errorf("read_window should be preserved at 5s, got %v", out.ReadWindow.AsDuration())
				}
			},
		},
		{
			name: "enabled, counter_scan, read_window unset — defaults to 3s",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeCounterScan,
				Reader:  RFIDReaderConfig{Host: "10.0.0.50", Port: 5084},
			},
			wantApply: func(t *testing.T, out RFIDConfig) {
				if out.ReadWindow.AsDuration() != 3*time.Second {
					t.Errorf("read_window should default to 3s, got %v", out.ReadWindow.AsDuration())
				}
			},
		},
		{
			name: "enabled, enclosure_diff with enclosure_id — ok",
			in: RFIDConfig{
				Enabled:     true,
				Mode:        RFIDModeEnclosureDiff,
				Reader:      RFIDReaderConfig{Host: "10.0.0.50", Port: 5084},
				EnclosureID: "BAY-A",
			},
		},
		{
			name: "enclosure_diff read_window over cap — error (would blow the 5s reply window)",
			in: RFIDConfig{
				Enabled:     true,
				Mode:        RFIDModeEnclosureDiff,
				Reader:      RFIDReaderConfig{Host: "h", Port: 5084},
				EnclosureID: "BAY-A",
				ReadWindow:  Duration(5 * time.Second),
			},
			wantErr: "too long for",
		},
		{
			name: "enclosure_diff read_window at cap — ok",
			in: RFIDConfig{
				Enabled:     true,
				Mode:        RFIDModeEnclosureDiff,
				Reader:      RFIDReaderConfig{Host: "h", Port: 5084},
				EnclosureID: "BAY-A",
				ReadWindow:  Duration(MaxEnclosureReadWindow),
			},
			wantApply: func(t *testing.T, out RFIDConfig) {
				if out.ReadWindow.AsDuration() != MaxEnclosureReadWindow {
					t.Errorf("read_window at cap should be preserved, got %v", out.ReadWindow.AsDuration())
				}
			},
		},
		{
			name: "counter_scan long read_window — ok (HTTP-driven, not bound by the 5s reply)",
			in: RFIDConfig{
				Enabled:    true,
				Mode:       RFIDModeCounterScan,
				Reader:     RFIDReaderConfig{Host: "h", Port: 5084},
				ReadWindow: Duration(10 * time.Second),
			},
		},
		{
			name:    "enabled with no mode — error",
			in:      RFIDConfig{Enabled: true, Reader: RFIDReaderConfig{Host: "h", Port: 5084}},
			wantErr: "rfid.mode is required",
		},
		{
			name:    "enabled with unknown mode — error",
			in:      RFIDConfig{Enabled: true, Mode: "bulk_scan", Reader: RFIDReaderConfig{Host: "h", Port: 5084}},
			wantErr: `rfid.mode must be "counter_scan" or "enclosure_diff"`,
		},
		{
			name:    "enabled with no host — error",
			in:      RFIDConfig{Enabled: true, Mode: RFIDModeCounterScan, Reader: RFIDReaderConfig{Port: 5084}},
			wantErr: "rfid.reader.host is required",
		},
		{
			name:    "enabled with no port — error",
			in:      RFIDConfig{Enabled: true, Mode: RFIDModeCounterScan, Reader: RFIDReaderConfig{Host: "h"}},
			wantErr: "rfid.reader.port is required",
		},
		{
			name: "enabled enclosure_diff without enclosure_id — error",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeEnclosureDiff,
				Reader:  RFIDReaderConfig{Host: "h", Port: 5084},
			},
			wantErr: "rfid.enclosure_id is required",
		},
		{
			name: "counter_scan without enclosure_id — ok (enclosure_id is enclosure-only)",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeCounterScan,
				Reader:  RFIDReaderConfig{Host: "h", Port: 5084},
			},
		},
		{
			name: "antennas list — multiple ports, distinct ids and powers — ok",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeCounterScan,
				Reader: RFIDReaderConfig{
					Host: "h", Port: 5084,
					Antennas: []RFIDAntennaConfig{
						{ID: 1, TxPowerDBm: 25.0},
						{ID: 3, TxPowerDBm: 20.5},
					},
				},
			},
		},
		{
			name: "antennas list — zero id — error",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeCounterScan,
				Reader: RFIDReaderConfig{
					Host: "h", Port: 5084,
					Antennas: []RFIDAntennaConfig{{ID: 0, TxPowerDBm: 25.0}},
				},
			},
			wantErr: "rfid.reader.antennas[0].id must be >= 1",
		},
		{
			name: "antennas list — negative id — error",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeCounterScan,
				Reader: RFIDReaderConfig{
					Host: "h", Port: 5084,
					Antennas: []RFIDAntennaConfig{{ID: -1, TxPowerDBm: 25.0}},
				},
			},
			wantErr: "rfid.reader.antennas[0].id must be >= 1",
		},
		{
			name: "antennas list — duplicate id — error",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeCounterScan,
				Reader: RFIDReaderConfig{
					Host: "h", Port: 5084,
					Antennas: []RFIDAntennaConfig{
						{ID: 1, TxPowerDBm: 25.0},
						{ID: 1, TxPowerDBm: 20.0},
					},
				},
			},
			wantErr: "duplicate id 1",
		},
		{
			name: "antennas list — non-positive tx_power_dbm — error",
			in: RFIDConfig{
				Enabled: true,
				Mode:    RFIDModeCounterScan,
				Reader: RFIDReaderConfig{
					Host: "h", Port: 5084,
					Antennas: []RFIDAntennaConfig{{ID: 1, TxPowerDBm: 0}},
				},
			},
			wantErr: "rfid.reader.antennas[0].tx_power_dbm must be > 0",
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

// TestRFIDEnvOverrides verifies KIOSK_RFID_* env vars override file
// values. Each override is tested in isolation so a regression in one
// override path is easy to localize. We use t.Setenv so the test
// harness restores the prior environment automatically.
func TestRFIDEnvOverrides(t *testing.T) {
	t.Run("KIOSK_RFID_ENABLED=true flips disabled to enabled", func(t *testing.T) {
		c := &Config{}
		t.Setenv("KIOSK_RFID_ENABLED", "true")
		applyEnvOverrides(c)
		if !c.RFID.Enabled {
			t.Errorf("expected Enabled=true, got false")
		}
	})

	t.Run("KIOSK_RFID_MODE sets mode", func(t *testing.T) {
		c := &Config{}
		t.Setenv("KIOSK_RFID_MODE", RFIDModeEnclosureDiff)
		applyEnvOverrides(c)
		if c.RFID.Mode != RFIDModeEnclosureDiff {
			t.Errorf("expected Mode=%q, got %q", RFIDModeEnclosureDiff, c.RFID.Mode)
		}
	})

	t.Run("KIOSK_RFID_READER_HOST sets host", func(t *testing.T) {
		c := &Config{}
		t.Setenv("KIOSK_RFID_READER_HOST", "192.168.1.50")
		applyEnvOverrides(c)
		if c.RFID.Reader.Host != "192.168.1.50" {
			t.Errorf("expected Host=192.168.1.50, got %q", c.RFID.Reader.Host)
		}
	})

	t.Run("KIOSK_RFID_READER_PORT sets port", func(t *testing.T) {
		c := &Config{}
		t.Setenv("KIOSK_RFID_READER_PORT", "5084")
		applyEnvOverrides(c)
		if c.RFID.Reader.Port != 5084 {
			t.Errorf("expected Port=5084, got %d", c.RFID.Reader.Port)
		}
	})

	t.Run("KIOSK_RFID_READER_PORT garbage is silently ignored (matches existing pattern)", func(t *testing.T) {
		c := &Config{RFID: RFIDConfig{Reader: RFIDReaderConfig{Port: 9999}}}
		t.Setenv("KIOSK_RFID_READER_PORT", "not-a-number")
		applyEnvOverrides(c)
		// Matches how KIOSK_SERVER_PORT etc. handle bad input: leave the
		// existing value alone rather than zeroing it. Surfacing the
		// error is config.Load's job, not applyEnvOverrides'.
		if c.RFID.Reader.Port != 9999 {
			t.Errorf("garbage port value should leave existing value alone, got %d", c.RFID.Reader.Port)
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

	t.Run("KIOSK_RFID_ENCLOSURE_ID sets enclosure id", func(t *testing.T) {
		c := &Config{}
		t.Setenv("KIOSK_RFID_ENCLOSURE_ID", "BAY-B")
		applyEnvOverrides(c)
		if c.RFID.EnclosureID != "BAY-B" {
			t.Errorf("expected EnclosureID=BAY-B, got %q", c.RFID.EnclosureID)
		}
	})
}
