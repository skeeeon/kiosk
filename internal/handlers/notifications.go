package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/notifications"
)

// notificationTemplateDTO is the JSON shape returned to the admin SPA.
// Mirrors the collection columns; `id` is included so the SPA can issue
// further requests by primary key if needed (today it routes by event_type).
type notificationTemplateDTO struct {
	ID        string                   `json:"id"`
	EventType string                   `json:"event_type"`
	Name      string                   `json:"name"`
	Enabled   bool                     `json:"enabled"`
	Subject   string                   `json:"subject"`
	Body      string                   `json:"body"`
	Updated   string                   `json:"updated"`
	UpdatedBy string                   `json:"updated_by"`
	// Recipients reflects the stored recipients spec. Empty/null persisted
	// values are normalized to the event-type's default so the SPA always
	// renders concrete checkbox state.
	Recipients notifications.Recipients `json:"recipients"`
	// SupportsWorker mirrors notifications.SupportsWorker. The SPA uses it
	// to enable/disable the worker_email checkbox without per-event-type
	// knowledge of its own.
	SupportsWorker bool `json:"supports_worker"`
}

func toNotificationTemplateDTO(r *core.Record) notificationTemplateDTO {
	eventType := r.GetString("event_type")
	recipients := notifications.DefaultRecipients(eventType)
	if raw := strings.TrimSpace(r.GetString("recipients")); raw != "" && raw != "null" {
		var parsed notifications.Recipients
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			recipients = parsed
		}
	}
	if recipients.Extras == nil {
		recipients.Extras = []string{}
	}
	return notificationTemplateDTO{
		ID:             r.Id,
		EventType:      eventType,
		Name:           r.GetString("name"),
		Enabled:        r.GetBool("enabled"),
		Subject:        r.GetString("subject"),
		Body:           r.GetString("body"),
		Updated:        r.GetDateTime("updated").String(),
		UpdatedBy:      r.GetString("updated_by"),
		Recipients:     recipients,
		SupportsWorker: notifications.SupportsWorker(eventType),
	}
}

// ListNotificationTemplates returns every seeded template row in
// event_type order. Admin-gated.
func (h *Handlers) ListNotificationTemplates(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	rows, err := h.App.FindRecordsByFilter(notifications.CollectionName, "", "event_type", 0, 0)
	if err != nil {
		return re.InternalServerError("load templates failed", err)
	}
	out := make([]notificationTemplateDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, toNotificationTemplateDTO(r))
	}
	return re.JSON(http.StatusOK, map[string]any{"templates": out})
}

// UpdateNotificationTemplate patches subject/body/enabled on an existing
// template row identified by its event_type path segment. Validates both
// template strings parse via text/template before saving — malformed input
// is rejected with a 400 carrying the parse error.
func (h *Handlers) UpdateNotificationTemplate(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	eventType := re.Request.PathValue("event_type")
	if eventType == "" {
		return re.BadRequestError("event_type is required", nil)
	}

	var body struct {
		Subject    *string                   `json:"subject,omitempty"`
		Body       *string                   `json:"body,omitempty"`
		Enabled    *bool                     `json:"enabled,omitempty"`
		Recipients *notifications.Recipients `json:"recipients,omitempty"`
	}
	if err := re.BindBody(&body); err != nil {
		return re.BadRequestError("invalid request body", err)
	}
	if body.Subject == nil && body.Body == nil && body.Enabled == nil && body.Recipients == nil {
		return re.BadRequestError("at least one of subject, body, enabled, recipients is required", nil)
	}

	rec, err := h.App.FindFirstRecordByFilter(
		notifications.CollectionName,
		"event_type = {:t}",
		dbx.Params{"t": eventType},
	)
	if err != nil || rec == nil {
		return re.NotFoundError("template not found", nil)
	}

	newSubject := rec.GetString("subject")
	newBody := rec.GetString("body")
	if body.Subject != nil {
		newSubject = *body.Subject
	}
	if body.Body != nil {
		newBody = *body.Body
	}

	// Parse-validate whenever either text field is changing. Saving an
	// unchanged template would still pass; we only block the new bytes.
	if body.Subject != nil || body.Body != nil {
		if err := notifications.ValidateTemplates(newSubject, newBody); err != nil {
			return re.BadRequestError(err.Error(), nil)
		}
	}

	if body.Subject != nil {
		rec.Set("subject", newSubject)
	}
	if body.Body != nil {
		rec.Set("body", newBody)
	}
	if body.Enabled != nil {
		rec.Set("enabled", *body.Enabled)
	}
	if body.Recipients != nil {
		normalized, err := normalizeRecipients(*body.Recipients, rec.GetString("event_type"))
		if err != nil {
			return re.BadRequestError(err.Error(), nil)
		}
		raw, err := json.Marshal(normalized)
		if err != nil {
			return re.InternalServerError("encode recipients", err)
		}
		rec.Set("recipients", string(raw))
	}
	rec.Set("updated_by", re.Auth.Id)

	if err := h.App.Save(rec); err != nil {
		return re.InternalServerError("save failed", err)
	}
	return re.JSON(http.StatusOK, toNotificationTemplateDTO(rec))
}

// normalizeRecipients trims and validates the supplied spec. Empties are
// coerced to empty slices (never nil) so the persisted JSON shape is
// stable, and extras are validated as parseable mail addresses so the
// notifier doesn't have to choke on bad input at send time. worker_email
// can only be true for event types whose payload implements
// WorkerEmailProvider — guarded by notifications.SupportsWorker.
func normalizeRecipients(in notifications.Recipients, eventType string) (notifications.Recipients, error) {
	out := notifications.Recipients{
		WorkerEmail: in.WorkerEmail,
		AllAdmins:   in.AllAdmins,
		Extras:      []string{},
	}
	if out.WorkerEmail && !notifications.SupportsWorker(eventType) {
		return out, fmt.Errorf("worker_email is not supported by event type %q", eventType)
	}
	seen := map[string]bool{}
	for _, raw := range in.Extras {
		addr := strings.TrimSpace(raw)
		if addr == "" {
			continue
		}
		if _, err := mail.ParseAddress(addr); err != nil {
			return out, fmt.Errorf("invalid extras email %q: %v", addr, err)
		}
		key := strings.ToLower(addr)
		if seen[key] {
			continue
		}
		seen[key] = true
		out.Extras = append(out.Extras, addr)
	}
	return out, nil
}

// GetNotificationTemplateDefaults returns the compiled-in subject + body
// for the given event_type so the SPA's "Reset to defaults" button can
// fill its textareas without issuing a server-side mutation. The admin
// still has to click Save to persist; this endpoint is read-only.
func (h *Handlers) GetNotificationTemplateDefaults(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	eventType := re.Request.PathValue("event_type")
	if eventType == "" {
		return re.BadRequestError("event_type is required", nil)
	}
	subject, body, ok := notifications.Defaults(eventType)
	if !ok {
		return re.NotFoundError("no defaults for that event_type", nil)
	}
	return re.JSON(http.StatusOK, map[string]any{
		"event_type": eventType,
		"subject":    subject,
		"body":       body,
	})
}
