package notifications

import (
	"fmt"
	"strconv"
)

// Cadence values supported by the scheduled_reports collection. The SPA
// uses these as a typed enum; the cron translation lives here in Go so
// admins never have to think in cron syntax.
const (
	CadenceDaily   = "daily"
	CadenceWeekly  = "weekly"
	CadenceMonthly = "monthly"
)

// CronExpressionFor renders a typed schedule (cadence + hour + weekday or
// day_of_month) into a five-field cron expression compatible with
// pocketbase's app.Cron(). Errors describe which field is out of range so
// the SPA's schedule editor can surface them inline.
//
// Conventions:
//   - hour: 0–23 (UTC; PB's Cron uses the process timezone)
//   - weekly.weekday: 0=Sunday … 6=Saturday (cron convention)
//   - monthly.day_of_month: 1–28 (capped at 28 to avoid month-length surprises)
func CronExpressionFor(cadence string, hour, weekday, dayOfMonth int) (string, error) {
	if hour < 0 || hour > 23 {
		return "", fmt.Errorf("hour out of range: %d (expected 0–23)", hour)
	}
	switch cadence {
	case CadenceDaily:
		return "0 " + strconv.Itoa(hour) + " * * *", nil
	case CadenceWeekly:
		if weekday < 0 || weekday > 6 {
			return "", fmt.Errorf("weekday out of range: %d (expected 0–6)", weekday)
		}
		return "0 " + strconv.Itoa(hour) + " * * " + strconv.Itoa(weekday), nil
	case CadenceMonthly:
		if dayOfMonth < 1 || dayOfMonth > 28 {
			return "", fmt.Errorf("day_of_month out of range: %d (expected 1–28)", dayOfMonth)
		}
		return "0 " + strconv.Itoa(hour) + " " + strconv.Itoa(dayOfMonth) + " * *", nil
	}
	return "", fmt.Errorf("unknown cadence %q (expected daily/weekly/monthly)", cadence)
}
