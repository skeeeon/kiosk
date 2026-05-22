// Package notifications renders and dispatches transactional emails for
// kiosk events. v1 ships one event type — receipt.transaction — whose body
// is iterated over all lines in a single commit; templates are stored in
// the notification_templates collection and editable from the admin SPA.
//
// Defaults compiled into the binary are the source of truth for the
// "Reset to defaults" affordance. The migration that creates the collection
// seeds the receipt.transaction row from these constants; the GET-defaults
// HTTP endpoint returns them on demand so the SPA can refill its textareas
// without a server-side reset mutation.
package notifications

// EventTypeReceiptTransaction is the only event type in v1. Future event
// types (alert.lowstock, alert.cross_user_return) add a new constant and a
// new case to Defaults.
const EventTypeReceiptTransaction = "receipt.transaction"

// DefaultReceiptSubject and DefaultReceiptBody are the v1 receipt template.
// Field references must match notifications.ReceiptContext.
const DefaultReceiptSubject = `Kiosk receipt — {{.Transaction.LinesCount}} {{pluralize .Transaction.LinesCount "item"}} from {{.Kiosk.Code}}`

const DefaultReceiptBody = `Hi {{.User.Name}},

Here is your receipt from kiosk {{.Kiosk.Code}} ({{.Kiosk.LocationCode}}) on {{formatTime .Transaction.CompletedAt}}:

{{range .Lines}}- {{actionVerb .Action}} {{.Qty}} × {{.ItemName}} ({{.ItemCode}}){{if .Serial}} [serial: {{.Serial}}]{{end}}
{{end}}
Transaction id: {{.Transaction.ID}}

Thanks.
`

// Defaults returns the compiled-in default subject and body for the given
// event type. ok is false when the event type is unknown — callers (the
// migration seeder and the GET-defaults handler) treat that as "nothing
// to do" rather than an error.
func Defaults(eventType string) (subject, body string, ok bool) {
	switch eventType {
	case EventTypeReceiptTransaction:
		return DefaultReceiptSubject, DefaultReceiptBody, true
	}
	return "", "", false
}

// DefaultName returns the human-readable label seeded for a template row.
func DefaultName(eventType string) string {
	switch eventType {
	case EventTypeReceiptTransaction:
		return "Transaction receipt"
	}
	return eventType
}

// SeededEventTypes lists every event type the migration should seed on
// first run. Adding a new built-in template means appending here and to
// Defaults / DefaultName.
func SeededEventTypes() []string {
	return []string{EventTypeReceiptTransaction}
}
