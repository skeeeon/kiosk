package notifications

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"text/template"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	pbmailer "github.com/pocketbase/pocketbase/tools/mailer"
)

// CollectionName is the PocketBase collection that stores editable
// templates. Single source of truth for migrations + handlers + notifier.
const CollectionName = "notification_templates"

// Notifier renders a stored template against the supplied data and sends
// the resulting email via PocketBase's mail client. Send is fire-and-forget
// from the caller's perspective: it spawns a goroutine so a slow SMTP
// server never blocks the commit response. Errors are logged via slog.
//
// A nil Notifier is a valid no-op — Send returns immediately. This lets
// the kiosk run without configuring SMTP at all.
type Notifier struct {
	app core.App
}

// New constructs a Notifier bound to the kiosk's PocketBase app. The app
// supplies both the DB (to load template rows) and the mailer (to send).
func New(app core.App) *Notifier {
	return &Notifier{app: app}
}

// Send dispatches the named event asynchronously. Errors during template
// load, render, or SMTP delivery are logged but never propagate back —
// notifications must not affect the success of the originating action.
func (n *Notifier) Send(eventType string, data any) {
	if n == nil {
		return
	}
	go func() {
		if err := n.deliver(eventType, data); err != nil {
			slog.Error("notifications send failed", "event_type", eventType, "err", err)
		}
	}()
}

// deliver is the synchronous body of Send. It is unexported but written
// to be straightforward to call from tests if a future test needs to
// observe send behavior without goroutines.
func (n *Notifier) deliver(eventType string, data any) error {
	rec, err := n.app.FindFirstRecordByFilter(CollectionName, "event_type = {:t}", dbx.Params{"t": eventType})
	if err != nil {
		return fmt.Errorf("find template %q: %w", eventType, err)
	}
	if !rec.GetBool("enabled") {
		return nil
	}

	recipient := recipientFor(eventType, data)
	if recipient == "" {
		return nil
	}

	subject, body, err := Render(rec.GetString("subject"), rec.GetString("body"), data)
	if err != nil {
		return fmt.Errorf("render template %q: %w", eventType, err)
	}

	settings := n.app.Settings()
	msg := &pbmailer.Message{
		From: mail.Address{
			Address: settings.Meta.SenderAddress,
			Name:    settings.Meta.SenderName,
		},
		To:      []mail.Address{{Address: recipient}},
		Subject: subject,
		Text:    body,
	}
	if err := n.app.NewMailClient().Send(msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// recipientFor extracts the destination address from a payload struct.
// Hardcoded per event type because v1 has exactly one. Adding a new event
// type means adding a case here — keeps recipient routing simple and
// declarative without a recipients-config column on the collection.
func recipientFor(eventType string, data any) string {
	switch eventType {
	case EventTypeReceiptTransaction:
		if rc, ok := data.(ReceiptContext); ok {
			return strings.TrimSpace(rc.User.Email)
		}
	}
	return ""
}

// Render parses and executes both subject and body against data. It is
// the same code path used by the live Send and by the admin PATCH handler
// (which calls ValidateTemplates first to catch syntax errors before save).
func Render(subjectSrc, bodySrc string, data any) (subject, body string, err error) {
	funcs := FuncMap()
	subjTmpl, err := template.New("subject").Funcs(funcs).Parse(subjectSrc)
	if err != nil {
		return "", "", fmt.Errorf("parse subject: %w", err)
	}
	bodyTmpl, err := template.New("body").Funcs(funcs).Parse(bodySrc)
	if err != nil {
		return "", "", fmt.Errorf("parse body: %w", err)
	}
	var sbuf, bbuf bytes.Buffer
	if err := subjTmpl.Execute(&sbuf, data); err != nil {
		return "", "", fmt.Errorf("execute subject: %w", err)
	}
	if err := bodyTmpl.Execute(&bbuf, data); err != nil {
		return "", "", fmt.Errorf("execute body: %w", err)
	}
	return strings.TrimSpace(sbuf.String()), bbuf.String(), nil
}

// ValidateTemplates returns the first parse error encountered while
// compiling subject and body. Use from the admin PATCH handler to reject
// malformed input with a useful message before persisting. Bad field
// references ({{.NotAField}}) are not caught here — those surface only
// at render time and end up in the slog stream.
func ValidateTemplates(subject, body string) error {
	funcs := FuncMap()
	if _, err := template.New("subject").Funcs(funcs).Parse(subject); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	if _, err := template.New("body").Funcs(funcs).Parse(body); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	return nil
}

// ErrUnknownEventType is returned by handlers that look up an event_type
// the binary doesn't recognize — usually a typo in a URL path.
var ErrUnknownEventType = errors.New("unknown event type")
