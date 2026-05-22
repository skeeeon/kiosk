package notifications

import (
	"strings"
	"testing"
	"time"
)

// TestDefaultsRenderAgainstReceiptContext is the regression test that
// catches "operator field reference broke" — the default subject + body
// must parse and render cleanly against a representative ReceiptContext.
// If a future commit changes a struct field name or template helper, this
// test fires before users do.
func TestDefaultsRenderAgainstReceiptContext(t *testing.T) {
	ctx := ReceiptContext{
		Kiosk: KioskInfo{Code: "BAY-01", LocationCode: "WAREHOUSE-A"},
		User:  UserInfo{ID: "u1", Code: "W1234", Name: "Alex Worker", Email: "alex@example.com"},
		Transaction: TransactionInfo{
			ID:          "tx-abc",
			StartedAt:   time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC),
			CompletedAt: time.Date(2026, 3, 14, 9, 4, 0, 0, time.UTC),
			LinesCount:  2,
			CheckedOut:  1,
			Returned:    1,
		},
		Lines: []LineInfo{
			{ItemCode: "DRL-001", ItemName: "Drill", Action: "checkout", Qty: 1, Serial: "SN-A"},
			{ItemCode: "BLT-007", ItemName: "Bolts (1/4\")", Action: "consume", Qty: 12},
		},
	}

	subject, body, err := Render(DefaultReceiptSubject, DefaultReceiptBody, ctx)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if !strings.Contains(subject, "BAY-01") {
		t.Errorf("subject missing kiosk code: %q", subject)
	}
	if !strings.Contains(subject, "2 items") {
		t.Errorf("subject missing line count pluralized: %q", subject)
	}
	if !strings.Contains(body, "Alex Worker") {
		t.Errorf("body missing user name: %q", body)
	}
	if !strings.Contains(body, "checked out 1 × Drill") {
		t.Errorf("body missing first action line: %q", body)
	}
	if !strings.Contains(body, "consumed 12 × Bolts") {
		t.Errorf("body missing second action line: %q", body)
	}
	if !strings.Contains(body, "[serial: SN-A]") {
		t.Errorf("body missing serial annotation: %q", body)
	}
	if !strings.Contains(body, "tx-abc") {
		t.Errorf("body missing transaction id: %q", body)
	}
}

func TestValidateTemplates(t *testing.T) {
	t.Run("valid templates pass", func(t *testing.T) {
		err := ValidateTemplates(DefaultReceiptSubject, DefaultReceiptBody)
		if err != nil {
			t.Errorf("defaults rejected: %v", err)
		}
	})

	t.Run("malformed subject rejected", func(t *testing.T) {
		err := ValidateTemplates("{{ .Bogus ", "body")
		if err == nil {
			t.Error("expected parse error for unclosed action in subject")
		}
		if !strings.Contains(err.Error(), "subject:") {
			t.Errorf("error should mention subject: %v", err)
		}
	})

	t.Run("malformed body rejected", func(t *testing.T) {
		err := ValidateTemplates("ok", "{{range .Lines}}{{.Action}}")
		if err == nil {
			t.Error("expected parse error for unclosed range in body")
		}
		if !strings.Contains(err.Error(), "body:") {
			t.Errorf("error should mention body: %v", err)
		}
	})
}

func TestRenderRejectsBadFieldRef(t *testing.T) {
	// Bad field refs aren't caught by Parse (that's what Validate is for) —
	// they surface at execute time. Render must still return an error so
	// the notifier can log it.
	ctx := ReceiptContext{Kiosk: KioskInfo{Code: "X"}}
	_, _, err := Render("{{.NotAField}}", "ok", ctx)
	if err == nil {
		t.Error("expected execute error for unknown field reference")
	}
}

func TestRecipientForReceiptTransaction(t *testing.T) {
	got := recipientFor(EventTypeReceiptTransaction, ReceiptContext{
		User: UserInfo{Email: "  worker@example.com  "},
	})
	if got != "worker@example.com" {
		t.Errorf("recipient = %q; want trimmed worker@example.com", got)
	}

	if recipientFor(EventTypeReceiptTransaction, ReceiptContext{}) != "" {
		t.Error("empty email should produce empty recipient")
	}

	if recipientFor("nope", ReceiptContext{User: UserInfo{Email: "a@b"}}) != "" {
		t.Error("unknown event type should produce empty recipient")
	}
}

func TestDefaultsTable(t *testing.T) {
	s, b, ok := Defaults(EventTypeReceiptTransaction)
	if !ok || s == "" || b == "" {
		t.Errorf("defaults for receipt.transaction missing")
	}
	if _, _, ok := Defaults("unknown"); ok {
		t.Error("Defaults(unknown) should report not ok")
	}
}
