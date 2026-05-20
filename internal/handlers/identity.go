package handlers

import (
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
)

type brandingPayload struct {
	// LogoURL is set to "/branding/logo" when a logo is configured. The SPA
	// falls back to a text-only header when this is empty, so the server is
	// the source of truth for "do we have a logo at all".
	LogoURL      string `json:"logo_url,omitempty"`
	Tagline      string `json:"tagline,omitempty"`
	PrimaryColor string `json:"primary_color,omitempty"`
}

type identityPayload struct {
	kioskctx.Identity
	Branding brandingPayload `json:"branding"`
}

// Identity returns the kiosk's stable identity (kiosk_code + location_code)
// plus the configured branding. The SPA fetches this once on boot.
func (h *Handlers) Identity(re *core.RequestEvent) error {
	out := identityPayload{Identity: kioskctx.Get()}
	if strings.TrimSpace(h.Cfg.Branding.LogoPath) != "" {
		out.Branding.LogoURL = "/branding/logo"
	}
	out.Branding.Tagline = h.Cfg.Branding.Tagline
	out.Branding.PrimaryColor = h.Cfg.Branding.PrimaryColor
	return re.JSON(http.StatusOK, out)
}
