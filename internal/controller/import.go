package controller

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/csvimport"
)

// maxCSVUploadBytes mirrors the kiosk-side cap. Tens of thousands of rows
// fit comfortably; tighter limits would only frustrate operators.
const maxCSVUploadBytes = 10 << 20

// CSVImportItems / CSVImportUsers / CSVImportGroups are thin HTTP wrappers
// around csvimport.Run. Each one auth-gates, parses the multipart upload,
// and JSON-encodes the result. The actual validation/upsert lives in
// internal/csvimport so the controller and kiosk binaries share one
// codepath. Catalog publisher hooks bound during startup pick up the
// resulting record-save events and fan them out to managed kiosks.
func (h *Handlers) CSVImportItems(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return runCSVImport(re, h.App, csvimport.KindItems)
}

func (h *Handlers) CSVImportUsers(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return runCSVImport(re, h.App, csvimport.KindUsers)
}

func (h *Handlers) CSVImportGroups(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return runCSVImport(re, h.App, csvimport.KindGroups)
}

func runCSVImport(re *core.RequestEvent, app core.App, kind csvimport.Kind) error {
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

// CSVImportTemplateItems / Users / Groups stream a starter CSV with the
// column headers the importer expects plus an example row or two. Admins
// can download → edit → upload without having to memorize the schema.
func (h *Handlers) CSVImportTemplateItems(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return writeImportTemplate(re, csvimport.KindItems)
}

func (h *Handlers) CSVImportTemplateUsers(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return writeImportTemplate(re, csvimport.KindUsers)
}

func (h *Handlers) CSVImportTemplateGroups(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	return writeImportTemplate(re, csvimport.KindGroups)
}

func writeImportTemplate(re *core.RequestEvent, kind csvimport.Kind) error {
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
