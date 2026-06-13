package notifications

import (
	"strconv"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

// WorkerEmail implements WorkerEmailProvider so the notifier's recipient
// resolver can include the worker for templates whose Recipients have
// worker_email=true.
func (r ReceiptContext) WorkerEmail() string {
	return r.User.Email
}

// ReceiptContext is the single template payload for receipt.transaction.
// Field names mirror the event payload keys already emitted by commit.go
// (see internal/events/payloads.go); operators editing templates use the
// same vocabulary they'd see in JetStream subjects and exported logs.
type ReceiptContext struct {
	Kiosk       KioskInfo
	User        UserInfo
	Transaction TransactionInfo
	Lines       []LineInfo
}

type KioskInfo struct {
	Code         string
	LocationCode string
}

type UserInfo struct {
	ID    string
	Code  string
	Name  string
	Email string
}

type TransactionInfo struct {
	ID          string
	StartedAt   time.Time
	CompletedAt time.Time
	LinesCount  int
	CheckedOut  int
	Returned    int
	Consumed    int
}

type LineInfo struct {
	ItemCode string
	ItemName string
	Action   string
	Qty      int
	Serial   string
}

// ItemInfo is the per-item slice referenced by non-receipt contexts
// (low-stock, future digests). Kept minimal — only fields a notification
// would plausibly want to render.
type ItemInfo struct {
	ID       string
	Code     string
	Name     string
	Category string
	Unit     string
}

// LowStockContext drives the alert.lowstock template. Built by the cart
// commit handler after Commit succeeds. Does NOT implement
// WorkerEmailProvider — alerts target ops, not the scanning worker.
type LowStockContext struct {
	Kiosk     KioskInfo
	Item      ItemInfo
	PrevQty   int
	NewQty    int
	Threshold int
	Available int    // for tools: qty_on_hand − open_checkouts; consumables: qty_on_hand
	Trigger   string // "consume" today; "adjust" reserved for future
}

// PayloadSummary contributes a one-line context snippet to
// notification_send_log so admins can scan the log without expanding
// individual rows. The shape ("item code · before → after") matches the
// receipt summary style.
func (c LowStockContext) PayloadSummary() string {
	return c.Item.Code + " · " + strconv.Itoa(c.PrevQty) + " → " + strconv.Itoa(c.NewQty) +
		" (threshold " + strconv.Itoa(c.Threshold) + ")"
}

// PayloadSummary mirrors the LowStockContext implementation for receipts.
// Used by the send log to render compact descriptions.
func (c ReceiptContext) PayloadSummary() string {
	return "tx " + c.Transaction.ID + " · " + strconv.Itoa(c.Transaction.LinesCount) + " lines"
}

// MaintenanceUnit is one serialized unit that entered maintenance, for the
// alert.maintenance template's per-unit list.
type MaintenanceUnit struct {
	ItemCode     string
	ItemName     string
	InstanceCode string
	Serial       string
	Reason       string
}

// MaintenanceContext drives the alert.maintenance template. Built by the cart
// commit handler — one context per transaction, listing every serialized unit
// the return routed into maintenance. Does NOT implement WorkerEmailProvider —
// alerts target ops.
//
// Ref is the dedup anchor (the transaction id), keyed by SendIfFirst so a
// JetStream redelivery of the same batch collapses to one email per day.
type MaintenanceContext struct {
	Kiosk   KioskInfo
	Units   []MaintenanceUnit
	Trigger string // "return"
	Ref     string
}

// PayloadSummary surfaces a compact "kiosk · N units → maintenance" line in
// the send log.
func (c MaintenanceContext) PayloadSummary() string {
	return c.Kiosk.Code + " · " + strconv.Itoa(len(c.Units)) + " unit(s) → maintenance"
}

// MaintenanceDigestRow is one unit currently in maintenance, for the
// digest.maintenance scheduled report. KioskCode is populated only on the
// controller's fleet-wide fan-out (the row's origin kiosk); standalone runs
// leave it empty since there's a single implicit kiosk.
type MaintenanceDigestRow struct {
	ItemCode     string
	ItemName     string
	InstanceCode string
	Serial       string
	Notes        string
	KioskCode    string
}

// MaintenanceDigestContext drives the digest.maintenance template. Built by
// the scheduler runner: standalone reads its own item_instances; the
// controller fans out instance snapshots across online kiosks (offline ones
// land in OfflineKiosks so the operator knows the digest is partial). Does
// not implement WorkerEmailProvider — digests target admins.
type MaintenanceDigestContext struct {
	Kiosk       KioskInfo
	GeneratedAt time.Time
	Rows        []MaintenanceDigestRow
	// RowsCount is duplicated from len(Rows) so templates can render counts
	// without the {{len .Rows}} action — same convention as the other digests.
	RowsCount int
	// Provenance breakdown of which kiosks contributed and how. Empty on a
	// standalone run. LastKnownKiosks were offline and served from the
	// controller's projection; UnavailableKiosks could not be reached and
	// have no fallback — both render a completeness note so the digest is
	// never silently partial. See KioskProvenance.
	KioskProvenance
}

// KioskProvenance records, for a controller fan-out, which kiosks answered
// live, which were served from last-known projected state (offline), and
// which could not be covered at all. Embedded in the digest contexts so
// templates can flag partial results explicitly. All empty on a standalone
// (non-fan-out) run.
type KioskProvenance struct {
	LiveKiosks        []string
	LastKnownKiosks   []string
	UnavailableKiosks []string
}

// PayloadSummary surfaces a compact "kiosk · N in maintenance" line in the
// send log.
func (c MaintenanceDigestContext) PayloadSummary() string {
	return c.Kiosk.Code + " · " + strconv.Itoa(c.RowsCount) + " in maintenance"
}

// TimeclockDigestRow is one user-day total in the timeclock digest. Total is
// pre-formatted (e.g. "7h30m") so the template stays arithmetic-free; Open
// flags a day whose interval was still running when the digest fired.
type TimeclockDigestRow struct {
	UserCode string
	UserName string
	Date     string // YYYY-MM-DD in the serving binary's local timezone
	Total    string
	Open     bool
}

// TimeclockDigestContext drives the digest.timeclock template. Built by the
// scheduler runner from the punch ledger (kiosk: local; controller: the
// fleet projection, optionally scoped by the schedule row's kiosk_code).
// Totals come from report-time punch pairing — display approximations; the
// raw-punch CSV is the payroll contract. Does not implement
// WorkerEmailProvider — digests target admins.
type TimeclockDigestContext struct {
	Kiosk        KioskInfo
	GeneratedAt  time.Time
	WindowStart  time.Time
	WindowEnd    time.Time
	Cadence      string // "daily" | "weekly" | "monthly"
	PunchCount   int
	ClockedInNow int
	Rows         []TimeclockDigestRow
	RowsCount    int
	Uncorrelated int
}

// PayloadSummary surfaces a compact "kiosk · N punches" line in the send log.
func (c TimeclockDigestContext) PayloadSummary() string {
	return c.Kiosk.Code + " · " + strconv.Itoa(c.PunchCount) + " punches"
}

// TimeclockSelfDigestContext drives the digest.timeclock_self template — ONE
// worker's own timesheet, delivered only to them. Built by the scheduler's
// per-worker fan-out runner (one context per active worker with punches in the
// window). Unlike TimeclockDigestContext (admin-facing, every worker's hours in
// one email), this implements WorkerEmailProvider so the notifier resolves the
// recipient to the worker themselves — the same delivery primitive receipts
// use. Rows reuse TimeclockDigestRow but carry only this worker's days; Total
// is the pre-formatted window total. Display approximations from punch pairing;
// the raw-punch CSV is the payroll contract.
type TimeclockSelfDigestContext struct {
	Kiosk        KioskInfo
	Worker       UserInfo
	GeneratedAt  time.Time
	WindowStart  time.Time
	WindowEnd    time.Time
	Cadence      string // "daily" | "weekly" | "monthly"
	Rows         []TimeclockDigestRow
	RowsCount    int
	Total        string // pre-formatted window total, e.g. "37h30m"
	ClockedIn    bool   // worker has an interval still running in the window
	Uncorrelated int    // this worker's unpaired punches in the window
}

// WorkerEmail implements WorkerEmailProvider so the notifier delivers this
// digest to the worker it summarizes — and to no one else.
func (c TimeclockSelfDigestContext) WorkerEmail() string {
	return c.Worker.Email
}

// PayloadSummary surfaces a compact "user_code · total" line in the send log.
func (c TimeclockSelfDigestContext) PayloadSummary() string {
	return c.Worker.Code + " · " + c.Total
}

// OpenChecksDigestRow is one row in an open-checkouts digest. Field names
// match the existing ledger.OpenCheckoutDTO shape so the conversion in the
// scheduler is a simple field copy.
type OpenChecksDigestRow struct {
	ItemCode     string
	ItemName     string
	Serial       string
	UserCode     string
	UserName     string
	CheckedOutAt time.Time
}

// OpenChecksDigestContext drives the digest.open_checkouts template. The
// scheduler builds one of these per scheduled-run by replaying the ledger.
// Does not implement WorkerEmailProvider — digests target admins.
type OpenChecksDigestContext struct {
	Kiosk       KioskInfo
	GeneratedAt time.Time
	Rows        []OpenChecksDigestRow
	// RowsCount is duplicated from len(Rows) so templates can render counts
	// without needing the {{len .Rows}} action and to feed the pluralize
	// helper without an extra step.
	RowsCount int
	// Provenance breakdown for a controller fan-out (empty on standalone).
	// See KioskProvenance — LastKnownKiosks/UnavailableKiosks drive the
	// completeness note so an offline kiosk is never silently dropped.
	KioskProvenance
}

// PayloadSummary surfaces a compact "kiosk · N rows" line in the send log.
func (c OpenChecksDigestContext) PayloadSummary() string {
	return c.Kiosk.Code + " · " + strconv.Itoa(c.RowsCount) + " open rows"
}

// DailyActivityItemRow is one entry in the top-items leaderboard of a
// daily-activity digest. LineCount is "how many distinct transaction_lines
// touched this item in the window," not the qty sum — the latter is
// dominated by consumables and would mask serialized-tool activity.
type DailyActivityItemRow struct {
	ItemCode  string
	ItemName  string
	LineCount int
}

// DailyActivityWorkerRow mirrors DailyActivityItemRow for the top-workers
// leaderboard.
type DailyActivityWorkerRow struct {
	UserCode  string
	UserName  string
	LineCount int
}

// DailyActivityContext drives the digest.daily_activity template. The
// window is sized by the schedule row's cadence (daily=24h, weekly=7d,
// monthly=30d); the kiosk-side runner stamps WindowStart/WindowEnd and
// the action counters here so the controller side is purely render+SMTP.
type DailyActivityContext struct {
	Kiosk            KioskInfo
	GeneratedAt      time.Time
	WindowStart      time.Time
	WindowEnd        time.Time
	Cadence          string // "daily" | "weekly" | "monthly"
	TransactionCount int
	LinesCount       int
	UniqueUsers      int
	CheckedOut       int
	Returned         int
	Consumed         int
	TopItems         []DailyActivityItemRow   // up to 5
	TopWorkers       []DailyActivityWorkerRow // up to 5
}

// PayloadSummary surfaces a compact "kiosk · cadence · N txns / M lines"
// line in the send log.
func (c DailyActivityContext) PayloadSummary() string {
	return c.Kiosk.Code + " · " + c.Cadence + " · " +
		strconv.Itoa(c.TransactionCount) + " txns / " +
		strconv.Itoa(c.LinesCount) + " lines"
}

// BuildReceiptContext assembles the template payload from the values the
// commit handler has in hand after a successful Commit. The user record is
// fetched here because the cart only carries id+code+name and templates
// need the email for downstream recipient resolution; one extra read on
// the receipt path is fine — commits are rare relative to scans.
func BuildReceiptContext(app core.App, c *cart.Cart, id kioskctx.Identity, result *commit.Result, completedAt time.Time) (ReceiptContext, error) {
	user, err := app.FindRecordById("users", c.UserID)
	if err != nil {
		return ReceiptContext{}, err
	}

	lines := make([]LineInfo, 0, len(c.Lines))
	for _, l := range c.Lines {
		lines = append(lines, LineInfo{
			ItemCode: l.ItemCode,
			ItemName: l.ItemName,
			Action:   l.Action,
			Qty:      l.Qty,
			Serial:   l.Serial,
		})
	}

	return ReceiptContext{
		Kiosk: KioskInfo{
			Code:         id.KioskCode,
			LocationCode: id.LocationCode,
		},
		User: UserInfo{
			ID:    user.Id,
			Code:  user.GetString("code"),
			Name:  user.GetString("name"),
			Email: user.GetString("email"),
		},
		Transaction: TransactionInfo{
			ID:          result.TransactionID,
			StartedAt:   c.StartedAt,
			CompletedAt: completedAt,
			LinesCount:  result.LinesCount,
			CheckedOut:  result.CheckedOut,
			Returned:    result.Returned,
			Consumed:    result.Consumed,
		},
		Lines: lines,
	}, nil
}
