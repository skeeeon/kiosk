package timeclock

import (
	"sort"
	"time"
)

// Report-time pairing of raw punches into display intervals. This is
// DISPLAY logic, not ledger state: the punch rows are the truth (and the
// payroll CSV exports them raw); intervals/totals exist so a human can read
// a screen. Deliberately no rounding, no overtime, no pay periods — that's
// downstream's job. Pure function, no I/O, table-tested.

// PunchRow is the minimal slice of a time_punches row the pairer needs.
// Callers hydrate it from PB records (kiosk) or projected rows (controller).
type PunchRow struct {
	ID         string
	UserID     string
	UserCode   string
	UserName   string
	Direction  string
	OccurredAt time.Time
	Created    time.Time // tie-break for same-occurred_at corrections
	JobCode    string    // optional job/work-order tag; meaningful on the "in" punch
	Note       string    // optional free-text annotation; may be on either punch
}

// Interval is one paired in→out stretch. Open intervals (clocked in, no out
// yet) have a zero Out and zero Duration.
type Interval struct {
	UserID   string        `json:"user_id"`
	UserCode string        `json:"user_code"`
	UserName string        `json:"user_name"`
	In       time.Time     `json:"in"`
	Out      time.Time     `json:"out,omitzero"`
	Open     bool          `json:"open,omitempty"`
	Duration time.Duration `json:"-"`
	Seconds  int64         `json:"seconds"`            // Duration for JSON consumers
	JobCode  string        `json:"job_code,omitempty"` // carried from the opening "in" punch
	// Notes are per-punch, so an interval surfaces both ends: Note from the
	// "in" punch, OutNote from the "out" punch. Either may be empty; a note
	// added on clock-out (the common case — "left early") lands in OutNote.
	Note    string `json:"note,omitempty"`
	OutNote string `json:"out_note,omitempty"`
}

// DayTotal is the summed closed-interval time for one user on one local
// calendar day. Intervals spanning midnight are split at midnight. Open is
// set when the user has an interval still running that started on (or spans
// into) this day — its time is NOT included in Seconds.
type DayTotal struct {
	UserID   string `json:"user_id"`
	UserCode string `json:"user_code"`
	UserName string `json:"user_name"`
	Date     string `json:"date"` // YYYY-MM-DD in the report's location
	Seconds  int64  `json:"seconds"`
	Open     bool   `json:"open,omitempty"`
}

// PairResult is everything a punch-history view renders.
type PairResult struct {
	Intervals    []Interval `json:"intervals"`
	Uncorrelated []PunchRow `json:"uncorrelated"` // out-without-in, or in superseded by a later in
	DayTotals    []DayTotal `json:"day_totals"`
}

// Pair walks punches per user in occurred_at order (created breaks ties —
// same rule as LatestPunch) and pairs in/out into intervals. Sequence
// anomalies from corrections degrade gracefully rather than erroring:
//
//   - "in" while an "in" is already open → the EARLIER in is uncorrelated
//     (it has no out it can claim), the new in becomes the open one.
//   - "out" with no open in → that out is uncorrelated.
//   - trailing open "in" → an Open interval (still clocked in).
//
// loc localizes day bucketing; nil means UTC.
func Pair(punches []PunchRow, loc *time.Location) PairResult {
	if loc == nil {
		loc = time.UTC
	}
	sorted := make([]PunchRow, len(punches))
	copy(sorted, punches)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Before(b.OccurredAt)
		}
		return a.Created.Before(b.Created)
	})

	res := PairResult{
		Intervals:    []Interval{},
		Uncorrelated: []PunchRow{},
		DayTotals:    []DayTotal{},
	}
	open := map[string]*PunchRow{} // userID → open "in" punch
	for i := range sorted {
		p := sorted[i]
		switch p.Direction {
		case DirectionIn:
			if prev, ok := open[p.UserID]; ok {
				res.Uncorrelated = append(res.Uncorrelated, *prev)
			}
			cp := p
			open[p.UserID] = &cp
		case DirectionOut:
			in, ok := open[p.UserID]
			if !ok {
				res.Uncorrelated = append(res.Uncorrelated, p)
				continue
			}
			d := p.OccurredAt.Sub(in.OccurredAt)
			res.Intervals = append(res.Intervals, Interval{
				UserID:   p.UserID,
				UserCode: p.UserCode,
				UserName: p.UserName,
				In:       in.OccurredAt,
				Out:      p.OccurredAt,
				Duration: d,
				Seconds:  int64(d / time.Second),
				JobCode:  in.JobCode,
				Note:     in.Note,
				OutNote:  p.Note,
			})
			delete(open, p.UserID)
		}
	}
	// Trailing open intervals, in deterministic (user-sorted) order.
	stillIn := make([]*PunchRow, 0, len(open))
	for _, p := range open {
		stillIn = append(stillIn, p)
	}
	sort.Slice(stillIn, func(i, j int) bool { return stillIn[i].UserID < stillIn[j].UserID })
	for _, p := range stillIn {
		res.Intervals = append(res.Intervals, Interval{
			UserID:   p.UserID,
			UserCode: p.UserCode,
			UserName: p.UserName,
			In:       p.OccurredAt,
			Open:     true,
			JobCode:  p.JobCode,
			Note:     p.Note,
		})
	}

	res.DayTotals = dayTotals(res.Intervals, loc)
	return res
}

// dayTotals buckets interval time per user per local calendar day, splitting
// midnight-spanning intervals at each local midnight. Open intervals
// contribute no time but flag their start day Open.
func dayTotals(intervals []Interval, loc *time.Location) []DayTotal {
	type key struct{ userID, date string }
	totals := map[key]*DayTotal{}
	ensure := func(iv Interval, date string) *DayTotal {
		k := key{iv.UserID, date}
		t, ok := totals[k]
		if !ok {
			t = &DayTotal{UserID: iv.UserID, UserCode: iv.UserCode, UserName: iv.UserName, Date: date}
			totals[k] = t
		}
		return t
	}
	dateOf := func(t time.Time) string { return t.In(loc).Format("2006-01-02") }

	for _, iv := range intervals {
		if iv.Open {
			ensure(iv, dateOf(iv.In)).Open = true
			continue
		}
		cur := iv.In
		for cur.Before(iv.Out) {
			local := cur.In(loc)
			nextMidnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			end := iv.Out
			if nextMidnight.Before(end) {
				end = nextMidnight
			}
			ensure(iv, dateOf(cur)).Seconds += int64(end.Sub(cur) / time.Second)
			cur = end
		}
	}

	out := make([]DayTotal, 0, len(totals))
	for _, t := range totals {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].UserCode < out[j].UserCode
	})
	return out
}
