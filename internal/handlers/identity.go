package handlers

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// Identity returns the kiosk's stable identity (kiosk_code + location_code).
// Loaded once from config at startup; the SPA fetches this on boot.
func (h *Handlers) Identity(re *core.RequestEvent) error {
	return re.JSON(http.StatusOK, kioskctx.Get())
}
