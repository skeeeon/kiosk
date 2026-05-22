package notifications

import (
	"testing"
	"time"
)

func TestPluralize(t *testing.T) {
	cases := []struct {
		n    int
		noun string
		want string
	}{
		{1, "item", "1 item"},
		{2, "item", "2 items"},
		{0, "tool", "0 tools"},
		{5, "tool", "5 tools"},
	}
	for _, c := range cases {
		got := pluralize(c.n, c.noun)
		if got != c.want {
			t.Errorf("pluralize(%d,%q) = %q; want %q", c.n, c.noun, got, c.want)
		}
	}
}

func TestActionVerb(t *testing.T) {
	cases := map[string]string{
		"checkout": "checked out",
		"return":   "returned",
		"consume":  "consumed",
		"unknown":  "unknown", // passthrough
	}
	for in, want := range cases {
		got := actionVerb(in)
		if got != want {
			t.Errorf("actionVerb(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFormatTime(t *testing.T) {
	// Time output is locale/TZ-sensitive; assert non-empty + contains the
	// year as a coarse sanity check rather than exact-string-match.
	in := time.Date(2026, 3, 14, 15, 4, 5, 0, time.UTC)
	got := formatTime(in)
	if got == "" {
		t.Fatal("formatTime returned empty string")
	}
	if !contains(got, "2026") {
		t.Errorf("formatTime missing year: %q", got)
	}

	if formatTime(nil) != "" {
		t.Error("formatTime(nil) should return empty string")
	}
	if formatTime("not a time") != "" {
		t.Error("formatTime(string) should return empty string")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
