# Tool/Consumable Checkout Kiosk

A self-service checkout kiosk for tool cribs and consumable storerooms. A worker
walks up to a touchscreen, scans their badge, scans items, confirms a cart, and
walks away. The same flow handles checkouts, returns, and consumable grabs in
one transaction.

The whole thing is one Go binary that embeds PocketBase and serves a Vue 3 SPA
from the same process. There's no separate API server, no separate database, no
external dependencies at runtime. `scp kiosk-app pi@kiosk:/opt/kiosk/` is the
entire deploy story.

## Overview

- **One binary.** Go + embedded PocketBase + SQLite + built Vue SPA. ~35 MB.
- **Edge-only.** Each kiosk is autonomous. No central server in v1. No internet
  required for normal operation.
- **Two auth populations.** Workers identify by badge scan (no login). Admins
  log in with email + password to manage items, workers, imports, and reports.
- **Append-only ledger.** Transactions are immutable after commit. The
  `open_checkouts` table is a materialized view rebuildable from the ledger;
  an integrity endpoint reports any drift.
- **Federation-ready.** Every transaction is stamped with `kiosk_code` and
  `location_code`. Every state change goes through a single `PublishEvent`
  function (currently logs only; v2 swaps in NATS without changing callers).

## Architecture

```
┌─────────────────────────────────────────────────┐
│              Mini-PC / Touchscreen              │
│                                                 │
│   Chromium --kiosk  →  http://localhost:8090    │
│                       ↕                         │
│         kiosk-app (Go binary)                   │
│         ├── PocketBase (REST API, hooks, /_/)   │
│         ├── /api/kiosk/* custom routes          │
│         └── Vue SPA served from pb_public/      │
│                                                 │
│              ↑                                  │
│      USB Barcode Scanner (HID keyboard)         │
└─────────────────────────────────────────────────┘
```

The barcode scanner is a USB HID keyboard. The browser captures keystrokes via
a window-level listener that buffers characters and dispatches on Enter. Same
mechanism reads user QR codes and item barcodes; the dispatcher disambiguates
by configurable prefix or by trying items first, then users.

## Tech stack

| Layer | Choice |
|---|---|
| Server | Go 1.25 (single binary) |
| Database | PocketBase 0.38 + SQLite (file at `pb_data/data.db`) |
| Migrations | Schema-as-code in Go (`migrations/*.go`), run on startup |
| Config | YAML + `KIOSK_*` env-var overrides |
| Frontend | Vue 3 (Composition API, `<script setup>`) + TypeScript |
| State | Pinia |
| Routing | Vue Router |
| Styling | Tailwind 4 |
| Headless components | Reka UI (accessibility primitives) |
| Build | Vite 6 → `pb_public/` |
| API client | PocketBase JS SDK 0.21 (admin CRUD); plain `fetch` (kiosk flow) |

## Project layout

```
kiosk/
├── cmd/kiosk/main.go            PB bootstrap, config load, route registration
├── internal/
│   ├── cart/                    In-memory cart store + tests
│   ├── commit/                  Cart-to-transaction orchestrator + tests
│   ├── config/                  YAML loader + env overrides
│   ├── events/                  PublishEvent stub (slog only in v1)
│   ├── handlers/                HTTP handlers for /api/kiosk/*
│   ├── kioskctx/                Process-global kiosk identity
│   └── scan/                    Scan resolver + tests
├── migrations/1779000000_init.go  All six collections + bootstrap admin
├── ui/                          Vue 3 SPA source (Vite project)
│   └── src/
│       ├── components/          Dialog primitives + cart UI
│       ├── composables/         useScan, useCart, useKioskIdentity
│       ├── lib/                 api.ts (fetch wrapper), pb.ts (PB SDK)
│       ├── stores/              session (cart), auth (admin)
│       ├── views/               CheckoutView + Admin*View
│       ├── App.vue, main.ts, router.ts, types.ts
├── pb_data/                     SQLite + assets (gitignored, created on first run)
├── pb_public/                   Built SPA (gitignored, created by Vite)
├── kiosk.yaml                   Local config (gitignored)
├── kiosk.yaml.example           Template config
└── go.mod, go.sum
```

## Prerequisites

- **Go 1.25+** (required by PocketBase 0.38)
- **Node 20+** with npm (any recent LTS works; tested on 25)
- **A USB HID barcode scanner** for production. Any keyboard-emulating model:
  Honeywell, Zebra, generic Chinese OEM. For development, your keyboard is fine.

## Quick start

```powershell
# 1. Clone, then from the project root:
go mod download

# 2. Build the SPA into pb_public/
npm install --prefix ui
npm run build --prefix ui

# 3. Build the Go binary
go build -o kiosk-app.exe ./cmd/kiosk     # on Linux/Mac: go build -o kiosk-app ./cmd/kiosk

# 4. Copy and customize the config
cp kiosk.yaml.example kiosk.yaml          # edit kiosk.code / location_code

# 5. Run
.\kiosk-app.exe                            # ./kiosk-app on Linux/Mac
```

On first boot the migration creates all collections and seeds one bootstrap
admin. The credentials are printed **once** to stdout:

```
================================================================
 BOOTSTRAP ADMIN CREDENTIALS — shown once, save them now
----------------------------------------------------------------
   email:    admin@kiosk.local
   password: <16-char base64 string>
----------------------------------------------------------------
 Sign in at /admin/login. Recovery: PB superuser UI at /_/.
================================================================
```

Save the password. If you miss it, you can reset it via the PocketBase
superuser UI at `http://localhost:8090/_/` (you'll be prompted to create a
superuser the first time you visit).

Open `http://localhost:8090/`:

- The kiosk checkout flow ("Scan your badge to begin").
- Top-right header has a tiny **Admin** link → `/admin/login`.

## Configuration

`kiosk.yaml`:

```yaml
kiosk:
  code: "KC-MAIN-01"           # Stable identity. Stamped on every transaction.
  location_code: "MAIN"        # Site/yard identifier.

server:
  port: 8090
  bind: "127.0.0.1"            # Localhost only — the touchscreen is the only client.

session:
  idle_timeout: "5m"           # In-memory carts expire after this much inactivity.
  cart_grace_period: "30s"     # Success screen duration (frontend constant).

scanning:
  user_qr_prefix: "U:"         # Optional. If set and a scan starts with this,
                               # it's resolved as a user only.
  item_barcode_prefix: ""      # Optional. Same idea for items.

returns:
  allow_cross_user: true       # Bob can return Alice's tool (with UI warning).
  allow_uncorrelated: true     # Accept returns of items not currently checked out.
```

### Environment overrides

Every YAML key has a `KIOSK_*` equivalent: prefix `KIOSK_`, replace dots with
underscores, uppercase. Env vars win over the file.

```
KIOSK_KIOSK_CODE=KC-YARD-03
KIOSK_KIOSK_LOCATION_CODE=YARD
KIOSK_SERVER_PORT=8091
KIOSK_RETURNS_ALLOW_CROSS_USER=false
```

Other env vars:

| Variable | Purpose |
|---|---|
| `KIOSK_CONFIG` | Path to the YAML file. Default: `kiosk.yaml` |
| `KIOSK_QUIET_BOOTSTRAP` | If set, suppresses the bootstrap admin credentials print (used in tests) |

Deployment pattern: one `kiosk.yaml` checked into config management with
sensible defaults; per-kiosk env vars set in the host's service definition.

## Development

### Backend dev loop

```powershell
# After Go changes:
go build -o kiosk-app.exe ./cmd/kiosk
.\kiosk-app.exe
```

The binary auto-applies any new migrations on start. Migrations are tracked
in PB's `_migrations` table; running the same binary twice is a no-op.

### Frontend dev loop

For hot reload on SPA changes, run the Vite dev server alongside the Go binary:

```powershell
# Terminal 1
.\kiosk-app.exe

# Terminal 2
npm run dev --prefix ui                   # opens http://localhost:5173
```

The Vite config proxies `/api` and `/_` to `http://127.0.0.1:8090`, so the
dev server hits your local Go binary for everything dynamic.

To produce a production SPA bundle for the binary to serve:

```powershell
npm run build --prefix ui                 # writes to pb_public/
```

### Resetting the database

```powershell
Remove-Item -Recurse -Force pb_data       # on Linux/Mac: rm -rf pb_data
.\kiosk-app.exe                            # re-runs migrations, re-seeds bootstrap admin
```

## Testing

```powershell
go test ./...
```

Tested modules:

- `internal/scan` — table-driven tests covering the resolver's dispatch order:
  user-prefix routing, item-prefix routing, no-prefix fallback chain, unknown.
- `internal/cart` — in-memory store: stacking rules, qty/action updates,
  expiry, line removal.
- `internal/commit` — the heart of the system. Integration tests spin up a
  fresh PocketBase app per case via `pocketbase.NewWithConfig` +
  `core.NewMigrationsRunner`, then exercise the state machine: checkout (with
  qty=N row creation), return (correlated and uncorrelated), cross-user
  return, consume, mixed cart, rollback-on-error, and serialized constraints.

## API reference

### Custom `/api/kiosk/*` endpoints

All custom endpoints serve the kiosk checkout flow or admin operations. PB's
`/api/collections/*` is used for PB-native CRUD.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/kiosk/identity` | none | Returns `{kiosk_code, location_code}` from config |
| `POST` | `/api/kiosk/scan` | none | Resolves a raw scan to `user`, `item`, or `unknown` |
| `POST` | `/api/kiosk/cart/start` | none | Returns existing or new cart for a user code |
| `POST` | `/api/kiosk/cart/add` | none | Appends or stacks a line; computes default action |
| `PATCH` | `/api/kiosk/cart/lines/{id}` | none | Update qty and/or action on a line |
| `DELETE` | `/api/kiosk/cart/lines/{id}` | none | Remove a line |
| `POST` | `/api/kiosk/cart/cancel` | none | Discard an in-progress cart |
| `POST` | `/api/kiosk/cart/commit` | none | Promote cart to transaction + side effects + events |
| `GET` | `/api/kiosk/integrity` | admin | Diff expected vs actual `open_checkouts` |
| `POST` | `/api/kiosk/items/import` | admin | Multipart CSV upload, upsert by `code` |

Admin endpoints require a token from the `admins` auth collection in the
`Authorization` header (the PB SDK handles this automatically after login).

The kiosk checkout endpoints are anonymous on the assumption that the kiosk is
physically secured and bound to `127.0.0.1`. Worker identification happens at
the application layer via badge scan — `code` resolves to a `users` record.

### PocketBase collection rules

| Collection | List/View | Create/Update/Delete |
|---|---|---|
| `users` (workers) | admin | admin |
| `admins` | self only | superuser only (via `/_/`) |
| `items` | admin | admin |
| `transactions` | admin | forbidden via API; written by commit hook only |
| `transaction_lines` | admin | forbidden via API |
| `open_checkouts` | admin | forbidden via API |

The kiosk checkout flow never touches PB's REST API. Every operation goes
through a custom `/api/kiosk/*` endpoint that runs in-process and bypasses
collection rules.

### Events

`internal/events.Publish(subject, payload)` is called from the commit hook
for every state change. v1 emits structured log lines via `slog.Info`. v2 will
add NATS publishing here (one file, zero changes upstream).

| Trigger | Subject |
|---|---|
| Transaction completed | `kiosk.{kiosk_code}.transaction.complete` |
| Checkout line | `kiosk.{kiosk_code}.item.checkout` |
| Return line | `kiosk.{kiosk_code}.item.return` |
| Consume line | `kiosk.{kiosk_code}.item.consume` |

## Schema

Six collections, all defined as code in `migrations/1779000000_init.go`.

| Collection | Purpose |
|---|---|
| `users` | Workers (badge-holders). PB default auth collection, real emails kept for future notifications. Workers don't log in in v1. |
| `admins` | Foremen / admins. Separate PB auth collection. Login via email + password. |
| `items` | Tools and consumables. `tracking_mode` is `quantity` or `serialized`. |
| `transactions` | Append-only ledger. `kiosk_code`, `location_code`, `user`, `started_at`, `completed_at`, `status`. |
| `transaction_lines` | One per item action within a transaction. `action` is `checkout`, `return`, or `consume`. |
| `open_checkouts` | Materialized view of "what's out right now." One row per unit out. Maintained by the commit hook. |

Cardinality rules for `open_checkouts`:

- A `checkout` line with `qty=N` for a non-serialized tool creates **N rows**.
- A `return` line with `qty=N` deletes up to **N rows** (line marked `uncorrelated=true` if fewer matched).
- Serialized tools always have `qty=1`, so there's exactly one row per serialized unit at any time.
- Consumables don't generate `open_checkouts` rows.

CSV import (`POST /api/kiosk/items/import`) format:

```csv
code,name,type,unit,tracking_mode,serial,category,rfid_epc,active,notes
DR-IMPACT-042,Impact Driver,tool,each,serialized,SN-1234,Power Tools,E280117020000042,true,
SCREW-DECK-3IN,Deck Screws 3in,consumable,box of 100,quantity,,Fasteners,,true,
```

Empty cells are nulls. `active` accepts `true|false|1|0|yes|no|y|n`. Items are
matched by `code` (upsert). Items not in the CSV are left alone.

## Operations

### Deploying a new kiosk

Minimum viable deploy:

```bash
# On the kiosk host (Linux example)
mkdir -p /opt/kiosk
cp kiosk-app /opt/kiosk/
cp kiosk.yaml /opt/kiosk/                 # customize kiosk.code / location_code
cd /opt/kiosk && ./kiosk-app
```

The binary creates `pb_data/` next to itself on first run, applies migrations,
and prints the bootstrap admin credentials. Save them, then point Chromium (or
any browser, in kiosk mode if appropriate) at `http://localhost:8090/`.

To auto-start on boot, wrap the binary in whatever supervisor your host uses
(systemd, runit, OpenRC, Windows service, etc.). The binary needs:

- A working directory containing `kiosk.yaml` (or `KIOSK_CONFIG=/path/to/yaml`)
- Write access to that directory (for `pb_data/`)
- A reachable port (default 8090, bound to `127.0.0.1`)

### Backups

The SQLite file at `pb_data/data.db` is the entire kiosk state.

```bash
# Simple hot copy — safe enough for a low-write system like this:
cp /opt/kiosk/pb_data/data.db /backups/data-$(date +%Y%m%d-%H).db
```

For belt-and-suspenders, use SQLite's online backup:

```bash
sqlite3 /opt/kiosk/pb_data/data.db ".backup /backups/data-$(date +%Y%m%d-%H).db"
```

Schedule it however your host schedules things. Hourly is plenty for a tool
crib; the kiosk write rate is human-paced.

### Verifying ledger integrity

After any suspected hiccup (power loss, manual DB edit, hook bug), an admin
can hit `GET /api/kiosk/integrity` (via the admin UI's PB SDK auth) and
inspect the diff. Clean ledger:

```json
{
  "checked_lines": 247,
  "expected_open": 18,
  "actual_open": 18,
  "missing_in_table": [],
  "extra_in_table": []
}
```

If `missing_in_table` or `extra_in_table` is non-empty, the `transaction_lines`
ledger is authoritative — the `open_checkouts` table can be rebuilt from it.

### Resetting the bootstrap admin password

The bootstrap admin email is `admin@kiosk.local`. If you've lost the password:

1. Open `http://localhost:8090/_/` and sign in as the PocketBase superuser.
2. Open the `admins` collection, find the `admin@kiosk.local` record, click it.
3. Set a new password, save.

The superuser at `/_/` is separate from the kiosk's `admins` collection. The
superuser is created interactively on first visit to `/_/` and is for managing
PocketBase itself (collections, settings, system tables).

## Troubleshooting

### `localhost:8090` returns `{"message":"The requested resource wasn't found."}`

The Go binary didn't find `pb_public/`. Run `npm run build --prefix ui` and
restart. The build outputs the SPA into `pb_public/` next to the binary.

### "scan didn't trigger anything" during development

The `useScan` composable listens for `keydown` and dispatches on `Enter`. It
skips when an `<input>`, `<textarea>`, `<select>`, or contenteditable element
has focus. Click into the body of the page (anywhere outside an input) and
type the badge or item code, then press Enter.

### Bootstrap admin credentials weren't printed

The migration runs **once**. If your `pb_data/` already has `_migrations`
recording this migration, the seed has already happened. Either:

- Use the superuser UI at `/_/` to reset the existing admin's password, or
- Delete `pb_data/` and start over (this wipes all data).

### Commit returns 500

Watch the binary's stdout. The commit function rolls back the DB transaction
on any error (invalid item ID, serialized item with qty > 1, etc.) and
returns the wrapped error. The cart is left intact for retry.

### Tests fail with `find items: sql: no rows in result set`

This means migrations didn't run before the test seeded fixtures. The test
helper in `internal/commit/commit_test.go` uses `core.NewMigrationsRunner` to
apply migrations after `app.Bootstrap()`. If you're writing new tests against
PB, follow the same pattern (`migratecmd`'s `Automigrate` hooks `OnServe`, not
`OnBootstrap`, so it doesn't fire in tests that don't start a server).

## Roadmap

Each item below is intentionally deferred from v1. Schema, identity stamping,
event subjects, and the `rfid_epc` field are already in place to make these
additions additive rather than rewrites.

- **NATS publishing (v2).** Replace the body of `events.Publish` to also
  publish to NATS. Subject names already follow NATS hierarchical naming.
- **Central reporting (v2.1).** A central PocketBase subscribes to
  `kiosk.>.transaction.complete` and mirrors transactions into its own
  collection for cross-kiosk reporting.
- **Catalog sync (v2.2).** Items authored centrally, distributed to kiosks
  via NATS. The CSV import remains as a fallback for kiosk-local additions.
- **RFID (v2.3).** Impinj reader publishes scans to
  `kiosk.{kiosk_code}.scan.rfid`. The scan dispatcher already resolves
  `rfid_epc` lookups — no new dispatch logic needed.
- **Multi-kiosk site coordination (v3).** NATS leaf node per site, shared KV
  for "tool out at kiosk A visible at kiosk B."

Each of these can be evaluated on demand. None should be built until there is
a concrete user asking for it.
