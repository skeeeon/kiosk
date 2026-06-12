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
