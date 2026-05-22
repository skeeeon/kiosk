package notifications

import "testing"

func TestCronExpressionFor(t *testing.T) {
	cases := []struct {
		name       string
		cadence    string
		hour       int
		weekday    int
		dayOfMonth int
		want       string
		wantErr    bool
	}{
		{"daily 8am", CadenceDaily, 8, 0, 0, "0 8 * * *", false},
		{"daily midnight", CadenceDaily, 0, 0, 0, "0 0 * * *", false},
		{"weekly Mon 9am", CadenceWeekly, 9, 1, 0, "0 9 * * 1", false},
		{"weekly Sun 3pm", CadenceWeekly, 15, 0, 0, "0 15 * * 0", false},
		{"monthly 1st 8am", CadenceMonthly, 8, 0, 1, "0 8 1 * *", false},
		{"monthly 28th 23h", CadenceMonthly, 23, 0, 28, "0 23 28 * *", false},

		{"hour too low", CadenceDaily, -1, 0, 0, "", true},
		{"hour too high", CadenceDaily, 24, 0, 0, "", true},
		{"weekday too low", CadenceWeekly, 8, -1, 0, "", true},
		{"weekday too high", CadenceWeekly, 8, 7, 0, "", true},
		{"day_of_month zero", CadenceMonthly, 8, 0, 0, "", true},
		{"day_of_month too high", CadenceMonthly, 8, 0, 29, "", true},
		{"unknown cadence", "yearly", 8, 0, 1, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CronExpressionFor(c.cadence, c.hour, c.weekday, c.dayOfMonth)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got cron %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
