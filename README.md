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
binary that distributes catalog down to managed kiosks over NATS JetStream
KV (items are per-kiosk via a `kiosk_items` membership table; users are
org-wide) and aggregates their transaction events into a central ledger.
Standalone kiosks ignore it; managed kiosks point at it via config.
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
  updates down to managed kiosks via JetStream KV. Item delivery is
  **membership-driven**: a `kiosk_items` join collection on the controller
  decides which SKUs each kiosk stocks, and item keys are namespaced
  `<kiosk_code>.<item_code>` so each kiosk only ever receives its own
  slice. Users remain org-wide. Managed kiosks become read-only on catalog
  from the admin UI; kiosk-local actions remain available.
- **Controller→kiosk command bus.** Over the same NATS connection, the
  controller can drive admin commands at a remote kiosk via core NATS
  request/reply (not JetStream — single attempt, fail fast when the kiosk
  is offline). v1 ships two commands: `inventory.adjust` (mutating,
  idempotent via a server-generated `command_id`) and `inventory.snapshot`
  (read-only). Same business logic and same `inventory.adjust` audit event
  as a local adjust; the controller-side audit trail records `source` and
  `controller_admin_id`.
- **Heartbeat + online status.** Each managed kiosk publishes a
  fire-and-forget heartbeat every 45 s on `<prefix>.<kiosk_code>.heartbeat`
  (core NATS, no JetStream). The controller keeps an in-memory map and
  serves it at `GET /api/controller/kiosks/heartbeats`, so the controller
  admin SPA shows three-state online/stale/offline badges that don't depend
  on the kiosk having actually transacted recently.

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
by configurable prefix or by trying instance code → item code → instance RFID
→ user code. RFID is instance-only — EPCs are per-tag and live on
`item_instances`, never on the SKU.

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
│   ├── commands/                Kiosk-side NATS command dispatcher; inventory.adjust + inventory.snapshot handlers + tests
│   ├── commit/                  Cart-to-transaction orchestrator + tests
│   ├── config/                  YAML loader + env overrides (shared between binaries)
│   ├── controller/              Controller-side aggregator, catalog publisher, membership helpers, heartbeat registry, inventory command endpoints, seed-catalog subcommand + tests
│   ├── dberr/                   Tiny shared helpers: IsNotFound, IsUniqueViolation
│   ├── events/                  Publish() + NATS publisher + JetStream/Conn accessors + tests
│   ├── handlers/                HTTP handlers for /api/kiosk/* + tests
│   ├── heartbeat/               Kiosk-side periodic heartbeat goroutine
│   ├── kioskctx/                Process-global kiosk identity
│   └── scan/                    Scan resolver + tests
├── migrations/                  Schema-as-code; runs on startup
│   ├── 1779000000_init.go                       Six base collections + bootstrap admin
│   ├── 1779235200_*.go                          created/updated autodate fields
│   ├── 1779400000_*.go                          items.quantity_on_hand / reorder_threshold
│   ├── 1779500000_*.go                          item_instances + FK backfill
│   ├── 1779600000_*.go                          stock_adjustments collection
│   ├── 1779700000_*.go                          transactions.lines_count denormalization
│   ├── 1779800000_*.go                          Drop vestigial items.rfid_epc / items.serial
│   ├── 1787000000_stock_adjust_remote.go        source / controller_admin_id / command_id on stock_adjustments
│   ├── 2000000000_controller_*.go               Controller-only: kiosks registry + source_* fields
│   ├── 2000100000_add_kiosk_items.go            Controller-only: kiosk_items membership + open kiosks.CreateRule
│   └── 2000200000_kiosks_last_transaction_at.go Controller-only: kiosks.last_transaction_at
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

In addition to the kill-switch flags, two role-based rules always apply at
commit time:

- **Cross-user return** (active worker returning a tool checked out to a
  different worker) requires the active worker to be a `foreman` whose
  `group` matches the original checkout user's group. Both groups must be
  non-empty. An ungrouped foreman, or a foreman in a different group, is
  rejected — the admin handles that case.
- **Uncorrelated return** (no matching open checkout) requires the active
  worker to be a `foreman`. Group is irrelevant; this is a janitorial
  action.

These rules apply regardless of the kill-switch flags; setting
`allow_cross_user: false` simply short-circuits the foreman check by
rejecting any cross-user return outright.

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
  row written, empty reason rejected, item-not-found, controller-source
  routes to `controller_admin_id`, idempotent replay by `command_id`
  returns prior result without re-applying).
- `internal/events` — `Publish()` invokes any installed publisher; nil
  publisher is a no-op (NATS path covered by manual smoke).
- `internal/commands` — kiosk command dispatcher: inventory.adjust happy
  path, every validation guard (missing fields, unknown item, bad mode),
  idempotent replay; inventory.snapshot all-items + filter-by-codes;
  subject-suffix routing.
- `internal/catalog` — payload round-trip with banned-field assertions; the
  watcher's `upsertItem`/`upsertUser` against a real PB DB (verifies that
  kiosk-local `quantity_on_hand` survives a catalog update); soft-delete
  sets `active=false` and is idempotent for unknown codes.
- `internal/controller` — idempotent transaction + line projection under
  redelivery; "parent not yet here" produces a retry; unknown user/item
  skipped with ack; `TouchKiosk` auto-registers on first sight and
  advances `last_transaction_at` on subsequent transaction.complete events.
  `KiosksForItem` / `ItemsForKiosk` membership helpers plus cascade-delete
  verification on `kiosk_items`. Heartbeat registry: record/snapshot,
  IsLikelyOnline thresholds, auto-register on first beat (once),
  malformed-payload tolerance.

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
| `POST` | `/api/kiosk/ledger/republish` | admin | Re-emit transaction.complete + item.{action} events for completed transactions in an optional `{from, to}` ISO8601 window. Aggregator is idempotent so safe to re-run. |
| `POST` | `/api/kiosk/items/import` | admin | Multipart CSV upload, upsert by `code` |
| `POST` | `/api/kiosk/items/{id}/adjust` | admin | Change `quantity_on_hand` + write a `stock_adjustments` audit row in one transaction |
| `GET` | `/api/kiosk/transactions.csv` | admin | Export completed transactions as CSV (optional `from=` / `to=` ISO8601 query params) |
| `GET` | `/api/kiosk/catalog/integrity` | admin | **Controller only.** Diff catalog DB vs JetStream KV; returns `missing_in_kv` + `extra_in_kv` per bucket |
| `POST` | `/api/kiosk/catalog/reconcile` | admin | **Controller only.** Push DB → KV (always); delete orphaned KV keys when body `{delete_orphans: true}` |
| `GET` | `/api/controller/kiosks/heartbeats` | admin | **Controller only.** Returns `{controller_started_at, kiosks: {code: lastSeenISO}}` — the SPA polls every 10s to render online/stale/offline badges |
| `GET` | `/api/controller/kiosks/{code}/inventory` | admin | **Controller only.** Fires the `inventory.snapshot` command over NATS request/reply; returns the kiosk's live on-hand for every stocked item. 503 `{error: "kiosk_offline", kiosk_code}` when stale heartbeat or NATS timeout. |
| `POST` | `/api/controller/kiosks/{code}/inventory/adjust` | admin | **Controller only.** Server-generates a `command_id`, fires `inventory.adjust` to the kiosk over NATS request/reply. Body: `{item_code, mode, value, reason}`. Idempotent via `command_id`; 503 on offline. |
| `GET` | `/health` | none | Returns `{status: "ok"}` — for liveness probes |

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
| Transaction completed | `{prefix}.{kiosk_code}.transaction.complete` |
| Checkout line | `{prefix}.{kiosk_code}.item.checkout` |
| Return line | `{prefix}.{kiosk_code}.item.return` |
| Consume line | `{prefix}.{kiosk_code}.item.consume` |
| Admin stock adjustment | `{prefix}.{kiosk_code}.inventory.adjust` |
| Open-checkouts rebuild | `{prefix}.{kiosk_code}.integrity.rebuild` |

`{prefix}` is `"kiosk"` by default and configurable via `nats.subject_prefix`
(both kiosk and controller must agree). Override only to avoid collisions on
a shared NATS cluster where another application already owns the `kiosk.>`
subject space. Subscribe locally with the `nats` CLI to confirm publishing:

```bash
nats sub "kiosk.>"
```

Two more NATS subject families exist alongside the events above but don't
ride JetStream:

| Subject | Direction | Transport |
|---|---|---|
| `{prefix}.{kiosk_code}.heartbeat` | kiosk → controller | Core NATS publish, 45s cadence. No persistence — last-write-wins is the entire signal. |
| `{prefix}.{kiosk_code}.command.<name>` | controller → kiosk | Core NATS request/reply, ≤5 s reply timeout. Today: `inventory.adjust`, `inventory.snapshot`. |

The `KIOSK_EVENTS` stream's `FilterSubjects` deliberately excludes both —
heartbeats and commands should never be replayed from a durable stream.

## Schema

Nine collections, defined as code across `migrations/*.go`. The initial
migration creates the first six; subsequent migrations add the per-instance
and audit-log collections, and the controller-only `kiosks` registry and
`kiosk_items` membership table (and a few fields on `items`).

| Collection | Purpose |
|---|---|
| `users` | Workers (badge-holders). PB default auth collection, real emails kept for future notifications. Workers don't log in in v1. `group` is an optional FK to `groups`. |
| `admins` | Foremen / admins. Separate PB auth collection. Login via email + password. |
| `groups` | Sub-contractors / trades. `code` is the stable join key (CSV import, cross-fleet sync); metadata fields (`name`, `contact_email`, `contact_phone`, `notes`) are admin-managed and downstream features like email receipts use them. Optional on workers; deletion sets affected `users.group` to null. |
| `items` | Tools and consumables (the SKU). `tracking_mode` is `quantity` or `serialized`. Carries `quantity_on_hand` (fleet count for tools / current stock for consumables) and `reorder_threshold` (low-stock alert level; 0 disables the alert). |
| `item_instances` | One physical unit of a serialized SKU. Holds the scannable `code`, the printed `serial`, an optional `rfid_epc`, and `active`. FK to the parent `items` row. |
| `transactions` | Append-only ledger. `kiosk_code`, `location_code`, `user`, `started_at`, `completed_at`, `status`. |
| `transaction_lines` | One per item action within a transaction. `action` is `checkout`, `return`, or `consume`. Carries optional `item_instance` FK for serialized lines. |
| `open_checkouts` | Materialized view of "what's out right now." One row per unit out. Carries `item_instance` FK for serialized units. Maintained by the commit hook. |
| `stock_adjustments` | Append-only audit log of changes to `items.quantity_on_hand` made via `/api/kiosk/items/{id}/adjust` (local) or the controller's `inventory.adjust` command bus (remote). Stores `delta`, `new_quantity` (snapshot), `reason`, the responsible `admin` (FK, populated for `source=local`), `source` ('local' \| 'controller'), `controller_admin_id` (text — controller's admin id, populated for `source=controller`), and `command_id` (UUID, unique-when-non-empty for idempotent replay of remote commands). |
| `kiosks` | **Controller-only.** Registry of every kiosk in the fleet. A row appears either when an admin pre-registers the kiosk via the "New kiosk" button on AdminKiosksView, auto-populated with `status=unknown` the first time the aggregator sees a `transaction.complete` from a new `kiosk_code`, or auto-populated on the first heartbeat. `last_transaction_at` advances on `transaction.complete` only (its name finally matches its meaning now that heartbeat owns general liveness); `last_seen` writes alongside it for one release as a deprecation window. Used for fleet visibility and as the join target when expanding aggregated transactions to "which kiosk did this come from?" |
| `kiosk_items` | **Controller-only.** Membership rows tying items to kiosks. One row = one (kiosk, item) pair = "this kiosk stocks that SKU." Cascade-deletes from either side. Drives per-kiosk catalog publishing; absent rows mean the kiosk never receives that item over JetStream KV. |

The controller's `transactions` and `transaction_lines` collections carry
two extra fields not present on standalone kiosks:
`source_kiosk_code` + `source_transaction_id` on transactions (unique pair
index, idempotency key for redelivery) and `source_line_id` on
transaction_lines (unique-when-non-empty index). These — along with the
`kiosks` and `kiosk_items` collections plus `kiosks.last_transaction_at` —
are added by three controller-only migrations
(`2000000000_controller_collections.go`,
`2000100000_add_kiosk_items.go`, and
`2000200000_kiosks_last_transaction_at.go`), all registered via a single
`sync.Once` body in `RegisterControllerMigrations`. The plain kiosk binary
never invokes it, so its DB never gets these.

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
code,name,type,unit,tracking_mode,category,active,notes,quantity_on_hand,reorder_threshold
DR-IMPACT-042,Impact Driver,tool,each,serialized,Power Tools,true,,1,0
SCREW-DECK-3IN,Deck Screws 3in,consumable,box of 100,quantity,Fasteners,true,,25,5
```

Empty cells are nulls. `active` accepts `true|false|1|0|yes|no|y|n`. The
`quantity_on_hand` and `reorder_threshold` columns are optional; if
omitted, existing rows keep their current values and new rows default to
zero. Items are matched by `code` (upsert). Items not in the CSV are left
alone. Per-unit serials and RFID EPCs live on `item_instances`, not on the
SKU row — serialized SKUs created via CSV still need their instances
added through the admin UI's instances panel.

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
admin token. The response reports `{ deleted, inserted }` counts. Each
rebuilt row carries the source checkout line's `completed_at` as
`checked_out_at` and a FK back to the originating `transaction_line`, so
aging and audit reports stay meaningful after a rebuild.

### Resyncing the kiosk ledger to the controller

When the controller's projected ledger has drifted from a kiosk's local
one — typically because NATS was briefly unreachable and an event was lost
on kiosk restart — the kiosk can re-emit its history:

```bash
# Republish every completed transaction in the kiosk's ledger:
curl -X POST \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{}' \
  http://localhost:8090/api/kiosk/ledger/republish

# Or scope to a date window (suspected drift in the last 24h):
curl -X POST \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"from": "2026-05-21T00:00:00Z"}' \
  http://localhost:8090/api/kiosk/ledger/republish
```

The endpoint walks completed transactions in `completed_at` order and
re-emits one `transaction.complete` event plus one `item.{action}` event
per line, with payloads rebuilt from the persisted records and the
kiosk_code/location_code that were stamped at original commit time.

Safe to re-run: the controller's aggregator dedupes on
`(source_kiosk_code, source_transaction_id)` for transactions and on
`source_line_id` for lines. Re-publishing the entire history is the
brute-force recovery; the `{from, to}` scope is friendlier for routine
ops. Response is `{ transactions_published, lines_published, skipped }`.

### NATS failure modes

NATS is best-effort on the kiosk and hard-required on the controller. This
section enumerates what survives each failure and what gets lost, so you
can decide what to monitor.

| Scenario | Kiosk behavior | Controller behavior |
|---|---|---|
| Broker unreachable at kiosk startup | Boots normally; `events.Connect` returns a buffering connection that dials in the background. The local ledger is authoritative; checkouts work without NATS. Catalog sync (if enabled) logs a warning and proceeds without sync until the broker is reachable. | N/A |
| Broker unreachable at controller startup | N/A | Controller fails to start (NATS is required). Operator must bring the broker up first. |
| Broker dies mid-publish (kiosk → controller) | The event is queued in the NATS client's in-memory buffer. If the buffer overflows or the kiosk process restarts before reconnect, the event is **lost** from the controller's view — but the kiosk's local ledger still has the underlying transaction. A future "resync this kiosk" admin button will close this gap; for now, the integrity check on the controller is your signal. | Misses the event; its projected ledger silently drifts from the kiosk's. Surface via cross-checking aggregate counts kiosk-side vs. controller-side. |
| Broker dies mid-publish (controller → kiosk catalog KV) | If the kiosk is offline at the time, the kiosk re-syncs from the bucket's current value on next connect (`Watch` replays the latest value per key). No loss. | The `Put` call fails; the controller logs a warning. The DB record is already saved, so the controller's state is correct — only the KV propagation failed. Re-save the record (any edit) to retry, or restart the controller to re-publish. |
| Broker recovers after outage | Buffered events flush automatically. Durable consumer (controller side) resumes from last-acked sequence — no replay storm. KV watchers reattach and project any keys that changed during the outage. | Same. |
| Controller process restart | N/A | Durable consumer `controller-aggregator` resumes from last-acked sequence. KV `CreateOrUpdateKeyValue` is idempotent. Hooks re-bind on the next save. No event loss across restarts; events that arrived *during* the outage are still in the JetStream stream (retention default: 7 days). |
| Kiosk process restart | In-memory cart is lost (documented). NATS publisher reconnects. Watcher (if managed) re-projects the current KV snapshot. Local ledger is intact on disk. | N/A |
| JetStream stream retention expires | N/A | Events older than `MaxAge` (default 7 days) drop off the stream. Re-running the consumer won't replay them. For long-term ledger archival, rely on the kiosk's local ledger or controller's projected ledger — both are persistent SQLite. |

The two big takeaways:

1. **Local ledgers are authoritative.** The kiosk's `pb_data/data.db` and the controller's `pb_data_controller/data.db` are the source of truth for their respective views. NATS is a transport, not a database.
2. **The controller's projected ledger can drift if events are lost in flight.** Today this is detected manually by spot-checking aggregate counts. A drift-detection job (compare per-kiosk `transactions.count` between controller and kiosk) is on the roadmap.

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

### Adjusting stock from the controller (remote)

Controller admins can adjust a kiosk's stock without walking to it. From
the controller's **Kiosks → \<kiosk\> → Inventory** tab, the SPA fetches a
live snapshot (`inventory.snapshot` over NATS) and offers a per-row
**Adjust** button that opens the same delta/absolute/reason dialog.
Submitting fires the `inventory.adjust` command at the target kiosk; the
kiosk runs the same `PerformStockAdjustment` business logic the local
endpoint uses, writes a `stock_adjustments` row with `source='controller'`
and the controller admin's id in `controller_admin_id`, and publishes the
usual `inventory.adjust` event back through JetStream. The controller-side
audit log therefore sees one event shape regardless of origin.

Failure modes:

- **Kiosk offline.** When the kiosk's last heartbeat is older than 90 s,
  the controller endpoint short-circuits with **503 `{error:
  "kiosk_offline", kiosk_code, command_id}`** before even publishing —
  the SPA renders "kiosk offline" in ~1 s instead of waiting for a NATS
  timeout. Same body is returned when the NATS request itself times out
  (5 s) or hits `nats.ErrNoResponders`.
- **Retry / duplicate submission.** If the SPA times out waiting for the
  reply but the kiosk actually processed the command, retrying with the
  same `command_id` is safe — the kiosk's idempotency check on
  `stock_adjustments.command_id` returns the prior result instead of
  re-applying. The controller currently generates one UUID per submit
  click; the future "reconcile" tool will accept an external `command_id`
  for explicit replay.

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
  `items` and `users`. Items are **not broadcast** — each kiosk's stock is
  governed by explicit `kiosk_items` membership rows on the controller. A
  row exists → that kiosk sees that item; no row → it doesn't. New SKUs
  do not auto-flow anywhere; admins add them via the kiosk's "Stocked
  items" panel or the bulk-add-by-category action. The shared
  `catalog_items` KV bucket uses namespaced keys `<kiosk_code>.<item_code>`,
  and each kiosk's watcher subscribes only to its own prefix
  (`Watch("KIOSK01.>")`), so the wire never carries items the kiosk
  shouldn't see. Users remain org-wide on `catalog_users`. Kiosk-local
  state (`quantity_on_hand`, `reorder_threshold`, `item_instances`) is
  intentionally not synced and survives catalog updates untouched.
- **Transactions up → controller.** Every kiosk already publishes
  `{prefix}.{code}.transaction.complete` and `{prefix}.{code}.item.{action}`
  when NATS is enabled (`{prefix}` defaults to `kiosk`). The controller
  runs a JetStream durable consumer (`controller-aggregator`) over the
  `KIOSK_EVENTS` stream (configurable via `nats.stream_name`) that projects
  every incoming event into its own `transactions` / `transaction_lines`
  rows. Idempotency keys (`source_kiosk_code + source_transaction_id` on
  transactions, `source_line_id` on lines) make redelivery safe.
- **Kiosks registry.** Three ways a kiosk gets a row in the controller's
  `kiosks` collection: (1) **pre-registered** by an admin via the "New
  kiosk" button on AdminKiosksView — required if you want to assign items
  before the kiosk has phoned home; (2) **self-registered** the first time
  the aggregator sees a `transaction.complete` event from a `kiosk_code`
  it doesn't know; (3) **heartbeat auto-register** — the first heartbeat
  from a new kiosk also creates the row, so kiosks that haven't yet
  transacted still appear in the registry. All three converge on the same
  row with `status=unknown`. `last_transaction_at` advances on
  `transaction.complete` only; live online status comes from the in-memory
  heartbeat map (see below), not from the persisted timestamp.
- **Heartbeat + online status.** Each managed kiosk publishes a small
  JSON beacon on `{prefix}.{kiosk_code}.heartbeat` every 45 s using core
  NATS (not JetStream — missing a beat is the entire point of the
  signal). The controller subscribes plainly, keeps a mutex-guarded
  `map[code]time.Time` in memory, and serves it at
  `GET /api/controller/kiosks/heartbeats`. The SPA polls every 10 s and
  renders three-state badges: **online** (<90 s), **stale** (90 s–5 min),
  **offline** (>5 min). For ~90 s after a controller restart the SPA
  shows "unknown" to avoid painting a fleet red while beats catch up.
- **Inventory commands** (`POST /api/controller/kiosks/{code}/inventory/adjust`,
  `GET /api/controller/kiosks/{code}/inventory`). The controller proxies
  admin clicks to the target kiosk over core NATS request/reply on
  `{prefix}.{kiosk_code}.command.<name>`. The kiosk runs the same
  `PerformStockAdjustment` business logic the local HTTP path does, then
  publishes the same `inventory.adjust` event the aggregator already
  knows about. Idempotency is server-side: the controller generates a UUID
  `command_id`; the kiosk's `stock_adjustments` schema has it as a unique
  column, so a retried command returns the prior result instead of
  double-applying. The endpoints fast-fail with **503 `{error:
  "kiosk_offline", kiosk_code, command_id}`** when the kiosk's heartbeat
  is stale, so the SPA doesn't wait 5 s for a NATS timeout to render
  "offline."

What it **doesn't** do in v1 (deliberately out of scope):

- Drift detection / state-hash compare between controller and kiosk.
- Controller-side per-kiosk `quantity_on_hand` projection (the
  `inventory.adjust` event still acks-and-logs at the aggregator; a
  fleet-wide low-stock view would consume it).
- Cross-fleet movement of serialized items.
- Tightening PB collection rules on managed kiosks (the projector uses
  the DAO, so rules don't matter; UI gating handles the admin experience).
- Other remote admin commands — only inventory adjust + snapshot ship in
  v1. The dispatcher's `HandleFunc` registry makes adding a new command a
  one-handler change.

### NATS provisioning

The controller and kiosks both connect to a `nats-server` you run
separately. JetStream must be enabled (`nats-server -js`). Provision the
stream and KV buckets once, out of band:

```bash
nats stream add KIOSK_EVENTS --subjects 'kiosk.>' --retention limits --max-age 168h --no-ack
nats kv add catalog_items --history 1
nats kv add catalog_users --history 1
nats kv add catalog_groups --history 1
```

(The controller will auto-create these on first start as well; the manual
form is here so operators have a record of what's provisioned.)

**`--no-ack` is load-bearing.** The controller→kiosk command bus uses
core NATS request/reply on subjects inside the stream's filter space
(e.g., `kiosk.K01.command.inventory.adjust`). With the default
`--no-ack=false`, JetStream sees the message's `Reply` inbox and races a
PubAck to it — which the controller's `nc.Request()` then mis-reads as
the kiosk's actual reply (you'll see "kiosk online" but get back a stream
sequence number instead of the result). The kiosk and controller never
use the JetStream publish API; everything goes through `nc.Publish`,
which doesn't expect a PubAck. So turning the publisher-side ack off
costs nothing and unblocks request/reply.

This setting is **separate from consumer acknowledgement** — the
controller's durable consumer still uses `AckExplicitPolicy`, so it acks
each event it processes and JetStream advances the cursor accordingly.
`--no-ack` only suppresses the stream's PubAck flowing back to the
*publisher*, not the consumer's ack flowing back to the *stream*. The
controller's `ensureStream` sets `NoAck: true` and the comment block
explains why; don't remove it without a replacement (e.g., narrowing
`--subjects` to exclude `command.*` and `heartbeat`).

Names above are the defaults. On a shared NATS cluster where `kiosk.>` or
`KIOSK_EVENTS` are already taken, override via `nats.subject_prefix` and
`nats.stream_name` (and the `controller.catalog_*_bucket` keys for the KV
buckets). The kiosk and controller must agree on the subject prefix; the
stream name is consumed only by the controller. Substitute your overrides
into the provisioning commands above.

**Run exactly one controller per stream.** The durable JetStream consumer is
named `controller-aggregator` (a constant in `internal/controller/consumer.go`),
so two `kiosk-controller` processes pointed at the same `nats.stream_name`
will fight for the same consumer cursor and projection writes will be
unpredictable. JetStream's durable-consumer model is single-owner by design;
this isn't a place we want HA via duplication. If you need redundancy, run
the controller under a supervisor that restarts on crash — durable means
restarts resume from the last-acked sequence with no event loss. To scale
horizontally across separate fleets, give each its own `nats.stream_name`
(and matching `nats.subject_prefix`) so the controllers don't overlap.

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
plus three controller-only migrations
(`2000000000_controller_collections.go`,
`2000100000_add_kiosk_items.go`, and
`2000200000_kiosks_last_transaction_at.go`) that are registered explicitly
via `migrations.RegisterControllerMigrations()` from
`cmd/controller/main.go` — the kiosk binary never calls it. The controller's
data dir is `pb_data_controller/` so a kiosk and controller can co-exist in
one working directory during development without colliding.

The controller's PocketBase admin UI lives at the same paths as a kiosk's:
`/_/` for the PB superuser, `/admin/login` for the kiosk admin. The Kiosks
list view (`/admin/kiosks`) shows the fleet with online badges; clicking
a row opens the per-kiosk detail view at `/admin/kiosks/<code>`, which has
three tabs:

- **Overview** — editable location, status, and notes; the live online
  indicator and last-transaction timestamp.
- **Items** — the "Stocked items" membership panel (lifted from the old
  dialog).
- **Inventory** — fetches a live snapshot of on-hand quantities from the
  kiosk via the `inventory.snapshot` NATS command, with a per-row Adjust
  button that drives the corresponding `inventory.adjust` command.

Use the Items view to add/edit items globally; user edits fan out to every
managed kiosk. Item edits fan out only to the kiosks that stock them —
open a kiosk's detail page and use the Items tab (or "Bulk add by
category") to assign SKUs first. The Items view shows a "Stocked at" chip
list inside each item's edit dialog so you can see the inverse projection
at a glance.

### Assigning items to kiosks

Once you have items and kiosks on the controller, decide which SKUs each
kiosk stocks. There are two paths in the admin UI:

1. **New kiosk button** on AdminKiosksView pre-registers a kiosk record by
   `kiosk_code` + `location_code` before the kiosk itself has phoned home.
   This unblocks the next step on day-one deployments. After creation, the
   SPA navigates straight to the new kiosk's detail page.
2. **Items tab** on the kiosk detail page (`/admin/kiosks/<code>`). From there:
   - **Add item** — search the global catalog and click an item to add it.
   - **Bulk add by category** — pick a category, preview the matching SKUs,
     confirm. The result is just rows in `kiosk_items` — there is no stored
     "category rule," so items added to the catalog later will not auto-flow
     to this kiosk. Click the button again when you want to top up.
   - **Remove** — drops the membership row. The kiosk receives a KV delete
     on its key and soft-deactivates the item locally; its `item_instances`
     and any transaction history stay intact.

Inversely, the **Stocked at** chip list on each item's edit dialog
(controller mode only) shows which kiosks currently carry that SKU.
Read-only — the source of truth is per-kiosk membership.

### CSV seed format

`seed-catalog` reads two optional CSV files. Items use the same columns as
the kiosk's `/api/kiosk/items/import` endpoint (see below). Users:

```csv
code,name,email,role,group,active
WORKER-1,Alice,alice@example.com,worker,electrical,true
FOREMAN-1,Bob,bob@example.com,foreman,electrical,true
```

The `group` column carries the group's **code** (see the `groups` collection in
the schema table). Blank means ungrouped. Unknown group codes are
**auto-created** during import — a row with `code = name = csvValue,
active = true` is inserted, which admins then enrich with `contact_email`,
phone, and notes via the Groups admin view. This keeps existing CSV
formats working unchanged. On shared sites with multiple trades, set
each worker's group so a foreman can return tools across users **only
within their own group**; an ungrouped foreman can't
act for anyone. See "Groups and roles" below for the full rules.

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

1. Connect to NATS, subscribe to its slice of the items bucket
   (`Watch("<my_kiosk_code>.>")`) and the shared `catalog_users` bucket,
   then project the snapshot into local `items` and `users` (matching by
   `code`). Items absent from membership simply never arrive.
2. Publish its own transaction events to NATS as before — the controller's
   consumer picks them up automatically and the kiosk shows up in the
   controller's `kiosks` registry (if it wasn't pre-registered already).
3. Hide catalog edit affordances in the admin SPA; the banner
   "Catalog managed by controller" appears on every admin page.

**Upgrade note.** Earlier builds used flat keys (`<item_code>`) in
`catalog_items` and broadcast every SKU to every kiosk. Membership-driven
publishing changes the key shape and meaning, so on upgrade: wipe each
kiosk's `pb_data/` and the controller's `catalog_items` KV bucket
(`nats kv rm catalog_items`, then let the controller re-create it), then
assign items to kiosks via the new "Stocked items" panel. The app isn't
in production yet so no compat shim is provided.

If `controller.enabled=true` but NATS is unreachable, the kiosk still
boots and serves checkouts against whatever catalog state it has — the
watcher logs a warning and proceeds without sync until the broker comes
back. The local ledger is always authoritative.

### Reconciling catalog drift

The controller's PB record hooks `Put` to the catalog KV buckets after
each save. If the broker was briefly unreachable during a save, the DB
row lands but the KV `Put` fails silently (logged at warn level). Over
time this can leave KV slightly out of sync with the controller's DB.
Symmetric scenarios produce the same problem: a controller restored
from an older backup is "out of sync" against newer KV state; a fresh
controller pointed at a pre-existing bucket inherits whatever was there.

Two endpoints help:

- **`GET /api/kiosk/catalog/integrity`** (admin) returns a read-only diff:

  ```json
  {
    "items": {
      "bucket": "catalog_items",
      "expected_keys": 24,
      "actual_keys": 25,
      "missing_in_kv": [],
      "extra_in_kv": ["KIOSK-OLD.OBSOLETE-SKU"]
    },
    "users": { ... }
  }
  ```

  `missing_in_kv` are DB rows whose KV entries never landed (push needed);
  `extra_in_kv` are KV keys with no backing DB record (rollback orphans,
  pre-existing bucket residue).

- **`POST /api/kiosk/catalog/reconcile`** (admin) pushes the DB state to
  KV. Body `{"delete_orphans": false}` (the default) re-Puts every
  expected key — safe to run any time, KV history is 1 so this is just
  an idempotent overwrite. Body `{"delete_orphans": true}` additionally
  removes the `extra_in_kv` set. Use the latter after a confirmed
  rollback or when adopting a pre-existing bucket.

  ```bash
  # Inspect first (read-only):
  curl -H "Authorization: Bearer <admin-token>" \
    http://controller:8091/api/kiosk/catalog/integrity

  # Push DB → KV without touching orphans (safe default):
  curl -X POST -H "Authorization: Bearer <admin-token>" \
    -H "Content-Type: application/json" -d '{}' \
    http://controller:8091/api/kiosk/catalog/reconcile

  # Push DB → KV and delete orphans (destructive, explicit):
  curl -X POST -H "Authorization: Bearer <admin-token>" \
    -H "Content-Type: application/json" \
    -d '{"delete_orphans": true}' \
    http://controller:8091/api/kiosk/catalog/reconcile
  ```

The controller's DB is always the source of truth; reconcile is one
direction only (DB → KV). There is no "KV teaches the DB" mode — that
would let an operator's `nats kv put` override the controller's
authoritative state, which we don't want.

Reconcile is **not** run automatically at boot. Operators who use
`nats kv put` for debug or override should not be surprised by the
controller blowing away their edit on next restart. After a rollback or
bucket-adoption, the runbook entry is: hit `reconcile` with
`delete_orphans: true`.

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
- **Per-kiosk catalog membership.** Controller-side `kiosk_items` join
  collection plus namespaced KV keys (`<kiosk_code>.<item_code>`) and a
  prefix-filtered kiosk-side watch. Admin UI on the controller has a
  "Stocked items" panel per kiosk with add/remove and a "Bulk add by
  category" snapshot action, plus a "Stocked at" reverse view on each
  item. Kiosks can also be pre-registered by an admin before they phone
  home so memberships can be assigned ahead of time.
- **Controller→kiosk command bus.** Core NATS request/reply (not
  JetStream) under `{prefix}.{kiosk_code}.command.<name>`. Two commands
  ship in v1: `inventory.adjust` (mutating, idempotent via a
  server-generated `command_id` unique-indexed on `stock_adjustments`)
  and `inventory.snapshot` (read-only). Controller endpoints
  fast-fail with 503 when the kiosk's heartbeat is stale, and pass the
  kiosk's reply through unchanged to the SPA.
- **Heartbeat + online status.** Each kiosk publishes a 45 s heartbeat on
  `{prefix}.{kiosk_code}.heartbeat` (core NATS, no persistence). The
  controller keeps an in-memory map and exposes
  `GET /api/controller/kiosks/heartbeats`; the SPA polls every 10 s and
  renders online/stale/offline badges in the list view and on the
  per-kiosk detail page.
- **Kiosk detail page.** New `/admin/kiosks/<code>` route on the
  controller SPA replaces the cramped edit dialog. Three tabs (Overview,
  Items, Inventory) gather all per-kiosk admin work behind a single deep
  link. The old `KioskDialog` shrank to create-only.
- **`kiosks.last_transaction_at`.** New field that means what its name
  says: "when did this kiosk last actually transact?" `touchKiosk` is
  now narrowed to `transaction.complete` events only — general liveness
  moved to the heartbeat. `last_seen` writes alongside it for one
  release as a deprecation window.

## Roadmap

Items below are still intentionally deferred. Schema and event subjects are
in place to make them additive rather than rewrites.

- **Controller-side qty projection.** The kiosk already publishes
  `{prefix}.{kiosk_code}.inventory.adjust` for every admin adjustment
  (local or controller-driven), and the controller ack-and-logs it. What's
  still deferred is projecting those adjustments (and `item.checkout` /
  `item.consume` qty deltas) into a controller-side per-kiosk
  `quantity_on_hand` so the controller has a fleet-wide low-stock view
  without re-querying each kiosk via `inventory.snapshot`.
- **More remote commands.** The command bus and dispatcher are in place
  (`internal/commands/`); v1 wires inventory adjust + snapshot. Natural
  next commands: force a catalog resync, lock a kiosk to a holding
  screen, integrity rebuild from the controller, ledger republish.
  Each is a single handler on the kiosk side plus a controller endpoint
  that fires `nc.Request` at the appropriate subject.
- **Drift detection.** Periodic state-hash compare between controller and
  each kiosk; surface discrepancies in the controller admin UI for triage.
- **Cross-fleet movement of serialized items.** Move a specific
  `item_instances` row from kiosk A to kiosk B with central as the
  arbiter — one serial belongs to one kiosk at a time.
- **Tighten PB collection rules in managed mode.** UI gating is the v1
  story; a follow-up could lock the collection rules themselves so a
  determined admin poking PB directly can't drift the catalog.
- **Per-subject NATS ACLs.** Today any holder of the NATS credentials can
  publish to `{prefix}.*.command.>`. Locking the command pattern to
  controller-only credentials (and the event subjects to kiosk-only
  credentials) is a deployment-time tightening worth doing before any
  multi-tenant scenario.
- **RFID reader integration.** Impinj reader publishes scans to
  `kiosk.{kiosk_code}.scan.rfid`. The scan dispatcher already resolves
  `rfid_epc` against `item_instances` — no new dispatch logic needed.

Each of these can be evaluated on demand. None should be built until there is
a concrete user asking for it.
