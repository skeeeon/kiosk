package handlers

import (
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

type brandingPayload struct {
	// LogoURL is set to "/branding/logo" when a logo is configured. The SPA
	// falls back to a text-only header when this is empty, so the server is
	// the source of truth for "do we have a logo at all".
	LogoURL      string `json:"logo_url,omitempty"`
	Tagline      string `json:"tagline,omitempty"`
	PrimaryColor string `json:"primary_color,omitempty"`
	// CustomCSSURL is set to "/branding/custom.css" when a custom CSS file
	// is configured. The SPA injects a <link rel="stylesheet"> for it after
	// Tailwind so any documented CSS variables (and, with caveats, any
	// utility class) can be overridden.
	CustomCSSURL string `json:"custom_css_url,omitempty"`
}

type identityPayload struct {
	// Role tells the SPA which binary it's running against so it can route
	// the operator at boot (checkout vs admin) and gate role-specific
	// affordances. Kiosks always emit "kiosk"; the controller's analogous
	// endpoint emits "controller".
	Role string `json:"role"`
	kioskctx.Identity
	Branding brandingPayload `json:"branding"`
	// MaxQty is published so the SPA's +/- button can disable at the same
	// ceiling the server enforces, without hardcoding the constant on both
	// sides. Source of truth is cart.MaxQty.
	MaxQty int `json:"max_qty"`
	// Managed reports whether this kiosk is opted into central control —
	// catalog (items + users) is pushed down from the controller, so the
	// admin SPA hides Add/Edit/Delete/Import affordances. Local stock
	// adjustments and other kiosk-local actions remain available.
	Managed bool `json:"managed"`
	// RFIDEnabled mirrors cfg.RFID.Enabled. The SPA uses it (with
	// RFIDMode) to decide whether to render the "RFID scan" button on
	// CheckoutView in Phase 2. Always present so the frontend can
	// branch on it without re-checking for undefined.
	RFIDEnabled bool `json:"rfid_enabled"`
	// RFIDMode is the mode of the single configured reader
	// (cfg.RFID.SoleReaderMode). Empty when RFIDEnabled is false or when more
	// than one reader is configured — multi-reader terminal→reader selection
	// is wired with the terminal work. The SPA gates the counter_scan button
	// on it.
	RFIDMode string `json:"rfid_mode,omitempty"`
	// Timeclock flags mirror cfg.Timeclock. Enabled gates the splash-screen
	// Time clock button + admin tab; the interlock flags let the SPA shape
	// copy (e.g. warn that clock-out is blocked by open tools) without
	// probing endpoints. Flat fields, matching the rfid_* precedent.
	TimeclockEnabled        bool `json:"timeclock_enabled"`
	TimeclockRequireClockIn bool `json:"timeclock_require_clock_in"`
	TimeclockBlockClockOut  bool `json:"timeclock_block_clock_out"`
	// TimeclockOnly turns the SPA into a dedicated punch station: the
	// checkout splash is replaced by a persistent punch panel and badge
	// scans route straight to it. Presentation-only — the backend surface
	// is unchanged.
	TimeclockOnly bool `json:"timeclock_only"`
	// TimeclockVirtual signals the public, per-user-authenticated terminal
	// (the cmd/timeclock binary). The SPA shows a worker login screen and,
	// once authed, the self-service punch panel wired to /api/self/timeclock/*
	// — there is no badge scan and no checkout. Only the timeclock binary
	// emits this true.
	TimeclockVirtual bool `json:"timeclock_virtual"`
}

// Identity returns the kiosk's stable identity (kiosk_code + location_code)
// plus the configured branding. The SPA fetches this once on boot.
func (h *Handlers) Identity(re *core.RequestEvent) error {
	out := identityPayload{
		Role:                    "kiosk",
		Identity:                kioskctx.Get(),
		MaxQty:                  cart.MaxQty,
		Managed:                 h.Cfg.Controller.Enabled,
		RFIDEnabled:             h.Cfg.RFID.Enabled,
		RFIDMode:                h.Cfg.RFID.SoleReaderMode(),
		TimeclockEnabled:        h.Cfg.Timeclock.Enabled,
		TimeclockRequireClockIn: h.Cfg.Timeclock.Enabled && h.Cfg.Timeclock.RequireClockInForCheckout,
		TimeclockBlockClockOut:  h.Cfg.Timeclock.Enabled && h.Cfg.Timeclock.BlockClockOutWithOpenCheckouts,
		TimeclockOnly:           h.Cfg.Timeclock.Enabled && h.Cfg.Timeclock.TimeclockOnly,
		TimeclockVirtual:        h.Cfg.Timeclock.Enabled && h.Cfg.Timeclock.Virtual,
	}
	if strings.TrimSpace(h.Cfg.Branding.LogoPath) != "" {
		out.Branding.LogoURL = "/branding/logo"
	}
	if strings.TrimSpace(h.Cfg.Branding.CustomCSSPath) != "" {
		out.Branding.CustomCSSURL = "/branding/custom.css"
	}
	out.Branding.Tagline = h.Cfg.Branding.Tagline
	out.Branding.PrimaryColor = h.Cfg.Branding.PrimaryColor
	return re.JSON(http.StatusOK, out)
}
