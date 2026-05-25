package migrations

import "github.com/pocketbase/pocketbase/core"

// Index introspection helpers used by several kiosk-side migrations
// (1779900000, 1780000000, 1787000000, 1789000000). Lives here rather
// than alongside any one migration so it survives the controller-only
// migrations moving out of this package into migrations/controller/.
// The same helpers are duplicated unexported in that subpackage; the
// duplication is cheap and keeps both packages free of cross-coupling.

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

// extractIndexName pulls the index name out of a PB index DDL string. PB
// stores indexes as raw SQL like
//
//	CREATE UNIQUE INDEX `idx_foo` ON `bar` (`col`) WHERE ...
//
// so we look for the name between the first pair of backticks.
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
