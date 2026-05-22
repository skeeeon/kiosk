# Tool/Consumable Checkout Kiosk

A self-service checkout kiosk for tool cribs and consumable storerooms. A worker
walks up to a touchscreen, scans their badge, scans items, confirms a cart, and
walks away. The same flow handles checkouts, returns, and consumable grabs in
one transaction.

The whole thing is one Go binary that embeds PocketBase and serves a Vue 3 SPA
from the same process. There's no separate API server, no separate database, no
external dependencies at runtime. `scp kiosk-app pi@kiosk:/opt/kiosk/` is the
entire deploy story.

For multi-kiosk deployments there's an optional **kiosk-controller** sibling
binary that distributes catalog (items + users) down to managed kiosks over
NATS JetStream KV and aggregates their transaction events into a central
ledger. Standalone kiosks ignore it; managed kiosks point at it via config.
See [Central controller (kiosk-controller)](#central-controller-kiosk-controller).

## Overview

- **One binary.** Go + embedded PocketBase + SQLite + built Vue SPA. ~40 MB.
- **Edge-only.** Each kiosk is autonomous. No central server required. No
  internet needed for normal operation; an optional NATS publisher can feed
  events to a central consumer when one is reachable.
- **Two auth populations.** Workers identify by badge scan (no login). Admins
  log in with email + password to manage items, workers, imports, and reports.
- **Append-only ledger.** Transactions are immutable after commit. The
  `open_checkouts` table is a materialized view rebuildable from the ledger
  (integrity diff + one-click rebuild from the admin Reports view).
- **Stock tracking with audit trail.** Items carry `quantity_on_hand` and
  `reorder_threshold`. Consumables decrement automatically on `consume`;
  admin edits go through an `/adjust` endpoint that writes a
  `stock_adjustments` audit row in the same transaction as the item update.
  A Low Stock report surfaces items at or below threshold.
- **Per-unit tracking for serialized tools.** One `items` row is the SKU;
  one `item_instances` row is the physical unit (its scannable barcode,
  printed serial, RFID tag). Returns close the exact instance scanned, so
  three impact drivers under one SKU don't blur together.
- **Federation-ready.** Every transaction is stamped with `kiosk_code` and
  `location_code`. Every state change flows through `events.Publish`, which
  always logs via slog and (when enabled) also publishes to NATS — same
  subjects, no caller changes.
- **Optional central controller.** An opt-in `kiosk-controller` binary
  (separate `cmd/controller`) aggregates per-kiosk transaction events into
  its own ledger via a JetStream durable consumer, and pushes catalog
  updates (items + users) down to managed kiosks via JetStream KV. Managed
  kiosks become read-only on catalog from the admin UI; stock adjustments
  and other kiosk-local actions remain available.

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
| Messaging  | NATS JetStream (optional) — durable streams for event aggregation, KV buckets for catalog distribution |

## Project layout

```
kiosk/
├── cmd/
│   ├── kiosk/main.go            Kiosk: PB bootstrap, config, NATS, routes, watcher
│   └── controller/main.go       Controller: PB bootstrap, JetStream consumer + KV publisher
├── internal/
│   ├── cart/                    In-memory cart store + tests
│   ├── catalog/                 Cross-fleet payload shape; kiosk-side KV watcher + projector + tests
│   ├── commit/                  Cart-to-transaction orchestrator + tests
│   ├── config/                  YAML loader + env overrides (shared between binaries)
│   ├── controller/              Controller-side aggregator, catalog publisher, seed-catalog subcommand + tests
│   ├── events/                  Publish() + NATS publisher + JetStream accessor + tests
│   ├── handlers/                HTTP handlers for /api/kiosk/* + tests
│   ├── kioskctx/                Process-global kiosk identity
│   └── scan/                    Scan resolver + tests
├── migrations/                  Schema-as-code; runs on startup
│   ├── 1779000000_init.go            Six base collections + bootstrap admin
│   ├── 1779235200_*.go               created/updated autodate fields
│   ├── 1779400000_*.go               items.quantity_on_hand / reorder_threshold
│   ├── 1779500000_*.go               item_instances + FK backfill
│   ├── 1779600000_*.go               stock_adjustments collection
│   └── 2000000000_controller_*.go    Controller-only: kiosks registry + source_* fields
├── ui/                          Vue 3 SPA source (Vite project)
│   └── src/
│       ├── components/          Dialog primitives + cart UI
│       ├── composables/         useScan, useCart, useKioskIdentity
│       ├── lib/                 api.ts (fetch wrapper), pb.ts (PB SDK)
│       ├── stores/              session (cart), auth (admin)
│       ├── views/               CheckoutView + Admin*View
│       ├── App.vue, main.ts, router.ts, types.ts
├── pb_data/                     Kiosk SQLite (gitignored, created on first run)
├── pb_data_controller/          Controller SQLite (gitignored; controller binary uses this)
├── pb_public/                   Built SPA (gitignored, created by Vite)
├── kiosk.yaml                   Kiosk config (gitignored)
├── kiosk.yaml.example           Kiosk config template
├── controller.yaml.example      Controller config template
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

controller:                    # Optional. Opt-in to central catalog sync.
  enabled: false               # When true, watches KV buckets below and projects updates locally.
  catalog_items_bucket: "catalog_items"   # JetStream KV bucket published by kiosk-controller
  catalog_users_bucket: "catalog_users"

branding:                      # Optional. Customize visual identity.
  logo_path: "./branding/logo.svg"   # Served by the binary at /branding/logo.
  tagline: "Tool & Consumable Checkout"  # Shown under the logo on the idle screen.
  primary_color: ""            # CSS color (e.g. "#059669"); empty = built-in default.

nats:                          # Optional. Off by default.
  enabled: false               # When true, events also publish to NATS.
  url: "nats://localhost:4222"
  # Use whichever auth your nats-server expects; leave blank for anonymous.
  token: ""
  username: ""
  password: ""
  credentials_file: ""         # JWT .creds file (NGS / JetStream Cloud)
  nkey_seed_file: ""           # ed25519 NKey seed
  tls_ca_file: ""
  tls_cert_file: ""
  tls_key_file: ""
  tls_insecure: false          # skip cert verification — dev only
```

The `returns.*` flags are enforced at commit time. With either flag set to
`false`, the matching transaction fails and rolls back; the cart is left
intact for the worker to fix and retry.

The `nats.*` block enables an optional publisher. The kiosk's primary job is
the local ledger, so NATS is best-effort: an unreachable server at startup
does **not** block the kiosk from booting (the connection enters a buffering
state and dials in the background). All `nats.go` auth modes are supported —
provide whichever your server expects, or leave them blank for anonymous.

The `branding/logo.svg` shipped in the repo is a generic example (Lucide wrench
+ "TOOL CRIB" wordmark). Replace it with your own PNG or SVG — point
`branding.logo_path` at the new file and restart. Leave any branding key empty
or omit the section entirely to get unbranded defaults.

The `controller.*` block opts the kiosk into central management by the
kiosk-controller. When enabled, the kiosk's admin SPA hides Add/Edit/Delete
on items and workers and shows a "Catalog managed by controller" banner;
catalog changes flow in over JetStream KV instead. Requires `nats.enabled=true`
pointing at the same broker the controller publishes to. Stock adjustments
remain available at each kiosk regardless.

### Environment overrides

Every YAML key has a `KIOSK_*` equivalent: prefix `KIOSK_`, replace dots with
underscores, uppercase. Env vars win over the file.

```
KIOSK_KIOSK_CODE=KC-YARD-03
KIOSK_KIOSK_LOCATION_CODE=YARD
KIOSK_SERVER_PORT=8091
KIOSK_RETURNS_ALLOW_CROSS_USER=false
KIOSK_BRANDING_LOGO_PATH=/etc/kiosk/yard-03.svg
KIOSK_BRANDING_TAGLINE=Yard 03 Crib
KIOSK_BRANDING_PRIMARY_COLOR=#1d4ed8
KIOSK_NATS_ENABLED=true
KIOSK_NATS_URL=nats://central.example.com:4222
KIOSK_NATS_CREDENTIALS_FILE=/etc/kiosk/nats.creds
KIOSK_CONTROLLER_ENABLED=true
KIOSK_CONTROLLER_CATALOG_ITEMS_BUCKET=catalog_items
KIOSK_CONTROLLER_CATALOG_USERS_BUCKET=catalog_users
```

Other env vars:

| Variable | Purpose |
|---|---|
| `KIOSK_CONFIG` | Path to the YAML file. Default: `kiosk.yaml` (kiosk) / `controller.yaml` (controller) |
| `KIOSK_QUIET_BOOTSTRAP` | If set, suppresses the bootstrap admin credentials print (used in tests) |
| `KIOSK_ROLE` | Set to `controller` by `cmd/controller` before config validation; relaxes the `kiosk.code` requirement. Not intended to be set by operators. |

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

- `internal/scan` — table-driven tests covering the resolver's dispatch order
  including the instance-before-item precedence.
- `internal/cart` — in-memory store: stacking rules, qty/action updates,
  expiry, line removal.
- `internal/commit` — the heart of the system. Integration tests spin up a
  fresh PocketBase app per case via `pocketbase.NewWithConfig` +
  `core.NewMigrationsRunner`, then exercise the state machine: checkout (with
  qty=N row creation), return (correlated and uncorrelated), cross-user
  return, consume (with `quantity_on_hand` decrement and negative-allowed
  semantics), mixed cart, rollback-on-error, serialized constraints,
  per-instance return targeting, and the policy flags (cross-user /
  uncorrelated rejection).
- `internal/handlers` — stock-adjustment transaction (delta/absolute, audit
  row written, empty reason rejected, item-not-found).
- `internal/events` — `Publish()` invokes any installed publisher; nil
  publisher is a no-op (NATS path covered by manual smoke).
- `internal/catalog` — payload round-trip with banned-field assertions; the
  watcher's `upsertItem`/`upsertUser` against a real PB DB (verifies that
  kiosk-local `quantity_on_hand` survives a catalog update); soft-delete
  sets `active=false` and is idempotent for unknown codes.
- `internal/controller` — idempotent transaction + line projection under
  redelivery; "parent not yet here" produces a retry; unknown user/item
  skipped with ack; `TouchKiosk` auto-registers on first sight and
  advances `last_seen` on subsequent events.

## API reference

### Custom `/api/kiosk/*` endpoints

All custom endpoints serve the kiosk checkout flow or admin operations. PB's
`/api/collections/*` is used for PB-native CRUD.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/kiosk/identity` | none | Returns `{kiosk_code, location_code, branding, max_qty, managed}` — `managed` is true when this kiosk is opted into central control |
| `POST` | `/api/kiosk/scan` | none | Resolves a raw scan to `user`, `item`, or `unknown` |
| `POST` | `/api/kiosk/cart/start` | none | Returns existing or new cart for a user code |
| `POST` | `/api/kiosk/cart/add` | none | Appends or stacks a line; computes default action |
| `PATCH` | `/api/kiosk/cart/lines/{id}` | none | Update qty and/or action on a line |
| `DELETE` | `/api/kiosk/cart/lines/{id}` | none | Remove a line |
| `POST` | `/api/kiosk/cart/cancel` | none | Discard an in-progress cart |
| `POST` | `/api/kiosk/cart/commit` | none | Promote cart to transaction + side effects + events |
| `GET` | `/api/kiosk/integrity` | admin | Diff expected vs actual `open_checkouts` |
| `POST` | `/api/kiosk/integrity/rebuild` | admin | Wipe `open_checkouts` and rebuild it from the ledger |
| `POST` | `/api/kiosk/items/import` | admin | Multipart CSV upload, upsert by `code` |
| `POST` | `/api/kiosk/items/{id}/adjust` | admin | Change `quantity_on_hand` + write a `stock_adjustments` audit row in one transaction |
| `GET` | `/api/kiosk/transactions.csv` | admin | Export completed transactions as CSV (optional `from=` / `to=` ISO8601 query params) |

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
| `item_instances` | admin | admin |
| `transactions` | admin | forbidden via API; written by commit hook only |
| `transaction_lines` | admin | forbidden via API |
| `open_checkouts` | admin | forbidden via API |
| `stock_adjustments` | admin | forbidden via API; written by `/items/{id}/adjust` only |

The kiosk checkout flow never touches PB's REST API. Every operation goes
through a custom `/api/kiosk/*` endpoint that runs in-process and bypasses
collection rules.

### Events

`internal/events.Publish(subject, payload)` is called from the commit hook
for every state change. It always emits a structured `slog.Info` line. When
`nats.enabled=true`, it also publishes the JSON-encoded payload to the same
subject via a buffering NATS connection. Errors from the NATS publish are
logged at warn level; commit paths are never blocked or failed by them.

| Trigger | Subject |
|---|---|
| Transaction completed | `kiosk.{kiosk_code}.transaction.complete` |
| Checkout line | `kiosk.{kiosk_code}.item.checkout` |
| Return line | `kiosk.{kiosk_code}.item.return` |
| Consume line | `kiosk.{kiosk_code}.item.consume` |

Subscribe locally with the `nats` CLI to confirm publishing:

```bash
nats sub "kiosk.>"
```

## Schema

Eight collections, defined as code across `migrations/*.go`. The initial
migration creates the first six; subsequent migrations add the per-instance
and audit-log collections (and a couple of fields on `items`).

| Collection | Purpose |
|---|---|
| `users` | Workers (badge-holders). PB default auth collection, real emails kept for future notifications. Workers don't log in in v1. |
| `admins` | Foremen / admins. Separate PB auth collection. Login via email + password. |
| `items` | Tools and consumables (the SKU). `tracking_mode` is `quantity` or `serialized`. Carries `quantity_on_hand` (fleet count for tools / current stock for consumables) and `reorder_threshold` (low-stock alert level; 0 disables the alert). |
| `item_instances` | One physical unit of a serialized SKU. Holds the scannable `code`, the printed `serial`, an optional `rfid_epc`, and `active`. FK to the parent `items` row. |
| `transactions` | Append-only ledger. `kiosk_code`, `location_code`, `user`, `started_at`, `completed_at`, `status`. |
| `transaction_lines` | One per item action within a transaction. `action` is `checkout`, `return`, or `consume`. Carries optional `item_instance` FK for serialized lines. |
| `open_checkouts` | Materialized view of "what's out right now." One row per unit out. Carries `item_instance` FK for serialized units. Maintained by the commit hook. |
| `stock_adjustments` | Append-only audit log of changes to `items.quantity_on_hand` made via `/api/kiosk/items/{id}/adjust`. Stores `delta`, `new_quantity` (snapshot), `reason`, and the responsible `admin`. |
| `kiosks` | **Controller-only.** Registry of every kiosk seen by the controller. Auto-populated with `status=unknown` on first event from a new `kiosk_code`; `last_seen` advances on every event. Used for fleet visibility and as the join target when expanding aggregated transactions to "which kiosk did this come from?" |

The controller's `transactions` and `transaction_lines` collections carry
two extra fields not present on standalone kiosks:
`source_kiosk_code` + `source_transaction_id` on transactions (unique pair
index, idempotency key for redelivery) and `source_line_id` on
transaction_lines (unique-when-non-empty index). These are added by the
controller-only `2000000000_controller_collections.go` migration; the
plain kiosk binary never registers it.

Cardinality rules for `open_checkouts`:

- A `checkout` line with `qty=N` for a non-serialized tool creates **N rows**.
- A `return` line with `qty=N` deletes up to **N rows** (line marked
  `uncorrelated=true` if fewer matched).
- A serialized line always has `qty=1` and carries an `item_instance` FK.
  At most one open row exists per instance at any time; returning targets
  that exact instance — sibling units of the same SKU are untouched.
- Consumables don't generate `open_checkouts` rows. Instead the commit hook
  decrements `items.quantity_on_hand` by `qty` inside the same transaction.
  The value is allowed to go negative — the ledger is authoritative, and a
  worker grabbing more than was recorded shouldn't be blocked at the kiosk.

CSV import (`POST /api/kiosk/items/import`) format:

```csv
code,name,type,unit,tracking_mode,serial,category,rfid_epc,active,notes,quantity_on_hand,reorder_threshold
DR-IMPACT-042,Impact Driver,tool,each,serialized,SN-1234,Power Tools,E280117020000042,true,,1,0
SCREW-DECK-3IN,Deck Screws 3in,consumable,box of 100,quantity,,Fasteners,,true,,25,5
```

Empty cells are nulls. `active` accepts `true|false|1|0|yes|no|y|n`. The
`quantity_on_hand` and `reorder_threshold` columns are optional; if
omitted, existing rows keep their current values and new rows default to
zero. Items are matched by `code` (upsert). Items not in the CSV are left
alone. CSV import targets `items` only — serialized SKUs created via CSV
still need their instances added through the admin UI's instances panel.

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

To rebuild: in the admin SPA, go to **Reports → Currently out** and click
**Rebuild from ledger** at the bottom of the list. A confirm modal warns
that this wipes and repopulates `open_checkouts` inside a single
transaction. Or call `POST /api/kiosk/integrity/rebuild` directly with an
admin token. The response reports `{ deleted, inserted }` counts.

### Adjusting stock

Admins should change `items.quantity_on_hand` through the admin UI's
**Adjust…** button (next to the read-only quantity in the item dialog),
not by editing the value directly. The Adjust dialog supports a signed
delta or an absolute "set to N" mode, both requiring a free-form reason.
Each submission writes a `stock_adjustments` row in the same transaction
as the item update, capturing who/what/why. Past adjustments for an item
are viewable via the **View adjustment history** link below the
threshold field.

The Low Stock tab on Reports surfaces every active item whose available
quantity is at or below its `reorder_threshold`. For serialized SKUs,
"available" is `active instance count − currently-out instance count`;
for quantity items, it's `quantity_on_hand − count(open_checkouts)`;
for consumables, just `quantity_on_hand`.

### Managing instances of serialized tools

In the admin SPA, opening (or creating + saving) a serialized item shows
an **Instances** panel below the form. Each row is one physical unit:

- **Code** — the barcode physically on the unit (e.g. `DR-042-A`). What
  workers actually scan. Wins over the SKU code on collision.
- **Serial** — the printed serial label (informational).
- **RFID EPC** — optional tag; resolves through the same scan dispatcher.
- **Active** — uncheck to retire a unit without breaking ledger history.

Deleting an instance is blocked while it has an open checkout (return it
first) and gracefully declines when the ledger holds an FK ("uncheck
Active to retire instead"). The Items list shows an `N inst` badge next
to the tracking mode for serialized rows so an admin can see at a glance
how many physical units exist per SKU.

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

## Central controller (kiosk-controller)

Optional. Stand-alone kiosks work without any of this. Bring up a controller
when you have more than one kiosk and want a single source of truth for
catalog plus a unified transaction ledger.

### What it does

- **Catalog down → kiosks.** The controller is the source of truth for
  `items` and `users`. PB record hooks on those collections publish each
  create/update/delete to two JetStream KV buckets (`catalog_items`,
  `catalog_users`). Managed kiosks watch those buckets and project changes
  into their local PB by `code` — names get renamed, workers go inactive,
  etc., all without operators touching individual kiosks. Kiosk-local state
  (`quantity_on_hand`, `reorder_threshold`) is intentionally not synced
  and survives catalog updates untouched.
- **Transactions up → controller.** Every kiosk already publishes
  `kiosk.{code}.transaction.complete` and `kiosk.{code}.item.{action}`
  when NATS is enabled. The controller runs a JetStream durable consumer
  (`controller-aggregator`) over the `KIOSK_EVENTS` stream that projects
  every incoming event into its own `transactions` / `transaction_lines`
  rows. Idempotency keys (`source_kiosk_code + source_transaction_id` on
  transactions, `source_line_id` on lines) make redelivery safe.
- **Kiosks registry.** First time the controller sees an event from a
  kiosk_code it doesn't know, a row is auto-created in the `kiosks`
  collection with `status=unknown`. `last_seen` advances on every message
  so the controller's admin UI shows fleet liveness at a glance.

What it **doesn't** do in v1 (deliberately out of scope):

- Inventory adjustments upstream (`stock_adjustments` stays kiosk-local).
- Drift detection / state-hash compare between controller and kiosk.
- A command channel (controller → kiosk: force resync, lock kiosk, etc.).
- Cross-fleet movement of serialized items.
- Tightening PB collection rules on managed kiosks (the projector uses
  the DAO, so rules don't matter; UI gating handles the admin experience).
- Low-stock reporting at the controller.

### NATS provisioning

The controller and kiosks both connect to a `nats-server` you run
separately. JetStream must be enabled (`nats-server -js`). Provision the
stream and KV buckets once, out of band:

```bash
nats stream add KIOSK_EVENTS --subjects 'kiosk.>' --retention limits --max-age 168h
nats kv add catalog_items --history 1
nats kv add catalog_users --history 1
```

(The controller will auto-create these on first start as well; the manual
form is here so operators have a record of what's provisioned.)

### Controller setup

```bash
# Build
go build -o kiosk-controller ./cmd/controller

# Config
cp controller.yaml.example controller.yaml      # set nats.url + auth

# First run: applies migrations, prints bootstrap admin creds once.
./kiosk-controller

# Or, for a one-shot CSV seed before serving:
./kiosk-controller seed-catalog --items=items.csv --users=users.csv
```

The controller binary uses the **same** `migrations/` package as the kiosk
plus one controller-only migration (`2000000000_controller_collections.go`)
that's registered explicitly by `cmd/controller/main.go` — the kiosk binary
never registers it. The controller's data dir is `pb_data_controller/` so
a kiosk and controller can co-exist in one working directory during
development without colliding.

The controller's PocketBase admin UI lives at the same paths as a kiosk's:
`/_/` for the PB superuser, `/admin/login` for the kiosk admin. Use it to
add/edit items and users; each change fans out to the KV buckets via the
record hooks, and managed kiosks project it within seconds.

### CSV seed format

`seed-catalog` reads two optional CSV files. Items use the same columns as
the kiosk's `/api/kiosk/items/import` endpoint (see below). Users:

```csv
code,name,email,role,active
WORKER-1,Alice,alice@example.com,worker,true
FOREMAN-1,Bob,bob@example.com,foreman,true
```

`--no-publish` skips the KV fan-out (useful for first-time seeding before
the broker is reachable; the next normal startup re-emits whatever needs
emitting when an admin edits a record).

### Opting a kiosk in

On each kiosk that should be managed:

```yaml
nats:
  enabled: true
  url: "nats://controller-host:4222"
  # ... matching creds

controller:
  enabled: true
  # bucket names default to catalog_items/catalog_users; only set these
  # if your operator provisioned with different names
```

Restart the kiosk. It will:

1. Connect to NATS, watch both catalog KV buckets, project the snapshot
   into local `items` and `users` (matching by `code`).
2. Publish its own transaction events to NATS as before — the controller's
   consumer picks them up automatically and the kiosk shows up in the
   controller's `kiosks` registry.
3. Hide catalog edit affordances in the admin SPA; the banner
   "Catalog managed by controller" appears on every admin page.

If `controller.enabled=true` but NATS is unreachable, the kiosk still
boots and serves checkouts against whatever catalog state it has — the
watcher logs a warning and proceeds without sync until the broker comes
back. The local ledger is always authoritative.

## Shipped since v1

These started as deferred roadmap items and are now live in the binary:

- **Stock tracking with audit log.** `items.quantity_on_hand` /
  `reorder_threshold`, automatic decrement on `consume`, low-stock report,
  `/items/{id}/adjust` endpoint with `stock_adjustments` audit table.
- **Per-instance serialized tracking.** `item_instances` collection, scan
  resolver precedence, per-instance returns, admin instances panel.
- **Returns policy enforcement.** `allow_cross_user` / `allow_uncorrelated`
  flags are honored at commit time and roll back the transaction when set
  to `false`.
- **NATS publishing.** `events.Publish` dual-publishes to NATS when
  `nats.enabled=true`; supports all `nats.go` auth modes; unreachable
  servers don't block the kiosk from booting.
- **Ledger rebuild.** Admin button + `/integrity/rebuild` endpoint repopulate
  `open_checkouts` from the ledger.
- **Central controller (MVP).** New `cmd/controller` binary with a
  JetStream durable consumer aggregating per-kiosk transactions into a
  central ledger plus PB hooks publishing item/user catalog changes down
  to managed kiosks via JetStream KV. Kiosks opt in via the `controller:`
  config block; the admin SPA gates catalog mutation affordances in
  managed mode. See [Central controller (kiosk-controller)](#central-controller-kiosk-controller).

## Roadmap

Items below are still intentionally deferred. Schema and event subjects are
in place to make them additive rather than rewrites.

- **Inventory adjust upstream.** Publish `stock_adjustments` rows as
  `kiosk.{kiosk_code}.inventory.adjust` events so the controller has a
  complete picture of qty across the fleet. Unblocks low-stock reporting
  at the controller.
- **Drift detection.** Periodic state-hash compare between controller and
  each kiosk; surface discrepancies in the controller admin UI for triage.
- **Command channel.** Controller → kiosk subjects for one-off actions
  (force resync, lock kiosk to a holding screen, push a config nudge).
- **Cross-fleet movement of serialized items.** Move a specific
  `item_instances` row from kiosk A to kiosk B with central as the
  arbiter — one serial belongs to one kiosk at a time.
- **Tighten PB collection rules in managed mode.** UI gating is the v1
  story; a follow-up could lock the collection rules themselves so a
  determined admin poking PB directly can't drift the catalog.
- **RFID reader integration.** Impinj reader publishes scans to
  `kiosk.{kiosk_code}.scan.rfid`. The scan dispatcher already resolves
  `rfid_epc` for both items and instances — no new dispatch logic needed.
- **Per-kiosk / location-aware availability.** All kiosks currently share
  the same catalog; narrow visibility to what the kiosk's location
  actually stocks.

Each of these can be evaluated on demand. None should be built until there is
a concrete user asking for it.
