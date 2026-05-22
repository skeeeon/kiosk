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
}

// PayloadSummary surfaces a compact "kiosk · N rows" line in the send log.
func (c OpenChecksDigestContext) PayloadSummary() string {
	return c.Kiosk.Code + " · " + strconv.Itoa(c.RowsCount) + " open rows"
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
