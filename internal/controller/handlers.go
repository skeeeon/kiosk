package controller

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/exports"
	"github.com/skeeeon/kiosk/internal/notifications"
)

// Handlers groups the controller's HTTP endpoints. The set is deliberately
// small — only what the SPA needs that PB's REST API doesn't already cover
// (identity, branding asset, CSV exports). All admin-write CRUD goes through
// pb.collection('...') on the SPA.
type Handlers struct {
	App      core.App
	Cfg      *config.Config
	Notifier *notifications.Notifier
}

func New(app core.App, cfg *config.Config, notifier *notifications.Notifier) *Handlers {
	return &Handlers{App: app, Cfg: cfg, Notifier: notifier}
}

// requireAdmin gates writes/exports behind the admins auth collection — same
// shape as the kiosk's handlers.requireAdmin, duplicated here to avoid pulling
// in the kiosk's whole Handlers package (which depends on cart, scan, etc.
// that aren't relevant on the controller).
func (h *Handlers) requireAdmin(re *core.RequestEvent) error {
	if re.Auth == nil || re.Auth.Collection() == nil ||
		re.Auth.Collection().Name != "admins" {
		return re.ForbiddenError("admin access required", nil)
	}
	return nil
}

// identityPayload is the controller's analog of the kiosk's identity
// response. Same shape; `role` is the discriminator the SPA checks at boot.
// Kiosk-only fields (kiosk_code, location_code, max_qty, managed) are
// intentionally absent here — the SPA's TS type makes them optional.
type identityPayload struct {
	Role     string          `json:"role"`
	Branding brandingPayload `json:"branding"`
	// TimeclockEnabled mirrors cfg.Timeclock.Enabled on the controller —
	// it gates the SPA's fleet timeclock report tab and the per-kiosk
	// remote-punch affordances. Same flat-field shape as the kiosk's
	// identity payload.
	TimeclockEnabled bool `json:"timeclock_enabled"`
}

type brandingPayload struct {
	LogoURL      string `json:"logo_url,omitempty"`
	Tagline      string `json:"tagline,omitempty"`
	PrimaryColor string `json:"primary_color,omitempty"`
	CustomCSSURL string `json:"custom_css_url,omitempty"`
}

// Identity returns {role: "controller"} plus branding. The SPA fetches this
// on boot; the kiosk's analogous handler returns role: "kiosk".
func (h *Handlers) Identity(re *core.RequestEvent) error {
	out := identityPayload{Role: "controller", TimeclockEnabled: h.Cfg.Timeclock.Enabled}
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

// CustomCSS streams the configured branding.custom_css_path. Mirrors the
// kiosk-side handler (handlers/branding.go); same caveats apply. The SPA
// looks for the URL on the identity payload and only injects a <link> when
// the server reports a file is present.
func (h *Handlers) CustomCSS(re *core.RequestEvent) error {
	path := strings.TrimSpace(h.Cfg.Branding.CustomCSSPath)
	if path == "" {
		return re.NotFoundError("no custom css configured", nil)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return re.NotFoundError("custom css file not found", nil)
		}
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return re.NotFoundError("custom css path is a directory", nil)
	}

	re.Response.Header().Set("Content-Type", "text/css; charset=utf-8")
	re.Response.Header().Set("Cache-Control", "public, max-age=300")
	re.Response.WriteHeader(http.StatusOK)
	_, err = io.Copy(re.Response, f)
	return err
}

// Logo streams the configured branding.logo_path. Same behavior and 404
// semantics as the kiosk's branding.Logo — duplicated to keep the package
// boundary clean. If we end up needing the same file streaming elsewhere
// it's worth extracting; one duplicate is below that bar.
func (h *Handlers) Logo(re *core.RequestEvent) error {
	path := strings.TrimSpace(h.Cfg.Branding.LogoPath)
	if path == "" {
		return re.NotFoundError("no logo configured", nil)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return re.NotFoundError("logo file not found", nil)
		}
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return re.NotFoundError("logo path is a directory", nil)
	}

	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "application/octet-stream"
	}
	re.Response.Header().Set("Content-Type", ct)
	re.Response.Header().Set("Cache-Control", "public, max-age=300")
	re.Response.WriteHeader(http.StatusOK)
	_, err = io.Copy(re.Response, f)
	return err
}

// ItemsExportCSV streams the items collection in the same column shape the
// kiosk's importer accepts. Note: the controller's items have qty/threshold
// fields but they're meaningless cross-fleet; we still emit the columns so a
// round-trip through CSV is symmetrical with the kiosk format.
func (h *Handlers) ItemsExportCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"controller-items-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteItemsCSV(h.App, w)
}

// CatalogIntegrity returns a request handler that runs a read-only diff
// of the controller's catalog DB against the live JetStream KV buckets.
// The closure captures the CatalogPublisher (which owns the bucket
// handles) so the route can be registered from main without exposing the
// publisher on the Handlers struct.
func (h *Handlers) CatalogIntegrity(cp *CatalogPublisher) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		report, err := cp.Integrity(re.Request.Context())
		if err != nil {
			return re.InternalServerError("catalog integrity failed", err)
		}
		return re.JSON(http.StatusOK, report)
	}
}

// CatalogReconcile returns a request handler that pushes the controller's
// DB state to KV and (when delete_orphans=true in the body) removes
// KV-only keys. Idempotent — safe to re-run.
func (h *Handlers) CatalogReconcile(cp *CatalogPublisher) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if err := h.requireAdmin(re); err != nil {
			return err
		}
		var body struct {
			DeleteOrphans bool `json:"delete_orphans"`
		}
		// Empty body is allowed: defaults to push-only (the safe direction).
		_ = re.BindBody(&body)

		report, err := cp.Reconcile(re.Request.Context(), body.DeleteOrphans)
		if err != nil {
			return re.InternalServerError("catalog reconcile failed", err)
		}
		return re.JSON(http.StatusOK, report)
	}
}

// TransactionsExportCSV streams the controller's aggregated transactions
// ledger. Optional ?from= and ?to= ISO8601 query params clip to a window.
// Includes the source_kiosk_code column so downstream consumers can
// demultiplex by originating kiosk.
func (h *Handlers) TransactionsExportCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	from := re.Request.URL.Query().Get("from")
	to := re.Request.URL.Query().Get("to")
	if from != "" {
		if _, err := time.Parse(time.RFC3339, from); err != nil {
			return re.BadRequestError("from must be RFC3339 (e.g. 2026-05-01T00:00:00Z)", err)
		}
	}
	if to != "" {
		if _, err := time.Parse(time.RFC3339, to); err != nil {
			return re.BadRequestError("to must be RFC3339", err)
		}
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"controller-transactions-%s.csv\"",
		time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteTransactionsCSV(h.App, w, exports.TransactionsOptions{
		From:               from,
		To:                 to,
		IncludeSourceKiosk: true,
	})
}
