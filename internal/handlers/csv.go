package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/csvimport"
	"github.com/skeeeon/kiosk/internal/exports"
)

const maxCSVUploadBytes = 10 << 20 // 10 MB — enough for tens of thousands of items

// CSVImport upserts items from a CSV upload. Row-level work lives in
// internal/csvimport so the kiosk and controller share the same logic;
// this handler only does auth, multipart parsing, and response shaping.
func (h *Handlers) CSVImport(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return importCSVHandler(re, h.App, csvimport.KindItems)
}

// UsersCSVImport / GroupsCSVImport are the workers and groups counterparts.
// On a managed kiosk these collections are read-only mirrors of the
// controller's catalog, so the SPA hides the corresponding tabs; the
// requireAdmin gate is still the trust boundary if someone hits the
// endpoint directly.
func (h *Handlers) UsersCSVImport(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return importCSVHandler(re, h.App, csvimport.KindUsers)
}

func (h *Handlers) GroupsCSVImport(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return importCSVHandler(re, h.App, csvimport.KindGroups)
}

// importCSVHandler is the shared HTTP wrapper around csvimport.Run. The
// controller's handlers package re-implements this wrapper rather than
// importing handlers (which would pull in cart/scan/etc.), but the shape
// — and the response contract the SPA depends on — matches by design.
func importCSVHandler(re *core.RequestEvent, app core.App, kind csvimport.Kind) error {
	if err := re.Request.ParseMultipartForm(maxCSVUploadBytes); err != nil {
		return re.BadRequestError("could not parse upload (max 10 MB)", err)
	}
	file, _, err := re.Request.FormFile("file")
	if err != nil {
		return re.BadRequestError("file field is required", err)
	}
	defer file.Close()

	dryRun := strings.EqualFold(re.Request.FormValue("dry_run"), "true")

	result, err := csvimport.Run(app, kind, file, dryRun)
	if err != nil {
		return re.BadRequestError(err.Error(), err)
	}
	return re.JSON(http.StatusOK, result)
}

// CSVImportTemplate streams a starter CSV for the items importer. Same
// columns the importer accepts, with one example tool and one consumable
// row pre-filled. Admin-gated like the importer itself; admins are the
// only audience for the download.
func (h *Handlers) CSVImportTemplate(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return writeCSVTemplate(re, csvimport.KindItems)
}

func (h *Handlers) UsersCSVImportTemplate(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return writeCSVTemplate(re, csvimport.KindUsers)
}

func (h *Handlers) GroupsCSVImportTemplate(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return writeCSVTemplate(re, csvimport.KindGroups)
}

// writeCSVTemplate sets headers and delegates to csvimport.TemplateFor.
// Filename is "<kind>-template.csv" so the operator can spot it in their
// downloads list.
func writeCSVTemplate(re *core.RequestEvent, kind csvimport.Kind) error {
	writer := csvimport.TemplateFor(kind)
	if writer == nil {
		return re.NotFoundError(fmt.Sprintf("no template for kind %q", kind), nil)
	}
	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"%s-template.csv\"", kind,
	))
	return writer(w)
}

// TransactionsExportCSV streams completed transactions as CSV. Optional
// ?from= and ?to= ISO timestamps narrow the window; both are inclusive on
// completed_at. Line counts are read from transactions.lines_count
// (denormalized at commit time), so the export is a single SELECT.
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
		"attachment; filename=\"transactions-%s.csv\"", time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteTransactionsCSV(h.App, w, exports.TransactionsOptions{
		From: from,
		To:   to,
		// IncludeSourceKiosk left false — on a standalone kiosk those fields
		// are always blank.
	})
}

// ItemsExportCSV streams the items collection as CSV with the same column
// shape the importer accepts, so an export can round-trip back through
// /items/import. Instances are intentionally not exported here — they live
// in their own collection and aren't part of the items round-trip.
func (h *Handlers) ItemsExportCSV(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}

	w := re.Response
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"items-%s.csv\"", time.Now().UTC().Format("20060102-150405"),
	))
	return exports.WriteItemsCSV(h.App, w)
}
