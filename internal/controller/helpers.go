package controller

import (
	"encoding/json"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/skeeeon/kiosk/internal/dberr"
)

// isNotFound / isUniqueViolation delegate to internal/dberr — the single home
// for the SQLite "what does this error mean?" string-matching. Kept as
// package-local thin wrappers so existing call sites read unchanged.
func isNotFound(err error) bool       { return dberr.IsNotFound(err) }
func isUniqueViolation(err error) bool { return dberr.IsUniqueViolation(err) }

// unmarshalMsg decodes a JetStream message payload into the given target.
// Exists because we always want JSON and always want to log the subject on
// failure — DRY for the handler.
func unmarshalMsg(msg jetstream.Msg, out any) error {
	return json.Unmarshal(msg.Data(), out)
}
