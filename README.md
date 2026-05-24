# Tool/Consumable Checkout Kiosk

A self-service checkout kiosk for tool cribs and consumable storerooms.
A worker walks up to a touchscreen, scans their badge, scans items,
confirms a cart, and walks away. The same flow handles checkouts,
returns, and consumable grabs in one transaction.

The whole thing is one Go binary that embeds PocketBase and serves a
Vue 3 SPA from the same process. There's no separate API server, no
separate database, no external dependencies at runtime.
`scp kiosk-app pi@kiosk:/opt/kiosk/` is the entire deploy story.

For multi-kiosk deployments there's an optional **kiosk-controller**
sibling binary that distributes catalog down to managed kiosks over
NATS JetStream KV and aggregates their transaction events into a
central ledger. Standalone kiosks ignore it; managed kiosks point at
it via config. See [docs/controller.md](docs/controller.md).

## What you get

- **One binary.** Go + embedded PocketBase + SQLite + built Vue SPA.
  ~40 MB.
- **Edge-only.** Each kiosk is autonomous. No central server required.
  No internet needed for normal operation; an optional NATS publisher
  can feed events to a central consumer when one is reachable.
- **Two auth populations.** Workers identify by badge scan (no login).
  Admins log in with email + password to manage items, workers,
  imports, and reports.
- **Append-only ledger.** Transactions are immutable after commit. The
  `open_checkouts` table is a materialized view rebuildable from the
  ledger (integrity diff + one-click rebuild from the admin Reports
  view).
- **Stock tracking with audit trail.** Items carry `quantity_on_hand`
  and `reorder_threshold`. Consumables decrement automatically on
  `consume`; admin edits go through an `/adjust` endpoint that writes a
  `stock_adjustments` audit row in the same transaction as the item
  update. A Low Stock report surfaces items at or below threshold.
- **Per-unit tracking for serialized tools.** One `items` row is the
  SKU; one `item_instances` row is the physical unit (its scannable
  barcode, printed serial, RFID tag). Returns close the exact instance
  scanned, so three impact drivers under one SKU don't blur together.
- **Federation-ready.** Every transaction is stamped with `kiosk_code`
  and `location_code`. Every state change flows through
  `events.Publish`, which always logs via slog and (when enabled) also
  publishes to NATS — same subjects, no caller changes.
- **Optional central controller.** A `kiosk-controller` binary
  aggregates per-kiosk transaction events into its own ledger via a
  JetStream durable consumer, pushes catalog updates down to managed
  kiosks via JetStream KV (membership-driven, namespaced keys), drives
  admin commands over core NATS request/reply, tracks live online
  status via 45 s heartbeats, and centralizes email notifications
  against a single SMTP + audit trail. See
  [docs/controller.md](docs/controller.md) and
  [docs/notifications.md](docs/notifications.md).
- **Fleet-wide reporting.** On the controller, the Reports view
  exposes seven tabs: currently-out, aging, low-stock (live snapshot
  fan-out — one NATS round-trip per online kiosk), group activity,
  recent transactions, adjustment audit (projected from
  `inventory.adjust` events), and notifications deliverability.
  Standalone kiosks share the same Reports surface against their
  local data minus the controller-only tabs.

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
│         └── Vue SPA (embedded via //go:embed)   │
│                                                 │
│              ↑                                  │
│      USB Barcode Scanner (HID keyboard)         │
└─────────────────────────────────────────────────┘
```

The barcode scanner is a USB HID keyboard. The browser captures
keystrokes via a window-level listener that buffers characters and
dispatches on Enter. Same mechanism reads user QR codes and item
barcodes; the dispatcher disambiguates by configurable prefix or by
trying instance code → item code → instance RFID → user code. RFID is
instance-only — EPCs are per-tag and live on `item_instances`, never
on the SKU.

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
| Build | Vite 6 → `internal/ui/dist/` → `//go:embed` into Go binary |
| API client | PocketBase JS SDK 0.21 (admin CRUD); plain `fetch` (kiosk flow) |
| Messaging | NATS JetStream (optional) — durable streams for event aggregation, KV buckets for catalog distribution |

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
├── ui/                          Vue 3 SPA source (Vite project)
├── pb_data/                     Kiosk SQLite (gitignored, created on first run)
├── pb_data_controller/          Controller SQLite (gitignored; controller binary uses this)
├── internal/ui/                 //go:embed seam: dist/ holds the Vite output, embedded into both binaries at build time
├── branding/                    Optional on-disk branding overrides (logo, custom CSS) — sit next to the binary, not embedded
├── kiosk.yaml                   Kiosk config (gitignored)
├── kiosk.yaml.example           Kiosk config template
├── controller.yaml.example      Controller config template
└── go.mod, go.sum
```

## Quick start

```powershell
# 1. Clone, then from the project root:
go mod download

# 2. Build the SPA — emits to internal/ui/dist/, which is embedded into the Go binary
npm install --prefix ui
npm run build --prefix ui

# 3. Build the Go binary (must run step 2 first — //go:embed will fail on an empty dist/)
go build -o kiosk-app.exe ./cmd/kiosk     # on Linux/Mac: go build -o kiosk-app ./cmd/kiosk

# 4. Copy and customize the config
cp kiosk.yaml.example kiosk.yaml          # edit kiosk.code / location_code

# 5. Run
.\kiosk-app.exe                            # ./kiosk-app on Linux/Mac
```

On first boot the migration creates all collections and seeds one
bootstrap admin. The credentials are printed **once** to stdout:

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
superuser UI at `http://localhost:8090/_/` (you'll be prompted to
create a superuser the first time you visit).

Open `http://localhost:8090/`:

- The kiosk checkout flow ("Scan your badge to begin").
- Top-right header has a tiny **Admin** link → `/admin/login`.

## Documentation

The repo's deeper documentation lives under [`docs/`](docs/):

- [Configuration](docs/configuration.md) — YAML + env-var surface,
  branding, custom CSS.
- [Development](docs/development.md) — dev loops, DB reset, test suite.
- [API reference](docs/api.md) — custom endpoints, collection rules,
  event subjects.
- [Schema](docs/schema.md) — collections, cardinality rules, CSV
  import format.
- [Operations](docs/operations.md) — deploy, backups, ledger integrity,
  NATS failure modes, stock adjustments, instances, password reset.
- [Troubleshooting](docs/troubleshooting.md) — common errors and fixes.
- [Central controller](docs/controller.md) — multi-kiosk deployments.
- [Notifications](docs/notifications.md) — receipts, low-stock alerts,
  scheduled digests.
- [Shipped & roadmap](docs/roadmap.md) — what's live and what's deferred.
