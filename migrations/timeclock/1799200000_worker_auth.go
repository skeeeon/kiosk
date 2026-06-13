// Package timeclockmigrations holds schema changes that exist ONLY on the
// virtual timeclock terminal (the cmd/timeclock binary). Like
// migrations/controller, it lives in a sibling package that only that one
// binary blank-imports, so regular kiosks and the controller never enable
// worker login.
//
// What this adds, on the virtual terminal's DB only:
//   - users.AuthRule = "active = true" — turns the worker auth collection
//     into something that can actually authenticate, gated to active workers.
//     (PB's default for an auth collection is the always-allow "" rule; we
//     tighten it so a deactivated worker's session can't be minted.)
//   - users OAuth2 enabled — so workers can sign in via the org IdP (SSO).
//     Provider clientId/secret are deployment secrets configured at deploy
//     time via the superuser UI, NOT committed here. Password auth is left at
//     PB's default (enabled, identity = email); workers without SSO set a
//     password via the reset-by-email flow (the catalog watcher seeds a
//     random one nobody knows).
//
// The match-only guard that rejects logins for emails with no pre-provisioned
// worker is a runtime hook in cmd/timeclock (OnRecordAuthWithOAuth2Request),
// not schema, so it lives with the binary.
package timeclockmigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(enableWorkerAuthUp, enableWorkerAuthDown)
}

func enableWorkerAuthUp(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}
	rule := "active = true"
	users.AuthRule = &rule
	users.OAuth2.Enabled = true
	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}

func enableWorkerAuthDown(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return nil
	}
	// Restore PB's default always-allow auth rule and disable OAuth2.
	empty := ""
	users.AuthRule = &empty
	users.OAuth2.Enabled = false
	if err := app.Save(users); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}
