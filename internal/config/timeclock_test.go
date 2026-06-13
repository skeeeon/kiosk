package config

import "testing"

func TestValidateTimeclockOnly(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Kiosk.Code = "K1"
		c.Kiosk.LocationCode = "L1"
		return c
	}

	c := base()
	c.Timeclock.TimeclockOnly = true
	if err := validate(c); err == nil {
		t.Fatal("timeclock_only without timeclock.enabled must fail validation")
	}

	c = base()
	c.Timeclock.Enabled = true
	c.Timeclock.TimeclockOnly = true
	if err := validate(c); err != nil {
		t.Fatalf("timeclock_only with enabled should pass: %v", err)
	}
}

func TestValidateTimeclockVirtual(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Kiosk.Code = "K1"
		c.Kiosk.LocationCode = "L1"
		return c
	}

	// virtual requires enabled.
	c := base()
	c.Timeclock.Virtual = true
	if err := validate(c); err == nil {
		t.Fatal("timeclock.virtual without timeclock.enabled must fail validation")
	}

	// virtual with only enabled is valid — standalone (mode 1), no NATS/controller.
	c = base()
	c.Timeclock.Enabled = true
	c.Timeclock.Virtual = true
	if err := validate(c); err != nil {
		t.Fatalf("virtual standalone should pass (no NATS/controller required): %v", err)
	}

	// virtual + NATS + controller (mode 3) is valid too.
	c = base()
	c.Timeclock.Enabled = true
	c.Timeclock.Virtual = true
	c.NATS.Enabled = true
	c.Controller.Enabled = true
	if err := validate(c); err != nil {
		t.Fatalf("virtual managed should pass: %v", err)
	}
}
