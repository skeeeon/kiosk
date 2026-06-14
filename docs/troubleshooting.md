# Troubleshooting

## `localhost:8090` returns `{"message":"The requested resource wasn't found."}`

The binary's embedded SPA bundle is empty. This happens when `go build`
ran against an empty `internal/ui/dist/` (e.g., a fresh clone without
`npm run build`). Rebuild in order:

```
npm run build --prefix ui      # populates internal/ui/dist/
go build ./cmd/kiosk           # //go:embed picks up the new dist
```

## "scan didn't trigger anything" during development

The `useScan` composable listens for `keydown` and dispatches on `Enter`.
It skips when an `<input>`, `<textarea>`, `<select>`, or contenteditable
element has focus. Click into the body of the page (anywhere outside an
input) and type the badge or item code, then press Enter.

## Bootstrap admin credentials weren't printed

The migration runs **once**. If your `pb_data/` already has
`_migrations` recording this migration, the seed has already happened.
Either:

- Use the superuser UI at `/_/` to reset the existing admin's password, or
- Delete `pb_data/` and start over (this wipes all data).

## Commit returns 500

Watch the binary's stdout. The commit function rolls back the DB
transaction on any error (invalid item ID, serialized item with qty > 1,
etc.) and returns the wrapped error. The cart is left intact for retry.

## A punch returns 409 (`already_clocked_in` / `not_clocked_in` / `open_checkouts`)

These are expected conflicts, not errors — the SPA branches on the
machine-readable `error` field. `already_clocked_in` / `not_clocked_in` mean
the merged state already disagrees with the requested direction (often the
worker punched at another kiosk and this kiosk's replica is fresher); the
panel refreshes so the button matches. `open_checkouts` means
`block_clock_out_with_open_checkouts` is on and the worker still holds tools —
in managed mode this is fleet-wide (tools out at any kiosk, each tagged with
the building to return it to), so the phone terminal blocks too. The body
lists them; the worker can re-check (after returning) or **clock out anyway**
to acknowledge and proceed. An admin punch with `force=true` is the override.
If the gate seems *not* to fire when it should: the replica is eventually
consistent and fail-open, so a brand-new checkout (or a controller/KV outage)
can leave it briefly permissive — it self-heals on the next projection.

## A worker can't sign in to the virtual timeclock terminal

Check, in order:

1. **Wrong binary.** Worker login only exists on `cmd/timeclock` (the
   binary-only migration enables it). A regular `cmd/kiosk` never lets workers
   authenticate.
2. **`active=false`.** The `AuthRule` is `active = true`; a deactivated worker
   can't get a session (and a still-valid token is rejected by `requireWorker`).
3. **No email on the record.** Both SSO (email matching) and password reset
   need the worker's email populated.
4. **OAuth2 "this account is not provisioned for time clock access".** The
   match-only guard rejected an IdP account whose email has no pre-provisioned
   worker. Provision the worker first (managed: on the controller; standalone:
   locally), with the matching email.
5. **Password reset email never arrives.** Provisioning seeds a random
   password, so workers must use "Forgot password" — which needs SMTP
   configured (superuser UI → Settings → Mail).

## Tests fail with `find items: sql: no rows in result set`

This means migrations didn't run before the test seeded fixtures. The
test helper in `internal/commit/commit_test.go` uses
`core.NewMigrationsRunner` to apply migrations after `app.Bootstrap()`.
If you're writing new tests against PB, follow the same pattern
(`migratecmd`'s `Automigrate` hooks `OnServe`, not `OnBootstrap`, so it
doesn't fire in tests that don't start a server).
