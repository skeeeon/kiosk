package notifications

import (
	"fmt"
	"text/template"
	"time"
)

// FuncMap returns the helper functions exposed to templates. Three only —
// add more on operator request, not speculatively. The same map is used
// for subject and body so an operator never has to remember which helpers
// work where.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"formatTime": formatTime,
		"actionVerb": actionVerb,
		"pluralize":  pluralize,
	}
}

// formatTime renders a time in the kiosk's local timezone. Accepts
// time.Time or *time.Time so templates can pass either shape.
func formatTime(v any) string {
	t, ok := toTime(v)
	if !ok {
		return ""
	}
	return t.Local().Format("Jan 2, 2006 3:04 PM")
}

func toTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case *time.Time:
		if t == nil {
			return time.Time{}, false
		}
		return *t, true
	}
	return time.Time{}, false
}

// actionVerb turns the commit action enum into past-tense receipt copy.
// Unknown values pass through unchanged so a future action type still
// renders something readable until templates are updated.
func actionVerb(action string) string {
	switch action {
	case "checkout":
		return "checked out"
	case "return":
		return "returned"
	case "consume":
		return "consumed"
	}
	return action
}

// pluralize appends "s" to noun when n != 1. Enough for "item"/"items",
// "tool"/"tools" — does not handle "box"→"boxes" and isn't trying to.
func pluralize(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
