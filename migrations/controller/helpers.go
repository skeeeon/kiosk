// Package controllermigrations holds the controller-only schema
// migrations. It exists as a sibling of migrations/ so the kiosk binary
// (which blank-imports migrations/) doesn't pick these up: only
// cmd/controller blank-imports this path.
//
// Each migration registers via init() the same way kiosk migrations do.
// Splitting the controller migrations into their own package is what
// makes that pattern safe — when the two sets shared a package, the
// kiosk's blank import would have run the controller migrations too.
package controllermigrations

import "github.com/pocketbase/pocketbase/core"

// adminRule duplicates the same string defined in
// migrations/1779000000_init.go. Copied unexported rather than exported
// from the kiosk migrations package so neither package grows public API
// for an internal constant.
const adminRule = "@request.auth.collectionName = 'admins'"

// Index introspection helpers duplicated unexported from
// migrations/index_helpers.go. Same reasoning as adminRule — no public
// API growth for an internal utility.

func hasIndex(c *core.Collection, name string) bool {
	for _, idx := range c.Indexes {
		if extractIndexName(idx) == name {
			return true
		}
	}
	return false
}

func removeIndex(c *core.Collection, name string) {
	out := c.Indexes[:0]
	for _, idx := range c.Indexes {
		if extractIndexName(idx) != name {
			out = append(out, idx)
		}
	}
	c.Indexes = out
}

func extractIndexName(ddl string) string {
	start := -1
	for i := 0; i < len(ddl); i++ {
		if ddl[i] == '`' {
			if start == -1 {
				start = i + 1
				continue
			}
			return ddl[start:i]
		}
	}
	return ""
}
