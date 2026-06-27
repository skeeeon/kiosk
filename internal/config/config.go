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
// A node can host SEVERAL readers — a counter plus one or more enclosure
// cabinets — so readers are a map keyed by an operator-chosen reader_id,
// each carrying its own mode:
//
//   - counter_scan — operator hits a button on CheckoutView; the kiosk
//     runs one inventory cycle for ReadWindow and resolves observed EPCs
//     through scan.Resolver into cart lines.
//   - enclosure_diff — NATS commands (cart.start, read.trigger) drive the
//     cart from an external access-control system. A read diffs observed
//     against the expected-present set and synthesizes checkout / return
//     lines. EnclosureID names the access-controlled cabinet and is part
//     of the cart-start idempotency key (user_code, enclosure_id).
//
// A single-reader kiosk declares exactly one entry; reader selection is
// then implicit (no per-terminal selector needed). ReadWindow is shared
// across readers. Per-reader fields are YAML-only — the KIOSK_* env
// overrides cover the top-level Enabled / ReadWindow, not map entries.
//
// Connection to each reader is best-effort: a failure on startup logs a
// warning and the binary continues — mirrors NATS unreachability
// handling. RFID endpoints/commands reply with errors until a reader
// connects.
type RFIDConfig struct {
	Enabled    bool                        `yaml:"enabled"`
	ReadWindow Duration                    `yaml:"read_window"`
	Readers    map[string]RFIDReaderConfig `yaml:"readers"`
}

// SoleReaderMode returns the mode of the single configured reader, or "" when
// zero or more than one reader is configured. The SPA's single rfid_mode hint
// (which gates the counter_scan button) is only meaningful at one reader;
// multi-reader terminal→reader selection arrives with the terminal work.
func (c RFIDConfig) SoleReaderMode() string {
	if len(c.Readers) != 1 {
		return ""
	}
	for _, r := range c.Readers {
		return r.Mode
	}
	return ""
}

// RFIDReaderConfig is one physical reader's config. Mode and EnclosureID are
// per-reader (moved off the top level) so one node can mix a counter and
// enclosure cabinets.
type RFIDReaderConfig struct {
	Mode        string `yaml:"mode"` // "counter_scan" | "enclosure_diff"
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	EnclosureID string `yaml:"enclosure_id"` // required when mode=enclosure_diff

	// Zone is an optional coarse location label for this reader. When set, a
	// custody read at this reader also stamps every observed unit's advisory
	// last-observed location at this zone (docs/location-sightings-plan.md, L1)
	// — free location data from custody activity. Empty = no location stamping
	// (N=1 invisible).
	Zone string `yaml:"zone"`

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
//     clock-out while the worker has open checkouts. The funnel merges THIS
//     kiosk's local open_checkouts with the fleet-wide replica (the
//     controller-written open_checkouts_state bucket, partitioned by
//     kiosk_code), so in managed mode the gate sees tools out at OTHER kiosks
//     too. Without a controller (standalone) or with KV unavailable it
//     degrades to local-only — fail-open for the cross-kiosk portion. Admin
//     force=true bypasses it; a self/foreman force=true is the worker's
//     "clock out anyway" acknowledgment (same column, told apart by source).
//   - TimeclockOnly turns the device into a dedicated punch station: the
//     SPA replaces the checkout splash with a persistent punch panel and
//     badge scans go straight to it — no carts, no checkout. Backend
//     surface is unchanged (the kiosk box is the trust boundary); this is
//     a presentation mode. Requires Enabled. A punch-only station never
//     writes its own open_checkouts, so BlockClockOutWithOpenCheckouts has
//     nothing LOCAL to block on — but in managed mode it still blocks on the
//     fleet replica (tools the worker has out at other kiosks). Standalone,
//     it's a no-op (no replica).
//   - Virtual marks the dedicated cmd/timeclock binary: a publicly-hosted,
//     per-user-authenticated self-service punch terminal (workers clock in/
//     out from their phones). Unlike every other kiosk the trust boundary is
//     the authenticated `users` session, not the box, so the binary registers
//     ONLY the authed /api/self/timeclock/* surface — none of the anonymous
//     checkout endpoints exist. The flag is presentation+wiring intent surfaced
//     to the SPA as timeclock_virtual; it requires Enabled and managed mode
//     (controller + NATS) because punches reach the fleet only via the
//     controller's punch_state broadcast and workers are sourced from the
//     catalog_users watcher.
type TimeclockConfig struct {
	Enabled                        bool `yaml:"enabled"`
	RequireClockInForCheckout      bool `yaml:"require_clock_in_for_checkout"`
	BlockClockOutWithOpenCheckouts bool `yaml:"block_clock_out_with_open_checkouts"`
	TimeclockOnly                  bool `yaml:"timeclock_only"`
	Virtual                        bool `yaml:"virtual"`
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
	// Per-reader RFID fields (mode/host/port/enclosure_id/antennas) live in
	// the rfid.readers map and are YAML-only — there's no flat env path into a
	// map entry. Only the top-level toggles get env overrides.
	if v := os.Getenv("KIOSK_RFID_READ_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.RFID.ReadWindow = Duration(d)
		} else {
			slog.Warn("config.env_override_ignored", "var", "KIOSK_RFID_READ_WINDOW", "value", v, "error", err)
		}
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
	if v := os.Getenv("KIOSK_TIMECLOCK_TIMECLOCK_ONLY"); v != "" {
		c.Timeclock.TimeclockOnly = parseBool(v)
	}
	if v := os.Getenv("KIOSK_TIMECLOCK_VIRTUAL"); v != "" {
		c.Timeclock.Virtual = parseBool(v)
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
	if c.Timeclock.TimeclockOnly && !c.Timeclock.Enabled {
		return fmt.Errorf("timeclock.timeclock_only requires timeclock.enabled=true")
	}
	// The virtual terminal supports the same three modes as a physical kiosk:
	// standalone (local punch ledger only), standalone + NATS event publishing,
	// and controller-managed. Only timeclock.enabled is a hard requirement; the
	// NATS/controller wiring degrades gracefully exactly like cmd/kiosk. In the
	// unmanaged modes workers are provisioned locally (admin SPA / superuser /
	// CSV) and clocked-in state is local-only; managed mode adds catalog-synced
	// workers and the fleet-wide punch_state replica.
	if c.Timeclock.Virtual && !c.Timeclock.Enabled {
		return fmt.Errorf("timeclock.virtual requires timeclock.enabled=true")
	}
	return nil
}

// validateRFID enforces the cross-field invariants for the RFID block.
// When disabled, everything below it is irrelevant; when enabled, at least
// one reader is required and each reader needs a valid mode + endpoint;
// enclosure_diff readers additionally require an enclosure_id. ReadWindow is
// shared, defaults to 3s, and is capped at MaxEnclosureReadWindow whenever any
// reader runs enclosure_diff (the read rides a ~5s NATS command reply).
func validateRFID(r *RFIDConfig) error {
	if !r.Enabled {
		return nil
	}
	if len(r.Readers) == 0 {
		return fmt.Errorf("rfid.readers must have at least one entry when rfid.enabled=true")
	}
	anyEnclosure := false
	for id, rd := range r.Readers {
		if id == "" {
			return fmt.Errorf("rfid.readers has an entry with an empty reader id")
		}
		switch rd.Mode {
		case RFIDModeCounterScan, RFIDModeEnclosureDiff:
			// ok
		case "":
			return fmt.Errorf("rfid.readers[%q].mode is required", id)
		default:
			return fmt.Errorf("rfid.readers[%q].mode must be %q or %q (got %q)",
				id, RFIDModeCounterScan, RFIDModeEnclosureDiff, rd.Mode)
		}
		if rd.Host == "" {
			return fmt.Errorf("rfid.readers[%q].host is required", id)
		}
		if rd.Port == 0 {
			return fmt.Errorf("rfid.readers[%q].port is required", id)
		}
		if rd.Mode == RFIDModeEnclosureDiff {
			anyEnclosure = true
			if rd.EnclosureID == "" {
				return fmt.Errorf("rfid.readers[%q].enclosure_id is required when mode=%q", id, RFIDModeEnclosureDiff)
			}
		}
		seen := make(map[int]struct{}, len(rd.Antennas))
		for i, a := range rd.Antennas {
			if a.ID <= 0 {
				return fmt.Errorf("rfid.readers[%q].antennas[%d].id must be >= 1 (got %d)", id, i, a.ID)
			}
			if _, dup := seen[a.ID]; dup {
				return fmt.Errorf("rfid.readers[%q].antennas: duplicate id %d", id, a.ID)
			}
			seen[a.ID] = struct{}{}
			if a.TxPowerDBm <= 0 {
				return fmt.Errorf("rfid.readers[%q].antennas[%d].tx_power_dbm must be > 0 (got %g)", id, i, a.TxPowerDBm)
			}
		}
	}
	// enclosure_diff runs the read synchronously inside a NATS request/reply
	// bounded by the controller's ~5s command timeout, so cap the shared
	// read_window with headroom whenever any reader runs that mode. A
	// counter_scan-only node is HTTP-driven and not subject to the 5s reply.
	if r.ReadWindow.AsDuration() == 0 {
		r.ReadWindow = Duration(3 * time.Second)
	}
	if anyEnclosure && r.ReadWindow.AsDuration() > MaxEnclosureReadWindow {
		return fmt.Errorf("rfid.read_window %s is too long with an enclosure_diff reader (max %s; the read runs inside a ~5s command reply window)",
			r.ReadWindow.AsDuration(), MaxEnclosureReadWindow)
	}
	return nil
}
