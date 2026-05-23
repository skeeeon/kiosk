// Package dberr centralizes the tiny "what does this DB error mean?"
// helpers that more than one package needs. They exist because PB
// (via modernc.org/sqlite) doesn't expose typed error codes for
// constraint failures, so callers do string matching instead. Two
// helpers, both best-effort.
package dberr

import (
	"database/sql"
	"errors"
	"strings"
)

// IsNotFound reports whether err means "no row matched." PB's FindFirst*
// helpers wrap sql.ErrNoRows; that's what we test against.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows)
}

// IsUniqueViolation reports whether err looks like a UNIQUE constraint
// failure from SQLite. False negatives only delay retries — they don't
// cause data corruption, so the string match is acceptable.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "constraint failed: unique")
}
