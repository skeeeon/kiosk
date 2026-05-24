// Package ui embeds the built Vue SPA so both binaries (kiosk and
// controller) ship as single self-contained artifacts. The `dist`
// directory is populated by `npm run build --prefix ui` and is
// gitignored — `go build` will fail with an embed error if it is
// missing or empty, which is the intended forcing function.
//
// Branding overrides (logo, custom CSS) remain on-disk and are served
// by separate handlers; see internal/handlers/branding.go and the
// branding.* config keys. Embedding the SPA does not affect them.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded SPA bundle rooted at the dist directory so
// callers serve `/index.html` rather than `/dist/index.html`.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Sub only fails on a malformed path; "dist" is a compile-time
		// constant, so this is unreachable.
		panic(err)
	}
	return sub
}
