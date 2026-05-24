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

## Tests fail with `find items: sql: no rows in result set`

This means migrations didn't run before the test seeded fixtures. The
test helper in `internal/commit/commit_test.go` uses
`core.NewMigrationsRunner` to apply migrations after `app.Bootstrap()`.
If you're writing new tests against PB, follow the same pattern
(`migratecmd`'s `Automigrate` hooks `OnServe`, not `OnBootstrap`, so it
doesn't fire in tests that don't start a server).
