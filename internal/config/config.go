// Package config loads the kiosk's runtime configuration from a YAML file
// and applies environment-variable overrides. Env var names follow the rule:
// prefix with KIOSK_, replace dots with underscores, uppercase.
package config

import (
	"fmt"
	"log/slog"
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
	RFID       RFIDConfig       `yaml:"rfid"`
	Timeclock  TimeclockConfig  `yaml:"timeclock"`
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

// RFIDConfig opts a kiosk into RFID-driven inventory flows. See
// docs/rfid.md for the full design. Disabled by default; when off the
// binary behaves exactly as it does without RFID hardware.
//
// Two modes:
//
//   - counter_scan — operator hits a button on CheckoutView, kiosk runs
//     a single inventory cycle for the configured ReadWindow, observed
//     EPCs resolve through scan.Resolver into cart lines.
//   - enclosure_diff — NATS commands (cart.start, read.trigger) drive
//     the cart from an external access-control / occupancy system. A
//     read computes a diff against the kiosk's expected-present set
//     and synthesizes checkout / return lines accordingly. DoorID is
//     the opaque label used as part of the cart-start idempotency key
//     (user_code, door_id), so two enclosures sharing a kiosk can be
//     disambiguated.
//
// Connection to the reader is best-effort: failure on startup logs a
// warning and the binary continues — mirrors NATS unreachability
// handling. RFID endpoints/commands will reply with errors until the
// connection comes up.
type RFIDConfig struct {
	Enabled    bool             `yaml:"enabled"`
	Mode       string           `yaml:"mode"`
	Reader     RFIDReaderConfig `yaml:"reader"`
	ReadWindow Duration         `yaml:"read_window"`
	DoorID     string           `yaml:"door_id"`
}

type RFIDReaderConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	// Antennas enumerates the reader's active antenna ports and the TX
	// power each one should run at. Empty list means "leave the reader's
	// own baseline alone" — useful for sites that prefer to provision
	// via the reader's web UI / IoT REST. Non-empty list means the kiosk
	// owns tuning: only the listed ports are inventoried, each at the
	// given dBm.
	//
	// Power is specified in dBm because it's human-meaningful; the LLRP
	// wire wants a 1-based index into a reader-specific power table, so
	// the kiosk resolves dBm → nearest achievable index at Connect time
	// via GET_READER_CAPABILITIES. The actual ceiling is whatever the
	// reader's regulatory region permits; we don't try to enforce that
	// statically.
	//
	// Per-antenna power (rather than one global value) because room
	// geometry varies — an overhead antenna often runs lower than a
	// side-mount one in the same cabinet to avoid RF bleed.
	Antennas []RFIDAntennaConfig `yaml:"antennas"`
}

// RFIDAntennaConfig pairs a reader antenna port with its TX power. ID
// is the 1-based port number on the reader (1–4 for an R700). Duplicate
// IDs are rejected at validation; an ID outside the reader's actual
// capability is rejected at Connect when capabilities come back.
type RFIDAntennaConfig struct {
	ID         int     `yaml:"id"`
	TxPowerDBm float64 `yaml:"tx_power_dbm"`
}

// TimeclockConfig opts a kiosk into the timeclock feature: an append-only
// clock-in/clock-out punch ledger (time_punches) with optional interlocks
// against the tool ledger. Plain bools (not the ReturnsConfig pointer trick)
// because every default is false — an omitted block leaves existing
// deployments exactly as they were.
//
//   - Enabled gates the whole surface: HTTP endpoints, the splash-screen
//     button, event publishing, and (in managed mode) the punch-state
//     watcher. Off → the feature does not exist.
//   - RequireClockInForCheckout makes commit reject any cart containing
//     checkout/consume lines when the cart user is not clocked in. Returns
//     are ALWAYS allowed — a worker holding a tool can hand it back
//     regardless of punch state.
//   - BlockClockOutWithOpenCheckouts makes the punch funnel reject a
//     clock-out while the worker has open checkouts at THIS kiosk
//     (local-scoped by design). Admin punches with force=true bypass it.
type TimeclockConfig struct {
	Enabled                        bool `yaml:"enabled"`
	RequireClockInForCheckout      bool `yaml:"require_clock_in_for_checkout"`
	BlockClockOutWithOpenCheckouts bool `yaml:"block_clock_out_with_open_checkouts"`
}

// Valid RFID mode strings. The set is fixed; new modes get a new
// constant and a switch arm in validate().
const (
	RFIDModeCounterScan   = "counter_scan"
	RFIDModeEnclosureDiff = "enclosure_diff"
)

// MaxEnclosureReadWindow caps rfid.read_window in enclosure_diff mode. The
// read runs synchronously inside a NATS request/reply whose caller (the
// controller) times out at ~5s; the window must leave headroom for the LLRP
// AddROSpec/EnableROSpec round-trips, the reconciliation queries, and the
// command-level deadline (commands.ReadTriggerBudget) that releases the
// reader lock. Keep this comfortably below that budget.
const MaxEnclosureReadWindow = 3500 * time.Millisecond

// BrandingConfig customizes the kiosk's visual identity. All fields are
// optional; empty/missing values fall back to the SPA's built-in defaults.
//
//   - LogoPath: path to an image file the binary will stream at
//     GET /branding/logo. Relative paths resolve against the config file's
//     directory (see resolvePaths). Suggested formats: PNG, SVG.
//   - Tagline: shown under the logo on the idle "Scan your badge" screen.
//   - PrimaryColor: CSS color string ("#10b981", "rgb(...)", named color).
//     Applied to the commit button and other primary action accents.
//   - CustomCSSPath: path (resolved like LogoPath) to a .css file
//     the binary will stream at GET /branding/custom.css. The SPA injects a
//     <link> for it after Tailwind so any color utility can be re-skinned
//     by overriding the matching `--color-<name>` variable on :root —
//     Tailwind 4 emits every utility as a var() reference. See README's
//     "Branding → Custom CSS overrides" section and
//     branding/theme.css.example for working examples.
type BrandingConfig struct {
	LogoPath      string `yaml:"logo_path"`
	Tagline       string `yaml:"tagline"`
	PrimaryColor  string `yaml:"primary_color"`
	CustomCSSPath string `yaml:"custom_css_path"`
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
	if p := c.Branding.CustomCSSPath; p != "" && !filepath.IsAbs(p) {
		c.Branding.CustomCSSPath = filepath.Join(base, p)
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
		} else {
			slog.Warn("config.env_override_ignored", "var", "KIOSK_SERVER_PORT", "value", v, "error", err)
		}
	}
	if v := os.Getenv("KIOSK_SERVER_BIND"); v != "" {
		c.Server.Bind = v
	}
	if v := os.Getenv("KIOSK_SESSION_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Session.IdleTimeout = Duration(d)
		} else {
			slog.Warn("config.env_override_ignored", "var", "KIOSK_SESSION_IDLE_TIMEOUT", "value", v, "error", err)
		}
	}
	if v := os.Getenv("KIOSK_SESSION_CART_GRACE_PERIOD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Session.CartGracePeriod = Duration(d)
		} else {
			slog.Warn("config.env_override_ignored", "var", "KIOSK_SESSION_CART_GRACE_PERIOD", "value", v, "error", err)
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
	if v := os.Getenv("KIOSK_BRANDING_CUSTOM_CSS_PATH"); v != "" {
		c.Branding.CustomCSSPath = v
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
	if v := os.Getenv("KIOSK_RFID_ENABLED"); v != "" {
		c.RFID.Enabled = parseBool(v)
	}
	if v := os.Getenv("KIOSK_RFID_MODE"); v != "" {
		c.RFID.Mode = v
	}
	if v := os.Getenv("KIOSK_RFID_READER_HOST"); v != "" {
		c.RFID.Reader.Host = v
	}
	if v := os.Getenv("KIOSK_RFID_READER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.RFID.Reader.Port = p
		} else {
			slog.Warn("config.env_override_ignored", "var", "KIOSK_RFID_READER_PORT", "value", v, "error", err)
		}
	}
	if v := os.Getenv("KIOSK_RFID_READ_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.RFID.ReadWindow = Duration(d)
		} else {
			slog.Warn("config.env_override_ignored", "var", "KIOSK_RFID_READ_WINDOW", "value", v, "error", err)
		}
	}
	if v := os.Getenv("KIOSK_RFID_DOOR_ID"); v != "" {
		c.RFID.DoorID = v
	}
	if v := os.Getenv("KIOSK_TIMECLOCK_ENABLED"); v != "" {
		c.Timeclock.Enabled = parseBool(v)
	}
	if v := os.Getenv("KIOSK_TIMECLOCK_REQUIRE_CLOCK_IN_FOR_CHECKOUT"); v != "" {
		c.Timeclock.RequireClockInForCheckout = parseBool(v)
	}
	if v := os.Getenv("KIOSK_TIMECLOCK_BLOCK_CLOCK_OUT_WITH_OPEN_CHECKOUTS"); v != "" {
		c.Timeclock.BlockClockOutWithOpenCheckouts = parseBool(v)
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
	if err := validateRFID(&c.RFID); err != nil {
		return err
	}
	return nil
}

// validateRFID enforces the cross-field invariants for the RFID block.
// When disabled, everything below it is irrelevant; when enabled, the
// mode + reader endpoint are required, and enclosure_diff additionally
// requires a door_id (it's part of the cart-start idempotency key).
// ReadWindow defaults to 3s when unset to spare callers from spelling
// out the common case.
func validateRFID(r *RFIDConfig) error {
	if !r.Enabled {
		return nil
	}
	switch r.Mode {
	case RFIDModeCounterScan, RFIDModeEnclosureDiff:
		// ok
	case "":
		return fmt.Errorf("rfid.mode is required when rfid.enabled=true")
	default:
		return fmt.Errorf("rfid.mode must be %q or %q (got %q)",
			RFIDModeCounterScan, RFIDModeEnclosureDiff, r.Mode)
	}
	if r.Reader.Host == "" {
		return fmt.Errorf("rfid.reader.host is required when rfid.enabled=true")
	}
	if r.Reader.Port == 0 {
		return fmt.Errorf("rfid.reader.port is required when rfid.enabled=true")
	}
	if r.Mode == RFIDModeEnclosureDiff && r.DoorID == "" {
		return fmt.Errorf("rfid.door_id is required when rfid.mode=%q", RFIDModeEnclosureDiff)
	}
	if r.ReadWindow.AsDuration() == 0 {
		r.ReadWindow = Duration(3 * time.Second)
	}
	// enclosure_diff runs the read synchronously inside a NATS request/reply
	// bounded by the controller's ~5s command timeout. A read_window at or
	// near that guarantees the reply misses the window — the caller then
	// renders "kiosk offline" even though the read succeeded. Cap it with
	// headroom for the LLRP round-trips + reconciliation queries. counter_scan
	// is HTTP-driven and not subject to the 5s reply, so it isn't capped here.
	if r.Mode == RFIDModeEnclosureDiff && r.ReadWindow.AsDuration() > MaxEnclosureReadWindow {
		return fmt.Errorf("rfid.read_window %s is too long for %q (max %s; the read runs inside a ~5s command reply window)",
			r.ReadWindow.AsDuration(), RFIDModeEnclosureDiff, MaxEnclosureReadWindow)
	}
	seen := make(map[int]struct{}, len(r.Reader.Antennas))
	for i, a := range r.Reader.Antennas {
		if a.ID <= 0 {
			return fmt.Errorf("rfid.reader.antennas[%d].id must be >= 1 (got %d)", i, a.ID)
		}
		if _, dup := seen[a.ID]; dup {
			return fmt.Errorf("rfid.reader.antennas: duplicate id %d", a.ID)
		}
		seen[a.ID] = struct{}{}
		if a.TxPowerDBm <= 0 {
			return fmt.Errorf("rfid.reader.antennas[%d].tx_power_dbm must be > 0 (got %g)", i, a.TxPowerDBm)
		}
	}
	return nil
}
