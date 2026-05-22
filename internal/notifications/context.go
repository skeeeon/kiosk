package notifications

import (
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/kioskctx"
)

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
