package handlers

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// CustomCSS streams the configured branding.custom_css_path. The SPA looks
// for the URL on the identity payload and injects a <link> only when the
// server reports the file is available, so a 404 here would be a misconfig
// on the operator's side rather than a normal fallback path.
//
// Content-Type is forced to text/css regardless of the file extension; the
// file is expected to be plain CSS and the browser must parse it as such
// for the <link rel="stylesheet"> to apply.
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

// Logo streams the configured branding.logo_path. Returns 404 with no body
// when no logo is configured or the file is missing, so the SPA's <img>
// fallback (text) takes over cleanly.
//
// Anonymous: the kiosk is bound to 127.0.0.1 and the logo isn't sensitive.
// The operator chose the configured path; we resolve it as-is (CWD-relative
// if not absolute) and stream it directly.
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
