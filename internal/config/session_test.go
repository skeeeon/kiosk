package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func baseKiosk() *Config {
	c := &Config{}
	c.Kiosk.Code = "K1"
	c.Kiosk.LocationCode = "L1"
	return c
}

// TestSessionDefaults — an omitted `session:` block left idle_timeout at
// zero, and cart.Store stamps ExpiresAt = now + idleTimeout. The kiosk
// booted, served the SPA, accepted a badge scan, then 404'd "Cart not
// found or expired" on the very next call, with nothing in the logs to
// say why. A kiosk that starts cleanly and cannot complete one checkout
// is the worst shape a config default can fail in.
func TestSessionDefaults(t *testing.T) {
	c := baseKiosk()
	if err := validate(c); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.Session.IdleTimeout.AsDuration(); got != 5*time.Minute {
		t.Errorf("idle_timeout = %v, want 5m — a zero here expires every cart on creation", got)
	}
	if got := c.Session.CartGracePeriod.AsDuration(); got != 30*time.Second {
		t.Errorf("cart_grace_period = %v, want 30s", got)
	}
}

// TestSessionDefaults_NonPositiveIsTreatedAsUnset covers the typo case —
// a negative duration expires carts exactly as instantly as zero does, and
// "never expire" is not a setting this product offers (carts are in-memory
// and an abandoned one belongs to whoever walks up next).
func TestSessionDefaults_NonPositiveIsTreatedAsUnset(t *testing.T) {
	c := baseKiosk()
	c.Session.IdleTimeout = Duration(-1 * time.Minute)
	c.Session.CartGracePeriod = Duration(-1 * time.Second)
	if err := validate(c); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.Session.IdleTimeout.AsDuration() != 5*time.Minute {
		t.Errorf("negative idle_timeout was not replaced: %v", c.Session.IdleTimeout.AsDuration())
	}
	if c.Session.CartGracePeriod.AsDuration() != 30*time.Second {
		t.Errorf("negative cart_grace_period was not replaced: %v", c.Session.CartGracePeriod.AsDuration())
	}
}

// TestSessionDefaults_ExplicitValueWins — defaulting must not quietly
// override an operator who asked for something unusual.
func TestSessionDefaults_ExplicitValueWins(t *testing.T) {
	c := baseKiosk()
	c.Session.IdleTimeout = Duration(90 * time.Second)
	c.Session.CartGracePeriod = Duration(2 * time.Second)
	if err := validate(c); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.Session.IdleTimeout.AsDuration(); got != 90*time.Second {
		t.Errorf("idle_timeout = %v, want the configured 90s", got)
	}
	if got := c.Session.CartGracePeriod.AsDuration(); got != 2*time.Second {
		t.Errorf("cart_grace_period = %v, want the configured 2s", got)
	}
}

// TestLoad_MinimalConfigIsUsable is the end-to-end version: the smallest
// config that passes validation must produce a kiosk that can hold a cart.
// This goes through Load rather than validate directly, so it also covers
// the YAML path.
func TestLoad_MinimalConfigIsUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kiosk.yaml")
	minimal := "kiosk:\n  code: \"K1\"\n  location_code: \"L1\"\n"
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Session.IdleTimeout.AsDuration() <= 0 {
		t.Fatal("a minimal config yields carts that expire the instant they are created")
	}
	if c.Server.Port == 0 || c.Server.Bind == "" {
		t.Errorf("server defaults not applied: %+v", c.Server)
	}
}
