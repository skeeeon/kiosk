package commit_test

import (
	"errors"
	"testing"
	"time"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/commit"
	"github.com/skeeeon/kiosk/internal/timeclock"
)

// The timeclock interlock: carts with checkout/consume lines require the
// cart user to be clocked in (per the merge rule); return-only carts always
// pass. Whole-cart atomicity — a mixed cart is rejected outright.

func checkoutLine(s seed) *cart.Line {
	return &cart.Line{
		ItemID:       s.ToolQtyID,
		ItemCode:     "HAMMER",
		ItemName:     "Hammer",
		ItemType:     "tool",
		TrackingMode: "quantity",
		Action:       "checkout",
		Qty:          1,
	}
}

func returnLine(s seed) *cart.Line {
	return &cart.Line{
		ItemID:       s.ToolQtyID,
		ItemCode:     "HAMMER",
		ItemName:     "Hammer",
		ItemType:     "tool",
		TrackingMode: "quantity",
		Action:       "return",
		Qty:          1,
	}
}

func interlockPolicy(fleet *timeclock.Fleet) commit.Policy {
	p := commit.DefaultPolicy()
	p.RequireClockInForCheckout = true
	p.PunchFleet = fleet
	return p
}

func TestCommit_TimeclockInterlock(t *testing.T) {
	t.Run("flag off ignores punch state", func(t *testing.T) {
		app := setupApp(t)
		s := seedFixtures(t, app)
		pub := &captured{}
		if _, err := commit.Commit(app, newCart(s.UserID, checkoutLine(s)), testIdentity, commit.DefaultPolicy(), pub.publish); err != nil {
			t.Fatalf("commit with interlock off: %v", err)
		}
	})

	t.Run("checkout rejected when not clocked in", func(t *testing.T) {
		app := setupApp(t)
		s := seedFixtures(t, app)
		pub := &captured{}
		_, err := commit.Commit(app, newCart(s.UserID, checkoutLine(s)), testIdentity, interlockPolicy(nil), pub.publish)
		if !errors.Is(err, timeclock.ErrNotClockedIn) {
			t.Fatalf("got %v, want ErrNotClockedIn", err)
		}
		if len(pub.events) != 0 {
			t.Fatalf("no events should publish on a rejected commit: %v", pub.subjects())
		}
		// Nothing written.
		if n := countOpenCheckouts(t, app, "", nil); n != 0 {
			t.Fatalf("open_checkouts rows after rejection: %d", n)
		}
	})

	t.Run("mixed cart rejected whole", func(t *testing.T) {
		app := setupApp(t)
		s := seedFixtures(t, app)
		pub := &captured{}
		_, err := commit.Commit(app, newCart(s.UserID, returnLine(s), checkoutLine(s)), testIdentity, interlockPolicy(nil), pub.publish)
		if !errors.Is(err, timeclock.ErrNotClockedIn) {
			t.Fatalf("got %v, want ErrNotClockedIn", err)
		}
	})

	t.Run("returns-only cart always passes", func(t *testing.T) {
		app := setupApp(t)
		s := seedFixtures(t, app)
		pub := &captured{}
		// Uncorrelated return — allowed by the default policy; the point is
		// the interlock doesn't gate it.
		if _, err := commit.Commit(app, newCart(s.UserID, returnLine(s)), testIdentity, interlockPolicy(nil), pub.publish); err != nil {
			t.Fatalf("returns-only commit while not clocked in: %v", err)
		}
	})

	t.Run("clocked in locally passes", func(t *testing.T) {
		app := setupApp(t)
		s := seedFixtures(t, app)
		if _, err := timeclock.PerformPunch(app, nil, timeclock.Rules{}, testIdentity, timeclock.PunchInput{
			TargetUserCode: "EMP-1",
			Direction:      "in",
			Source:         timeclock.SourceSelf,
		}); err != nil {
			t.Fatalf("clock in: %v", err)
		}
		pub := &captured{}
		if _, err := commit.Commit(app, newCart(s.UserID, checkoutLine(s)), testIdentity, interlockPolicy(nil), pub.publish); err != nil {
			t.Fatalf("commit while clocked in: %v", err)
		}
	})

	t.Run("fleet replica satisfies the gate", func(t *testing.T) {
		app := setupApp(t)
		s := seedFixtures(t, app)
		fleet := timeclock.NewFleet()
		fleet.Upsert(timeclock.PunchStatePayload{
			UserCode:   "EMP-1",
			ClockedIn:  true,
			OccurredAt: time.Now().Add(-time.Minute),
		})
		pub := &captured{}
		if _, err := commit.Commit(app, newCart(s.UserID, checkoutLine(s)), testIdentity, interlockPolicy(fleet), pub.publish); err != nil {
			t.Fatalf("commit with fleet-clocked-in user: %v", err)
		}
	})
}
