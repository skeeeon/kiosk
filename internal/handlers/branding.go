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
