package timeclock_test

import (
	"testing"
	"time"

	"github.com/skeeeon/kiosk/internal/timeclock"
)

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	out, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return out
}

func punch(id, user, direction string, occurred time.Time, created time.Time) timeclock.PunchRow {
	return timeclock.PunchRow{
		ID:         id,
		UserID:     user,
		UserCode:   user,
		UserName:   user,
		Direction:  direction,
		OccurredAt: occurred,
		Created:    created,
	}
}

func TestPair_SimpleInterval(t *testing.T) {
	in := ts(t, "2026-06-11T07:00:00Z")
	out := ts(t, "2026-06-11T15:30:00Z")
	res := timeclock.Pair([]timeclock.PunchRow{
		punch("p2", "bob", "out", out, out),
		punch("p1", "bob", "in", in, in),
	}, time.UTC)

	if len(res.Intervals) != 1 || len(res.Uncorrelated) != 0 {
		t.Fatalf("got %d intervals, %d uncorrelated", len(res.Intervals), len(res.Uncorrelated))
	}
	iv := res.Intervals[0]
	if iv.Seconds != int64(8*3600+30*60) {
		t.Fatalf("seconds: got %d", iv.Seconds)
	}
	if len(res.DayTotals) != 1 || res.DayTotals[0].Date != "2026-06-11" || res.DayTotals[0].Seconds != iv.Seconds {
		t.Fatalf("day totals: %+v", res.DayTotals)
	}
}

func TestPair_OpenInterval(t *testing.T) {
	in := ts(t, "2026-06-11T07:00:00Z")
	res := timeclock.Pair([]timeclock.PunchRow{punch("p1", "bob", "in", in, in)}, time.UTC)
	if len(res.Intervals) != 1 || !res.Intervals[0].Open {
		t.Fatalf("expected one open interval: %+v", res.Intervals)
	}
	if len(res.DayTotals) != 1 || !res.DayTotals[0].Open || res.DayTotals[0].Seconds != 0 {
		t.Fatalf("open day total: %+v", res.DayTotals)
	}
}

func TestPair_DoubleInAndOrphanOut(t *testing.T) {
	t1 := ts(t, "2026-06-11T07:00:00Z")
	t2 := ts(t, "2026-06-11T08:00:00Z")
	t3 := ts(t, "2026-06-11T12:00:00Z")
	t0 := ts(t, "2026-06-10T18:00:00Z")
	res := timeclock.Pair([]timeclock.PunchRow{
		punch("orphan-out", "bob", "out", t0, t0), // out with no in
		punch("in-1", "bob", "in", t1, t1),        // superseded by in-2
		punch("in-2", "bob", "in", t2, t2),
		punch("out-1", "bob", "out", t3, t3), // pairs with in-2
	}, time.UTC)

	if len(res.Intervals) != 1 {
		t.Fatalf("intervals: %+v", res.Intervals)
	}
	if !res.Intervals[0].In.Equal(t2) {
		t.Fatalf("interval should start at the LATER in: %+v", res.Intervals[0])
	}
	if len(res.Uncorrelated) != 2 {
		t.Fatalf("uncorrelated: %+v", res.Uncorrelated)
	}
}

func TestPair_MidnightSplit(t *testing.T) {
	in := ts(t, "2026-06-11T22:00:00Z")
	out := ts(t, "2026-06-12T02:00:00Z")
	res := timeclock.Pair([]timeclock.PunchRow{
		punch("p1", "bob", "in", in, in),
		punch("p2", "bob", "out", out, out),
	}, time.UTC)

	if len(res.DayTotals) != 2 {
		t.Fatalf("expected the interval split across two days: %+v", res.DayTotals)
	}
	if res.DayTotals[0].Date != "2026-06-11" || res.DayTotals[0].Seconds != 2*3600 {
		t.Fatalf("day 1: %+v", res.DayTotals[0])
	}
	if res.DayTotals[1].Date != "2026-06-12" || res.DayTotals[1].Seconds != 2*3600 {
		t.Fatalf("day 2: %+v", res.DayTotals[1])
	}
}

func TestPair_SameTimestampTieBreakOnCreated(t *testing.T) {
	// Two punches share occurred_at (a same-moment correction); the one
	// recorded later must sort later, so the sequence reads in → out.
	at := ts(t, "2026-06-11T07:00:00Z")
	res := timeclock.Pair([]timeclock.PunchRow{
		punch("out-corr", "bob", "out", at, ts(t, "2026-06-11T09:00:00Z")), // recorded later
		punch("in-live", "bob", "in", at, ts(t, "2026-06-11T07:00:00Z")),
	}, time.UTC)
	if len(res.Intervals) != 1 || res.Intervals[0].Open {
		t.Fatalf("expected one closed zero-length interval: %+v", res.Intervals)
	}
	if len(res.Uncorrelated) != 0 {
		t.Fatalf("uncorrelated: %+v", res.Uncorrelated)
	}
}

func TestPair_JobCodeCarriesFromInPunch(t *testing.T) {
	in := ts(t, "2026-06-11T07:00:00Z")
	out := ts(t, "2026-06-11T15:00:00Z")
	openIn := ts(t, "2026-06-11T16:00:00Z")
	res := timeclock.Pair([]timeclock.PunchRow{
		// Closed interval: the job rides the "in"; the "out" carries none.
		{ID: "in-1", UserID: "bob", UserCode: "bob", Direction: "in", OccurredAt: in, Created: in, JobCode: "WO-1234"},
		{ID: "out-1", UserID: "bob", UserCode: "bob", Direction: "out", OccurredAt: out, Created: out},
		// Trailing open interval with its own job.
		{ID: "in-2", UserID: "bob", UserCode: "bob", Direction: "in", OccurredAt: openIn, Created: openIn, JobCode: "WO-5678"},
	}, time.UTC)

	if len(res.Intervals) != 2 {
		t.Fatalf("expected 2 intervals: %+v", res.Intervals)
	}
	var closed, open timeclock.Interval
	for _, iv := range res.Intervals {
		if iv.Open {
			open = iv
		} else {
			closed = iv
		}
	}
	if closed.JobCode != "WO-1234" {
		t.Fatalf("closed interval job: got %q want WO-1234", closed.JobCode)
	}
	if open.JobCode != "WO-5678" {
		t.Fatalf("open interval job: got %q want WO-5678", open.JobCode)
	}
}

func TestPair_MultiUserIsolation(t *testing.T) {
	res := timeclock.Pair([]timeclock.PunchRow{
		punch("a-in", "alice", "in", ts(t, "2026-06-11T07:00:00Z"), ts(t, "2026-06-11T07:00:00Z")),
		punch("b-out", "bob", "out", ts(t, "2026-06-11T08:00:00Z"), ts(t, "2026-06-11T08:00:00Z")),
	}, time.UTC)
	// Alice's in must NOT pair with Bob's out.
	if len(res.Intervals) != 1 || !res.Intervals[0].Open || res.Intervals[0].UserID != "alice" {
		t.Fatalf("intervals: %+v", res.Intervals)
	}
	if len(res.Uncorrelated) != 1 || res.Uncorrelated[0].UserID != "bob" {
		t.Fatalf("uncorrelated: %+v", res.Uncorrelated)
	}
}
