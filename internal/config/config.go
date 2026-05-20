// Package config loads the kiosk's runtime configuration from a YAML file
// and applies environment-variable overrides. Env var names follow the rule:
// prefix with KIOSK_, replace dots with underscores, uppercase.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Kiosk    KioskConfig    `yaml:"kiosk"`
	Server   ServerConfig   `yaml:"server"`
	Session  SessionConfig  `yaml:"session"`
	Scanning ScanningConfig `yaml:"scanning"`
	Returns  ReturnsConfig  `yaml:"returns"`
}

type KioskConfig struct {
	Code         string `yaml:"code"`
	LocationCode string `yaml:"location_code"`
}

type ServerConfig struct {
	Port int    `yaml:"port"`
	Bind string `yaml:"bind"`
}

type SessionConfig struct {
	IdleTimeout     Duration `yaml:"idle_timeout"`
	CartGracePeriod Duration `yaml:"cart_grace_period"`
}

type ScanningConfig struct {
	UserQRPrefix      string `yaml:"user_qr_prefix"`
	ItemBarcodePrefix string `yaml:"item_barcode_prefix"`
}

type ReturnsConfig struct {
	AllowCrossUser    bool `yaml:"allow_cross_user"`
	AllowUncorrelated bool `yaml:"allow_uncorrelated"`
}

// Duration wraps time.Duration so YAML can parse "5m", "30s", etc.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) AsDuration() time.Duration { return time.Duration(d) }

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	applyEnvOverrides(&c)
	if err := validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

func applyEnvOverrides(c *Config) {
	if v := os.Getenv("KIOSK_KIOSK_CODE"); v != "" {
		c.Kiosk.Code = v
	}
	if v := os.Getenv("KIOSK_KIOSK_LOCATION_CODE"); v != "" {
		c.Kiosk.LocationCode = v
	}
	if v := os.Getenv("KIOSK_SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Server.Port = p
		}
	}
	if v := os.Getenv("KIOSK_SERVER_BIND"); v != "" {
		c.Server.Bind = v
	}
	if v := os.Getenv("KIOSK_SESSION_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Session.IdleTimeout = Duration(d)
		}
	}
	if v := os.Getenv("KIOSK_SESSION_CART_GRACE_PERIOD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Session.CartGracePeriod = Duration(d)
		}
	}
	if v := os.Getenv("KIOSK_SCANNING_USER_QR_PREFIX"); v != "" {
		c.Scanning.UserQRPrefix = v
	}
	if v := os.Getenv("KIOSK_SCANNING_ITEM_BARCODE_PREFIX"); v != "" {
		c.Scanning.ItemBarcodePrefix = v
	}
	if v := os.Getenv("KIOSK_RETURNS_ALLOW_CROSS_USER"); v != "" {
		c.Returns.AllowCrossUser = parseBool(v)
	}
	if v := os.Getenv("KIOSK_RETURNS_ALLOW_UNCORRELATED"); v != "" {
		c.Returns.AllowUncorrelated = parseBool(v)
	}
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func validate(c *Config) error {
	if c.Kiosk.Code == "" {
		return fmt.Errorf("kiosk.code is required")
	}
	if c.Kiosk.LocationCode == "" {
		return fmt.Errorf("kiosk.location_code is required")
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8090
	}
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1"
	}
	return nil
}
