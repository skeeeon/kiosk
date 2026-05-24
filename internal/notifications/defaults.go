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

// Event type identifiers. Each constant corresponds to one
// notification_templates row, one Defaults case, and (for events with a
// non-receipt payload) one Recipients default.
const (
	EventTypeReceiptTransaction = "receipt.transaction"
	EventTypeLowStock           = "alert.lowstock"
	EventTypeOpenChecksDigest   = "digest.open_checkouts"
	EventTypeDailyActivity      = "digest.daily_activity"
)

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

// DefaultOpenChecksSubject and DefaultOpenChecksBody render against
// OpenChecksDigestContext. Scheduled reports populate this template; the
// recipients on the schedule row override the template's recipients spec.
const DefaultOpenChecksSubject = `Open checkouts digest from {{.Kiosk.Code}} — {{.RowsCount}} {{pluralize .RowsCount "item"}}`

const DefaultOpenChecksBody = `Open checkouts at kiosk {{.Kiosk.Code}} as of {{formatTime .GeneratedAt}}:

{{if eq .RowsCount 0}}No items are currently checked out.
{{else}}{{range .Rows}}- {{.ItemName}} ({{.ItemCode}}) — held by {{.UserName}} since {{formatTime .CheckedOutAt}}{{if .Serial}} [serial: {{.Serial}}]{{end}}
{{end}}{{end}}
This is an automated digest. Adjust its schedule or recipients in the kiosk admin SPA.
`

// DefaultDailyActivitySubject and DefaultDailyActivityBody render against
// DailyActivityContext. The window is sized by the schedule row's cadence
// (daily=24h, weekly=7d, monthly=30d). When the window is empty the body
// short-circuits to a "no activity" line — same pattern as the
// open-checkouts digest.
const DefaultDailyActivitySubject = `{{.Kiosk.Code}} activity {{.Cadence}} — {{.TransactionCount}} {{pluralize .TransactionCount "transaction"}}, {{.LinesCount}} {{pluralize .LinesCount "line"}}`

const DefaultDailyActivityBody = `Activity at kiosk {{.Kiosk.Code}} ({{.Kiosk.LocationCode}}) from {{formatTime .WindowStart}} to {{formatTime .WindowEnd}}:

{{if eq .TransactionCount 0}}No activity in this window.
{{else}}- Transactions: {{.TransactionCount}}
- Lines: {{.LinesCount}}
- Unique workers: {{.UniqueUsers}}
- Checked out: {{.CheckedOut}} · Returned: {{.Returned}} · Consumed: {{.Consumed}}

{{if .TopItems}}Top items:
{{range .TopItems}}- {{.ItemName}} ({{.ItemCode}}) — {{.LineCount}} {{pluralize .LineCount "line"}}
{{end}}{{end}}{{if .TopWorkers}}
Top workers:
{{range .TopWorkers}}- {{.UserName}} ({{.UserCode}}) — {{.LineCount}} {{pluralize .LineCount "line"}}
{{end}}{{end}}{{end}}
This is an automated digest. Adjust its schedule or recipients in the kiosk admin SPA.
`

// DefaultLowStockSubject and DefaultLowStockBody render against
// LowStockContext (see internal/notifications/context.go).
const DefaultLowStockSubject = `Low stock at {{.Kiosk.Code}}: {{.Item.Name}} ({{.Item.Code}})`

const DefaultLowStockBody = `Heads up — {{.Item.Name}} ({{.Item.Code}}) just crossed its reorder threshold at kiosk {{.Kiosk.Code}} ({{.Kiosk.LocationCode}}).

Available: {{.Available}}{{if .Item.Unit}} {{.Item.Unit}}{{end}}
Threshold: {{.Threshold}}
Triggered by: {{.Trigger}}

This alert fires once per item per day. Adjust the reorder threshold or
deactivate the template if the dedupe window is too tight.
`

// Defaults returns the compiled-in default subject and body for the given
// event type. ok is false when the event type is unknown — callers (the
// migration seeder and the GET-defaults handler) treat that as "nothing
// to do" rather than an error.
func Defaults(eventType string) (subject, body string, ok bool) {
	switch eventType {
	case EventTypeReceiptTransaction:
		return DefaultReceiptSubject, DefaultReceiptBody, true
	case EventTypeLowStock:
		return DefaultLowStockSubject, DefaultLowStockBody, true
	case EventTypeOpenChecksDigest:
		return DefaultOpenChecksSubject, DefaultOpenChecksBody, true
	case EventTypeDailyActivity:
		return DefaultDailyActivitySubject, DefaultDailyActivityBody, true
	}
	return "", "", false
}

// DefaultName returns the human-readable label seeded for a template row.
func DefaultName(eventType string) string {
	switch eventType {
	case EventTypeReceiptTransaction:
		return "Transaction receipt"
	case EventTypeLowStock:
		return "Low stock alert"
	case EventTypeOpenChecksDigest:
		return "Open checkouts digest"
	case EventTypeDailyActivity:
		return "Daily activity digest"
	}
	return eventType
}

// SeededEventTypes lists every event type the migration should seed on
// first run. Adding a new built-in template means appending here and to
// Defaults / DefaultName.
func SeededEventTypes() []string {
	return []string{
		EventTypeReceiptTransaction,
		EventTypeLowStock,
		EventTypeOpenChecksDigest,
		EventTypeDailyActivity,
	}
}

// Recipients is the editable per-template audience descriptor stored in the
// recipients JSON column on notification_templates. The notifier resolves
// it to a concrete []mail.Address at send time:
//
//   - WorkerEmail: if true and the event's payload implements
//     WorkerEmailProvider, the worker's email is included.
//   - AllAdmins:   if true, every admins-collection row with active=true is
//     expanded into the recipient list.
//   - Extras:      free-form addresses (e.g., a shared ops mailbox).
//
// An empty/missing JSON column is treated as the event's compiled-in
// default — see DefaultRecipients. Empty Extras + AllAdmins=false +
// WorkerEmail=false produces a no-op skip rather than an error.
type Recipients struct {
	WorkerEmail bool     `json:"worker_email"`
	AllAdmins   bool     `json:"all_admins"`
	Extras      []string `json:"extras"`
}

// DefaultRecipients returns the audience an event type ships with. Used by
// the recipients migration (to seed existing rows) and by the notifier (to
// fall back when a row's recipients column is null/empty).
func DefaultRecipients(eventType string) Recipients {
	switch eventType {
	case EventTypeReceiptTransaction:
		return Recipients{WorkerEmail: true, Extras: []string{}}
	case EventTypeLowStock:
		// Alerts target ops, not the worker who happened to push the item
		// across the threshold. all_admins captures every active admin;
		// operators can add a shared mailbox via the extras textarea.
		return Recipients{AllAdmins: true, Extras: []string{}}
	case EventTypeOpenChecksDigest:
		// Digests address admins by default. Each scheduled_reports row
		// overrides this with its own recipients spec at send time.
		return Recipients{AllAdmins: true, Extras: []string{}}
	case EventTypeDailyActivity:
		return Recipients{AllAdmins: true, Extras: []string{}}
	}
	// Conservative default for unrecognized event types: address nobody.
	// Operators must explicitly opt in to a recipient class.
	return Recipients{Extras: []string{}}
}

// SupportsWorker reports whether the event type's payload implements
// WorkerEmailProvider — i.e., whether the SPA's recipients editor should
// expose the worker_email checkbox. Per-event-type registry rather than
// runtime introspection so the SPA can render the right form before any
// send has fired.
func SupportsWorker(eventType string) bool {
	switch eventType {
	case EventTypeReceiptTransaction:
		return true
	}
	return false
}

// WorkerEmailProvider is implemented by payload types whose recipient set
// can include the worker associated with the event. Receipts implement it;
// future alert/digest contexts (low-stock, scheduled reports) do not.
type WorkerEmailProvider interface {
	WorkerEmail() string
}
