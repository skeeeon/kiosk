# Development

## Prerequisites

- **Go 1.25+** (required by PocketBase 0.38)
- **Node 20+** with npm (any recent LTS works; tested on 25)
- **A USB HID barcode scanner** for production. Any keyboard-emulating model:
  Honeywell, Zebra, generic Chinese OEM. For development, your keyboard is fine.

## Backend dev loop

```powershell
# After Go changes:
go build -o kiosk-app.exe ./cmd/kiosk
.\kiosk-app.exe
```

The binary auto-applies any new migrations on start. Migrations are tracked
in PB's `_migrations` table; running the same binary twice is a no-op.

## Frontend dev loop

For hot reload on SPA changes, run the Vite dev server alongside the Go
binary:

```powershell
# Terminal 1
.\kiosk-app.exe

# Terminal 2
npm run dev --prefix ui                   # opens http://localhost:5173
```

The Vite config proxies `/api` and `/_` to `http://127.0.0.1:8090`, so the
dev server hits your local Go binary for everything dynamic.

To produce a production SPA bundle, run the npm build then re-run `go build`.
Vite writes to `internal/ui/dist/`, which `internal/ui/embed.go` pulls into
both binaries at compile time via `//go:embed`:

```powershell
npm run build --prefix ui                 # writes to internal/ui/dist/
go build -o kiosk-app.exe ./cmd/kiosk     # embeds the dist/ bundle
```

## Resetting the database

```powershell
Remove-Item -Recurse -Force pb_data       # on Linux/Mac: rm -rf pb_data
.\kiosk-app.exe                            # re-runs migrations, re-seeds bootstrap admin
```

## Testing

```powershell
go test ./...
```

The frontend has no test suite. SPA correctness is verified by `vue-tsc`
during `npm run build`.

### Tested modules

- `internal/scan` — table-driven tests covering the resolver's dispatch
  order including the instance-before-item precedence.
- `internal/cart` — in-memory store: stacking rules, qty/action updates,
  expiry, line removal.
- `internal/commit` — the heart of the system. Integration tests spin up a
  fresh PocketBase app per case via `pocketbase.NewWithConfig` +
  `core.NewMigrationsRunner`, then exercise the state machine: checkout
  (with qty=N row creation), return (correlated and uncorrelated),
  cross-user return, consume (with `quantity_on_hand` decrement and
  negative-allowed semantics), mixed cart, rollback-on-error, serialized
  constraints, per-instance return targeting, and the policy flags
  (cross-user / uncorrelated rejection). Admin force-close covers the
  lost/damaged qty side-effect for quantity items and the serialized
  variant (retire only — no `stock_adjustments` row, two events).
- `internal/handlers` — stock-adjustment transaction (delta/absolute,
  audit row written, empty reason rejected, item-not-found,
  controller-source routes to `controller_admin_id`, idempotent replay
  by `command_id` returns prior result without re-applying, serialized
  items rejected with no row written and quantity untouched).
- `internal/instances` — the audit/lifecycle hooks plus the derived
  `quantity_on_hand` recompute: create / to_maintenance / return_to_service
  / retire / unretire track the non-retired-instance count (in_service +
  maintenance; retired excluded), non-serialized items are never touched,
  idempotent command replay doesn't double-count, and cascade-delete of the
  parent item is safe.
- `internal/events` — `Publish()` invokes any installed publisher; nil
  publisher is a no-op (NATS path covered by manual smoke).
- `internal/commands` — kiosk command dispatcher: inventory.adjust happy
  path, every validation guard (missing fields, unknown item, bad mode,
  serialized-item rejection), idempotent replay; inventory.snapshot
  all-items + filter-by-codes; subject-suffix routing.
- `internal/catalog` — payload round-trip with banned-field assertions;
  the watcher's `upsertItem`/`upsertUser` against a real PB DB (verifies
  that kiosk-local `quantity_on_hand` survives a catalog update);
  soft-delete sets `active=false` and is idempotent for unknown codes.
- `internal/controller` — idempotent transaction + line projection under
  redelivery; "parent not yet here" produces a retry; unknown user/item
  skipped with ack; `TouchKiosk` auto-registers on first sight and
  advances `last_transaction_at` on subsequent transaction.complete
  events. `KiosksForItem` / `ItemsForKiosk` membership helpers plus
  cascade-delete verification on `kiosk_items`. Heartbeat registry:
  record/snapshot, IsLikelyOnline thresholds, auto-register on first beat
  (once), malformed-payload tolerance.
