package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/events"
)

// LedgerRepublishResult summarizes a republish run. Counts are at-rest:
// publish failures land in slog (events.Publish swallows them) but don't
// decrement the totals here — the operator's view is "we walked N rows."
type LedgerRepublishResult struct {
	From                  string `json:"from,omitempty"`
	To                    string `json:"to,omitempty"`
	TransactionsPublished int    `json:"transactions_published"`
	LinesPublished        int    `json:"lines_published"`
	// AdminClosesPublished counts admin force-close transactions re-emitted as
	// a single checkout.admin_close event (the live shape) rather than the
	// transaction.complete + item.{action} pair the regular walk emits.
	AdminClosesPublished int `json:"admin_closes_published,omitempty"`
	Skipped              int `json:"skipped"`
}

// RepublishLedger walks completed transactions (optionally clipped to a
// completed_at window) and re-emits the events the live commit path would
// have: transaction.complete + item.{action} for ordinary transactions, and
// a single checkout.admin_close for admin force-close transactions (which
// commit.AdminClose emits *instead of* the regular pair — so republish must
// match that shape or the controller would diverge). The controller's
// aggregator is idempotent on (source_kiosk_code, source_transaction_id),
// source_line_id, and the admin_close line id, so re-publishing is safe.
//
// Use when the controller's projected ledger has drifted from this
// kiosk's — e.g., the broker was unreachable mid-publish and the
// buffered event was lost on kiosk restart. Full-history republish is
// the brute-force recovery; pass from/to (ISO8601) to scope to the
// suspect window.
//
// kiosk_code / location_code on each event are read from the
// transaction record, not the current kioskctx — so a republish after
// a kiosk.code config change still emits under the original code that
// the aggregator dedupe key was built from.
//
// IMPORTANT: payload shape MUST stay in sync with commit.Commit's
// inline payloads in internal/commit/commit.go. If a field is added
// there, mirror it here.
func (h *Handlers) RepublishLedger(re *core.RequestEvent) error {
	if err := h.requireAdmin(re); err != nil {
		return err
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	_ = re.BindBody(&body)

	result, err := PerformLedgerRepublish(h.App, body.From, body.To, events.Publish)
	if err != nil {
		var verr *validationError
		if errors.As(err, &verr) {
			return re.BadRequestError(verr.msg, nil)
		}
		return re.InternalServerError("load transactions", err)
	}
	return re.JSON(http.StatusOK, result)
}

// PerformLedgerRepublish parses {from, to} as RFC3339, builds the filter,
// and walks completed transactions re-emitting transaction.complete +
// item.{action} events. Pure of HTTP — shared with the controller's
// ledger.republish command bus path. Bad timestamp formats are returned as
// validationError so the HTTP wrapper can surface them as 400.
//
// The publish argument lets tests capture events without a real publisher;
// production wires it to events.Publish.
func PerformLedgerRepublish(app core.App, from, to string, publish func(string, any)) (*LedgerRepublishResult, error) {
	filter := "status = 'completed'"
	params := dbx.Params{}
	if from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return nil, &validationError{msg: "from must be RFC3339 (e.g. 2026-05-01T00:00:00Z)"}
		}
		filter += " && completed_at >= {:from}"
		params["from"] = t.UTC()
	}
	if to != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return nil, &validationError{msg: "to must be RFC3339"}
		}
		filter += " && completed_at <= {:to}"
		params["to"] = t.UTC()
	}

	result, err := republishLedger(app, filter, params, publish)
	if err != nil {
		return nil, err
	}
	result.From = from
	result.To = to
	return &result, nil
}

// validationError flags a bad caller input so wrappers can surface a 400
// (HTTP) or a {success:false, error:"..."} (command bus) without leaking
// it as a 500. Internal — exported only via errors.As-checkable type
// inside the same package.
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

// republishLedger is the pure-DB core of the republish handler — separated
// so it can be unit-tested without an HTTP RequestEvent. Callers pass an
// already-built filter + params and a publish function (events.Publish in
// production, a capture closure in tests).
func republishLedger(app core.App, filter string, params dbx.Params, publish func(string, any)) (LedgerRepublishResult, error) {
	var out LedgerRepublishResult

	txs, err := app.FindRecordsByFilter("transactions",
		filter, "completed_at", 0, 0, params)
	if err != nil {
		return out, err
	}

	for _, tx := range txs {
		userRec, err := app.FindRecordById("users", tx.GetString("user"))
		if err != nil {
			out.Skipped++
			continue
		}
		lines, err := app.FindRecordsByFilter("transaction_lines",
			"transaction = {:tx}", "id", 0, 0,
			dbx.Params{"tx": tx.Id})
		if err != nil {
			out.Skipped++
			continue
		}

		kioskCode := tx.GetString("kiosk_code")
		locationCode := tx.GetString("location_code")
		completedAt := tx.GetDateTime("completed_at").Time()

		// Admin force-close transactions get the LIVE event shape: only a
		// checkout.admin_close event. commit.AdminClose emits exactly that —
		// never transaction.complete or item.* — so re-emitting the regular
		// pair here would (a) add transaction/line rows the controller's live
		// path never creates, and (b) leave the closed open_checkouts row OPEN
		// on the controller, because item.admin_close is a no-op for the
		// open_checkouts projector — only checkout.admin_close deletes the row.
		// An admin-close transaction always has exactly one line.
		if len(lines) == 1 && lines[0].GetString("action") == "admin_close" {
			l := lines[0]
			itemRec, ierr := app.FindRecordById("items", l.GetString("item"))
			if ierr != nil {
				out.Skipped++
				continue
			}
			publish(events.AdminCloseSubject(kioskCode),
				events.BuildAdminClosePayload(events.AdminCloseInput{
					TransactionID:  tx.Id,
					LineID:         l.Id,
					KioskCode:      kioskCode,
					LocationCode:   locationCode,
					ItemID:         itemRec.Id,
					ItemCode:       itemRec.GetString("code"),
					ItemName:       itemRec.GetString("name"),
					UserID:         userRec.Id,
					UserCode:       userRec.GetString("code"),
					UserGroup:      tx.GetString("user_group"),
					ItemInstanceID: l.GetString("item_instance"),
					Serial:         l.GetString("serial"),
					Qty:            l.GetInt("qty"),
					ClosureReason:  l.GetString("closure_reason"),
					CompletedAt:    completedAt,
					// OpenCheckoutID intentionally empty: the original
					// open_checkouts row was deleted at close time and isn't
					// recoverable. The controller's admin_close projection keys
					// its idempotency guard on LineID (stable across the live
					// path and republish) for exactly this reason.
				}))
			out.AdminClosesPublished++
			continue
		}

		var checkedOut, returned, consumed int
		for _, l := range lines {
			switch l.GetString("action") {
			case "checkout":
				checkedOut++
			case "return":
				returned++
			case "consume":
				consumed++
			}
		}

		publish(events.TransactionCompleteSubject(kioskCode),
			events.BuildTransactionCompletePayload(events.TransactionCompleteInput{
				TransactionID: tx.Id,
				KioskCode:     kioskCode,
				LocationCode:  locationCode,
				UserID:        userRec.Id,
				UserCode:      userRec.GetString("code"),
				UserName:      userRec.GetString("name"),
				UserGroup:     tx.GetString("user_group"),
				StartedAt:     tx.GetDateTime("started_at").Time(),
				CompletedAt:   completedAt,
				LinesCount:    len(lines),
				CheckedOut:    checkedOut,
				Returned:      returned,
				Consumed:      consumed,
			}))
		out.TransactionsPublished++

		for _, l := range lines {
			itemRec, err := app.FindRecordById("items", l.GetString("item"))
			if err != nil {
				continue
			}
			// Resolve original_checkout_user → code, mirroring the live commit
			// path so a republish recreates the controller's projection of
			// cross-user returns. Self-returns where the FK matches the
			// transaction's user skip the second lookup.
			var origUserCode string
			if origID := l.GetString("original_checkout_user"); origID != "" {
				if origID == userRec.Id {
					origUserCode = userRec.GetString("code")
				} else if u, err := app.FindRecordById("users", origID); err == nil {
					origUserCode = u.GetString("code")
				}
			}
			publish(events.ItemActionSubject(kioskCode, l.GetString("action")),
				events.BuildItemActionPayload(events.ItemActionInput{
					TransactionID:            tx.Id,
					LineID:                   l.Id,
					KioskCode:                kioskCode,
					LocationCode:             locationCode,
					UserID:                   userRec.Id,
					UserCode:                 userRec.GetString("code"),
					UserGroup:                tx.GetString("user_group"),
					ItemID:                   itemRec.Id,
					ItemCode:                 itemRec.GetString("code"),
					ItemName:                 itemRec.GetString("name"),
					Action:                   l.GetString("action"),
					Qty:                      l.GetInt("qty"),
					Serial:                   l.GetString("serial"),
					Uncorrelated:             l.GetBool("uncorrelated"),
					OriginalCheckoutUserCode: origUserCode,
					ItemInstanceID:           l.GetString("item_instance"),
					CompletedAt:              completedAt,
				}))
			out.LinesPublished++
		}
	}

	return out, nil
}
