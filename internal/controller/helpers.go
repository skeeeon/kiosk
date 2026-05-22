package controller

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// errNotFound returns the sentinel used to recognize "no row matched". PB's
// FindFirst* helpers wrap sql.ErrNoRows; we test with errors.Is.
func errNotFound() error { return sql.ErrNoRows }

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows)
}

// isUniqueViolation is a best-effort string match against the modernc.org
// SQLite driver's error text. PB uses SQLite under the hood; the driver
// doesn't expose typed error codes for unique constraint failures, so we
// match the message. False negatives only delay redelivery — they don't
// cause data corruption.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "constraint failed: unique")
}

// unmarshalMsg decodes a JetStream message payload into the given target.
// Exists because we always want JSON and always want to log the subject on
// failure — DRY for the handler.
func unmarshalMsg(msg jetstream.Msg, out any) error {
	return json.Unmarshal(msg.Data(), out)
}

// randomPassword returns a URL-safe base64 string with at least `nbytes` of
// entropy. Used when seeding new worker records — workers don't log in but
// PB's auth collection requires a non-empty password on create.
func randomPassword(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
