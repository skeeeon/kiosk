package handlers

import "testing"

func TestCrossedLowStock(t *testing.T) {
	cases := []struct {
		name      string
		prev      int
		current   int
		threshold int
		want      bool
	}{
		{"clean drop across threshold", 10, 3, 5, true},
		{"drop exactly to threshold", 8, 5, 5, true},
		{"prev already at threshold", 5, 3, 5, false},
		{"prev below threshold", 4, 2, 5, false},
		{"no change", 6, 6, 5, false},
		{"increased", 4, 6, 5, false},
		{"threshold zero disabled", 10, 0, 0, false},
		{"negative threshold (defensive)", 10, 0, -1, false},
		{"single-unit drop to threshold boundary", 6, 5, 5, true},
		{"single-unit drop within threshold", 5, 4, 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := crossedLowStock(c.prev, c.current, c.threshold); got != c.want {
				t.Errorf("crossedLowStock(prev=%d,curr=%d,th=%d) = %v; want %v",
					c.prev, c.current, c.threshold, got, c.want)
			}
		})
	}
}
