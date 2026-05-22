// Package config loads the kiosk's runtime configuration from a YAML file
// and applies environment-variable overrides. Env var names follow the rule:
// prefix with KIOSK_, replace dots with underscores, uppercase.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Kiosk      KioskConfig      `yaml:"kiosk"`
	Server     ServerConfig     `yaml:"server"`
	Session    SessionConfig    `yaml:"session"`
	Scanning   ScanningConfig   `yaml:"scanning"`
	Returns    ReturnsConfig    `yaml:"returns"`
	Branding   BrandingConfig   `yaml:"branding"`
	NATS       NATSConfig       `yaml:"nats"`
	Controller ControllerConfig `yaml:"controller"`
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

// ReturnsConfig controls which return scenarios the commit hook will accept.
// Pointers (not plain bools) so an omitted YAML key is distinguishable from
// an explicit `false` — omission falls back to the permissive default rather
// than silently enabling strict mode for kiosks that pre-date this config.
type ReturnsConfig struct {
	AllowCrossUser    *bool `yaml:"allow_cross_user"`
	AllowUncorrelated *bool `yaml:"allow_uncorrelated"`
}

// CrossUserAllowed reports whether a return of a tool checked out to another
// worker is permitted. Default (unset) is true.
func (r ReturnsConfig) CrossUserAllowed() bool {
	if r.AllowCrossUser == nil {
		return true
	}
	return *r.AllowCrossUser
}

// UncorrelatedAllowed reports whether a return that doesn't match any open
// checkout is permitted. Default (unset) is true.
func (r ReturnsConfig) UncorrelatedAllowed() bool {
	if r.AllowUncorrelated == nil {
		return true
	}
	return *r.AllowUncorrelated
}

// NATSConfig drives the optional NATS publisher in internal/events. When
// disabled (the default), events.Publish stays slog-only — same as v1.
// When enabled, every event also publishes to NATS using whatever auth
// fields are provided; leave them blank for anonymous access.
//
// Auth modes (use one — they compose in the order shown if multiple are
// set, but the typical deployment picks exactly one):
//
//   - token            simple bearer token (`nats-server -auth token`)
//   - username/password   basic auth
//   - credentials_file    JWT .creds file (NGS / JetStream Cloud)
//   - nkey_seed_file      ed25519 NKey seed
//
// TLS is auto-negotiated when URL starts with tls:// or nats+tls://; you
// can also configure CA/cert/key paths explicitly. TLSInsecure is for dev.
type NATSConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`

	// SubjectPrefix is the leading namespace for every event published by
	// the kiosk (e.g. "kiosk.<code>.transaction.complete"). Override only
	// when sharing a NATS cluster with another application that already
	// owns the "kiosk.>" subject space. Both kiosk and controller must
	// agree on the value. Empty → events.DefaultSubjectPrefix.
	SubjectPrefix string `yaml:"subject_prefix"`

	// StreamName is the JetStream stream the controller binds and consumes.
	// Controller-only — the kiosk publishes to subjects, not streams.
	// Override only to avoid collision with another stream that already
	// owns the same subject space on a shared cluster. Empty →
	// events.DefaultStreamName.
	StreamName string `yaml:"stream_name"`

	Token           string `yaml:"token"`
	Username        string `yaml:"username"`
	Password        string `yaml:"password"`
	CredentialsFile string `yaml:"credentials_file"`
	NKeySeedFile    string `yaml:"nkey_seed_file"`
	TLSCAFile       string `yaml:"tls_ca_file"`
	TLSCertFile     string `yaml:"tls_cert_file"`
	TLSKeyFile      string `yaml:"tls_key_file"`
	TLSInsecure     bool   `yaml:"tls_insecure"`
}

// ControllerConfig opts a kiosk into "managed" mode. When enabled, the kiosk
// watches the JetStream KV buckets named below for catalog updates pushed
// down by the central kiosk-controller and projects them into its local
// items/users tables. The admin SPA hides catalog mutation affordances so
// edits go through the controller instead.
//
// Disabled by default — a kiosk with controller.enabled=false continues to
// behave as a standalone v1 kiosk regardless of whether NATS is on.
type ControllerConfig struct {
	Enabled             bool   `yaml:"enabled"`
	CatalogItemsBucket  string `yaml:"catalog_items_bucket"`
	CatalogUsersBucket  string `yaml:"catalog_users_bucket"`
	CatalogGroupsBucket string `yaml:"catalog_groups_bucket"`
}

// BrandingConfig customizes the kiosk's visual identity. All fields are
// optional; empty/missing values fall back to the SPA's built-in defaults.
//
//   - LogoPath: absolute or working-dir-relative path to an image file the
//     binary will stream at GET /branding/logo. Suggested formats: PNG, SVG.
//   - Tagline: shown under the logo on the idle "Scan your badge" screen.
//   - PrimaryColor: CSS color string ("#10b981", "rgb(...)", named color).
//     Applied to the commit button and other primary action accents.
type BrandingConfig struct {
	LogoPath     string `yaml:"logo_path"`
	Tagline      string `yaml:"tagline"`
	PrimaryColor string `yaml:"primary_color"`
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
	resolvePaths(&c, path)
	if err := validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolvePaths rewrites any file-path config values that are relative so they
// resolve against the directory containing the loaded config file rather than
// the process CWD. Without this, `./branding/logo.svg` only works when the
// operator happens to launch the binary from the same directory the yaml
// lives in — a common footgun for systemd units and `cd /tmp && kiosk-app`.
func resolvePaths(c *Config, configFile string) {
	base, err := filepath.Abs(filepath.Dir(configFile))
	if err != nil {
		return
	}
	if p := c.Branding.LogoPath; p != "" && !filepath.IsAbs(p) {
		c.Branding.LogoPath = filepath.Join(base, p)
	}
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
		b := parseBool(v)
		c.Returns.AllowCrossUser = &b
	}
	if v := os.Getenv("KIOSK_RETURNS_ALLOW_UNCORRELATED"); v != "" {
		b := parseBool(v)
		c.Returns.AllowUncorrelated = &b
	}
	if v := os.Getenv("KIOSK_BRANDING_LOGO_PATH"); v != "" {
		c.Branding.LogoPath = v
	}
	if v := os.Getenv("KIOSK_BRANDING_TAGLINE"); v != "" {
		c.Branding.Tagline = v
	}
	if v := os.Getenv("KIOSK_BRANDING_PRIMARY_COLOR"); v != "" {
		c.Branding.PrimaryColor = v
	}
	if v := os.Getenv("KIOSK_NATS_ENABLED"); v != "" {
		c.NATS.Enabled = parseBool(v)
	}
	if v := os.Getenv("KIOSK_NATS_URL"); v != "" {
		c.NATS.URL = v
	}
	if v := os.Getenv("KIOSK_NATS_SUBJECT_PREFIX"); v != "" {
		c.NATS.SubjectPrefix = v
	}
	if v := os.Getenv("KIOSK_NATS_STREAM_NAME"); v != "" {
		c.NATS.StreamName = v
	}
	if v := os.Getenv("KIOSK_NATS_TOKEN"); v != "" {
		c.NATS.Token = v
	}
	if v := os.Getenv("KIOSK_NATS_USERNAME"); v != "" {
		c.NATS.Username = v
	}
	if v := os.Getenv("KIOSK_NATS_PASSWORD"); v != "" {
		c.NATS.Password = v
	}
	if v := os.Getenv("KIOSK_NATS_CREDENTIALS_FILE"); v != "" {
		c.NATS.CredentialsFile = v
	}
	if v := os.Getenv("KIOSK_NATS_NKEY_SEED_FILE"); v != "" {
		c.NATS.NKeySeedFile = v
	}
	if v := os.Getenv("KIOSK_NATS_TLS_CA_FILE"); v != "" {
		c.NATS.TLSCAFile = v
	}
	if v := os.Getenv("KIOSK_NATS_TLS_CERT_FILE"); v != "" {
		c.NATS.TLSCertFile = v
	}
	if v := os.Getenv("KIOSK_NATS_TLS_KEY_FILE"); v != "" {
		c.NATS.TLSKeyFile = v
	}
	if v := os.Getenv("KIOSK_NATS_TLS_INSECURE"); v != "" {
		c.NATS.TLSInsecure = parseBool(v)
	}
	if v := os.Getenv("KIOSK_CONTROLLER_ENABLED"); v != "" {
		c.Controller.Enabled = parseBool(v)
	}
	if v := os.Getenv("KIOSK_CONTROLLER_CATALOG_ITEMS_BUCKET"); v != "" {
		c.Controller.CatalogItemsBucket = v
	}
	if v := os.Getenv("KIOSK_CONTROLLER_CATALOG_USERS_BUCKET"); v != "" {
		c.Controller.CatalogUsersBucket = v
	}
	if v := os.Getenv("KIOSK_CONTROLLER_CATALOG_GROUPS_BUCKET"); v != "" {
		c.Controller.CatalogGroupsBucket = v
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
	// The controller binary uses the same Config struct but doesn't have a
	// kiosk identity — it's the central aggregator, not a kiosk. cmd/controller
	// sets KIOSK_ROLE=controller before Load runs.
	if os.Getenv("KIOSK_ROLE") != "controller" {
		if c.Kiosk.Code == "" {
			return fmt.Errorf("kiosk.code is required")
		}
		if c.Kiosk.LocationCode == "" {
			return fmt.Errorf("kiosk.location_code is required")
		}
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8090
	}
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1"
	}
	return nil
}
