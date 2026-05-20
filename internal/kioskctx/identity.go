// Package kioskctx exposes the kiosk's identity (kiosk_code and location_code)
// as process-global values. These are loaded once from config at startup and
// stamped onto every transaction by the commit hook — never trust the client
// to supply them.
package kioskctx

import "sync/atomic"

type Identity struct {
	KioskCode    string `json:"kiosk_code"`
	LocationCode string `json:"location_code"`
}

var current atomic.Pointer[Identity]

// Set installs the kiosk's identity. Called once at startup from main.
func Set(id Identity) {
	current.Store(&id)
}

// Get returns the kiosk's identity. Safe for concurrent reads. Returns a zero
// Identity if Set has not been called — callers should treat that as a bug.
func Get() Identity {
	if p := current.Load(); p != nil {
		return *p
	}
	return Identity{}
}
