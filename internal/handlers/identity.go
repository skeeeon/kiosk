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
}

// Identity returns the kiosk's stable identity (kiosk_code + location_code)
// plus the configured branding. The SPA fetches this once on boot.
func (h *Handlers) Identity(re *core.RequestEvent) error {
	out := identityPayload{
		Role:     "kiosk",
		Identity: kioskctx.Get(),
		MaxQty:   cart.MaxQty,
		Managed:  h.Cfg.Controller.Enabled,
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
