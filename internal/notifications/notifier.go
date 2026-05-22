package notifications

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"text/template"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	pbmailer "github.com/pocketbase/pocketbase/tools/mailer"
)

// CollectionName is the PocketBase collection that stores editable
// templates. Single source of truth for migrations + handlers + notifier.
const CollectionName = "notification_templates"

// SendLogCollectionName is the audit table written one-row-per-recipient
// by every Send call. Visible to admins in the SPA "Recent sends" view.
const SendLogCollectionName = "notification_send_log"

// DedupeCollectionName backs SendIfFirst. The unique (event_type, ref, day)
// index is the race-free gate — an insert that violates the constraint
// means "another caller already fired this combination today."
const DedupeCollectionName = "notification_dedupe"

// Send log status values. Kept here so the notifier and the SPA agree on
// the strings without a separate constants file.
const (
	SendStatusSent    = "sent"
	SendStatusFailed  = "failed"
	SendStatusSkipped = "skipped"
)

// PayloadSummarizer is an optional interface payloads can implement to
// provide a one-line context snippet for the send log. Receipts use it to
// surface "tx tx-abc · 3 lines"; unimplemented payloads log an empty string.
type PayloadSummarizer interface {
	PayloadSummary() string
}

// Notifier renders a stored template against the supplied data and sends
// the resulting email via PocketBase's mail client. Send is fire-and-forget
// from the caller's perspective: it spawns a goroutine so a slow SMTP
// server never blocks the commit response. Errors are logged via slog and
// recorded in the send log.
//
// A nil Notifier is a valid no-op — Send returns immediately. This lets
// the kiosk run without configuring SMTP at all.
type Notifier struct {
	app core.App
}

// New constructs a Notifier bound to the kiosk's PocketBase app. The app
// supplies both the DB (to load template rows + write logs) and the mailer.
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

// SendTo dispatches a send with an explicit Recipients spec, ignoring
// whatever the template row's recipients column says. Synchronous so
// callers can observe the outcome — the scheduler uses this and stamps
// last_status/last_error on the schedule row from the returned error.
// Returns nil when the template is disabled or recipients resolve to no
// addresses (treated as skipped, with a log row written).
func (n *Notifier) SendTo(eventType string, data any, recipients Recipients) error {
	if n == nil {
		return nil
	}
	return n.deliverWith(eventType, data, &recipients)
}

// SendIfFirst is Send gated by a dedupe insert keyed on (eventType, refKey,
// today). It is the path low-stock alerts use so an item that crosses its
// threshold ten times in a day produces one email, not ten.
//
// The unique index on notification_dedupe is the source of truth — a second
// caller racing past the existence check still hits the constraint at
// insert time and is treated as a duplicate. No external locking.
//
// `day` is computed in UTC. Operators in non-UTC TZ get a deduplication
// boundary at UTC midnight rather than local midnight; acceptable for
// alerts and matches how the send log timestamps already render.
func (n *Notifier) SendIfFirst(eventType, refKey string, data any) {
	if n == nil {
		return
	}
	go func() {
		first, err := n.claimDedupe(eventType, refKey)
		if err != nil {
			slog.Warn("notifications dedupe insert failed; dropping to avoid double-send",
				"event_type", eventType, "ref", refKey, "err", err)
			return
		}
		if !first {
			return
		}
		if err := n.deliver(eventType, data); err != nil {
			slog.Error("notifications send failed", "event_type", eventType, "err", err)
		}
	}()
}

// claimDedupe attempts to insert the (event_type, ref, day) tuple. Returns
// first=true when this insert won the race; first=false when a prior call
// already claimed the slot (unique-constraint violation). Any other error
// is returned to the caller and treated as a drop.
func (n *Notifier) claimDedupe(eventType, refKey string) (bool, error) {
	col, err := n.app.FindCollectionByNameOrId(DedupeCollectionName)
	if err != nil {
		return false, fmt.Errorf("find %s: %w", DedupeCollectionName, err)
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", eventType)
	rec.Set("ref", refKey)
	rec.Set("day", todayUTC())
	if err := n.app.Save(rec); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "unique") || strings.Contains(msg, "constraint") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func todayUTC() string {
	return timeNowUTC().Format("2006-01-02")
}

// timeNowUTC is overridable for tests that need to drive the dedupe day
// boundary deterministically. Defaults to time.Now().UTC() in production.
var timeNowUTC = func() time.Time { return time.Now().UTC() }

// deliver is the synchronous body of Send. It loads the template, resolves
// recipients from the template's recipients column, renders, sends, and
// logs. deliverWith is the shared path used by Send and SendTo — pass
// override=nil to use the template's recipients.
func (n *Notifier) deliver(eventType string, data any) error {
	return n.deliverWith(eventType, data, nil)
}

func (n *Notifier) deliverWith(eventType string, data any, override *Recipients) error {
	rec, err := n.app.FindFirstRecordByFilter(CollectionName, "event_type = {:t}", dbx.Params{"t": eventType})
	if err != nil {
		return fmt.Errorf("find template %q: %w", eventType, err)
	}
	if !rec.GetBool("enabled") {
		return nil
	}

	var recipients []mail.Address
	if override != nil {
		recipients = n.resolveSpec(*override, data)
	} else {
		recipients = n.resolveRecipients(eventType, rec, data)
	}
	if len(recipients) == 0 {
		n.writeLog(eventType, rec.Id, "", SendStatusSkipped, "", summaryOf(data))
		return nil
	}

	subject, body, err := Render(rec.GetString("subject"), rec.GetString("body"), data)
	if err != nil {
		// Render failures apply to the whole batch — log one failure row
		// per recipient so the SPA can show "5 recipients · all failed".
		for _, r := range recipients {
			n.writeLog(eventType, rec.Id, r.Address, SendStatusFailed, truncErr(err.Error()), summaryOf(data))
		}
		return fmt.Errorf("render template %q: %w", eventType, err)
	}

	settings := n.app.Settings()
	msg := &pbmailer.Message{
		From: mail.Address{
			Address: settings.Meta.SenderAddress,
			Name:    settings.Meta.SenderName,
		},
		To:      recipients,
		Subject: subject,
		Text:    body,
	}
	sendErr := n.app.NewMailClient().Send(msg)
	status := SendStatusSent
	errMsg := ""
	if sendErr != nil {
		status = SendStatusFailed
		errMsg = truncErr(sendErr.Error())
	}
	for _, r := range recipients {
		n.writeLog(eventType, rec.Id, r.Address, status, errMsg, summaryOf(data))
	}
	if sendErr != nil {
		return fmt.Errorf("smtp send: %w", sendErr)
	}
	return nil
}

// resolveRecipients expands the stored Recipients spec into a concrete
// address list. Missing/empty JSON in the template row falls back to the
// event type's compiled-in default — keeps phase-1 behavior intact for any
// template whose recipients column wasn't migrated.
func (n *Notifier) resolveRecipients(eventType string, rec *core.Record, data any) []mail.Address {
	spec := parseRecipients(rec.GetString("recipients"))
	if spec == nil {
		def := DefaultRecipients(eventType)
		spec = &def
	}
	return n.resolveSpec(*spec, data)
}

// resolveSpec is the inner workhorse — pure (recipients spec + payload) →
// concrete dedup'd addresses. Shared by recipientsFromTemplate and the
// scheduler's per-schedule override path.
func (n *Notifier) resolveSpec(spec Recipients, data any) []mail.Address {
	// Dedupe by lowercased address so worker == admin == extra doesn't
	// produce three log rows for the same person.
	seen := map[string]bool{}
	out := []mail.Address{}
	add := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		key := strings.ToLower(addr)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, mail.Address{Address: addr})
	}

	if spec.WorkerEmail {
		if wp, ok := data.(WorkerEmailProvider); ok {
			add(wp.WorkerEmail())
		}
	}
	if spec.AllAdmins {
		for _, addr := range n.loadAdminEmails() {
			add(addr)
		}
	}
	for _, e := range spec.Extras {
		add(e)
	}
	return out
}

func parseRecipients(raw string) *Recipients {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var r Recipients
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		slog.Warn("notifications recipients parse failed; falling back to default", "err", err)
		return nil
	}
	return &r
}

// loadAdminEmails fetches every active admin's email address. Cached per
// Send call would help if AllAdmins toggled for multiple events in one
// commit; today it's once per Send and the admin pool is tiny.
func (n *Notifier) loadAdminEmails() []string {
	rows, err := n.app.FindRecordsByFilter("admins", "active = true", "email", 0, 0)
	if err != nil {
		slog.Warn("notifications could not list admins", "err", err)
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if email := r.GetString("email"); email != "" {
			out = append(out, email)
		}
	}
	return out
}

// writeLog inserts one notification_send_log row. Best-effort — log-write
// errors slog and continue so the underlying send doesn't get masked by an
// audit-table problem.
func (n *Notifier) writeLog(eventType, templateID, recipient, status, errMsg, summary string) {
	col, err := n.app.FindCollectionByNameOrId(SendLogCollectionName)
	if err != nil {
		slog.Warn("send log collection missing", "err", err)
		return
	}
	rec := core.NewRecord(col)
	rec.Set("event_type", eventType)
	if templateID != "" {
		rec.Set("template", templateID)
	}
	rec.Set("recipient", recipient)
	rec.Set("status", status)
	rec.Set("error", errMsg)
	rec.Set("payload_summary", summary)
	if err := n.app.Save(rec); err != nil {
		slog.Warn("send log write failed", "err", err)
	}
}

// PruneSendLog deletes rows older than the cutoff. Wired into a daily cron
// in cmd/kiosk/main.go to keep the table bounded.
func (n *Notifier) PruneSendLog(olderThan string) (int, error) {
	return n.pruneCollection(SendLogCollectionName, "created < {:cutoff}", olderThan)
}

// PruneDedupe deletes dedupe rows older than the cutoff. The dedupe gate is
// only meaningful for ~one day; keeping the same 90-day window as the send
// log gives a generous safety margin without growing the table.
func (n *Notifier) PruneDedupe(olderThan string) (int, error) {
	return n.pruneCollection(DedupeCollectionName, "created < {:cutoff}", olderThan)
}

func (n *Notifier) pruneCollection(collection, filter, cutoff string) (int, error) {
	if n == nil {
		return 0, nil
	}
	rows, err := n.app.FindRecordsByFilter(collection, filter, "", 0, 0, dbx.Params{"cutoff": cutoff})
	if err != nil {
		return 0, fmt.Errorf("list aged rows in %s: %w", collection, err)
	}
	deleted := 0
	for _, r := range rows {
		if err := n.app.Delete(r); err != nil {
			slog.Warn("prune failed for row", "collection", collection, "id", r.Id, "err", err)
			continue
		}
		deleted++
	}
	return deleted, nil
}

func summaryOf(data any) string {
	if p, ok := data.(PayloadSummarizer); ok {
		return p.PayloadSummary()
	}
	return ""
}

func truncErr(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max]
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
// at render time and end up in the slog stream and send log.
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
