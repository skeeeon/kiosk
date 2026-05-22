# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A self-service tool/consumable checkout kiosk. One Go binary (`cmd/kiosk`) that
embeds PocketBase (REST API + SQLite + admin UI at `/_/`) and serves a Vue 3
SPA from `pb_public/`. No separate API server, no separate database, no
external runtime dependencies. Designed to run on a mini-PC with a touchscreen
and USB HID barcode scanner; the SPA listens to window-level keydown to read
scans.

Optionally, a second binary (`cmd/controller`) acts as a central
"kiosk-controller" — same PB stack, no kiosk handlers. It runs a JetStream
durable consumer that aggregates per-kiosk transaction events into its own
ledger, and PB record hooks that publish catalog (items + users) changes
down to managed kiosks via JetStream KV. See the "Central controller" section
in README.md for the operator-facing view.

README.md has a thorough product/architecture overview; consult it for the
business logic (action defaulting, returns policy, central controller, etc.).
This file is for orienting in the code.

## Build & run

```bash
# Kiosk backend
go build -o kiosk-app ./cmd/kiosk
./kiosk-app                              # serves on 127.0.0.1:8090

# Controller backend (optional)
go build -o kiosk-controller ./cmd/controller
./kiosk-controller                       # serves on 127.0.0.1:8091 by default

# Frontend
npm install --prefix ui
npm run build --prefix ui                # outputs to pb_public/ (served by Go binary)
npm run dev --prefix ui                  # Vite dev server on :5173, proxies /api + /_ to :8090
```

The kiosk binary expects `kiosk.yaml` in CWD (or `KIOSK_CONFIG=/path`). On
first run it creates `pb_data/`, applies all migrations, and prints bootstrap
admin credentials to stdout **once**. To reset state: `rm -rf pb_data && ./kiosk-app`.

The controller binary expects `controller.yaml`, uses `pb_data_controller/`,
and sets `KIOSK_ROLE=controller` before config validation runs (relaxes the
`kiosk.code` requirement). It can also be invoked with the `seed-catalog`
subcommand for one-shot CSV import — see `controller.yaml.example` and the
README's "Central controller" section.

Frontend dev loop: run both the Go binary and `npm run dev` — Vite proxies
`/api` and `/_` to the Go process. Frontend build emits to `pb_public/` (not
`ui/dist/`); the Go binary serves that directory via `apis.Static` with
`indexFallback=true` so client-side routes resolve.

## Tests

```bash
go test ./...                            # all Go tests
go test ./internal/commit/...            # heart of the system
go test ./internal/controller/...        # controller aggregator + idempotency
go test ./internal/catalog/...           # KV payload + kiosk projector
go test -run TestCommit_CrossUser ./internal/commit/...   # single test
```

Frontend has no test suite. SPA correctness is verified by `vue-tsc` during
`npm run build`.

The commit/controller/catalog tests all use the same pattern: boot a real
PocketBase app per case via `pocketbase.NewWithConfig` +
`core.NewMigrationsRunner` (see `setupApp` in `internal/commit/commit_test.go`).
`migratecmd`'s `Automigrate` hooks `OnServe`, not `OnBootstrap`, so tests that
don't start a server must apply migrations explicitly via the runner — copy
this pattern for any new PB-backed test. Controller tests additionally call
`migrations.RegisterControllerMigrations()` before the runner so the
controller-only migration is included.

## Architecture you can't see from one file

**Two parallel API surfaces, two parallel auth models.**

- `/api/kiosk/*` — custom endpoints registered in `cmd/kiosk/main.go`, served
  by handlers in `internal/handlers/`. The kiosk checkout flow lives here.
  These endpoints are **anonymous** (the kiosk box is the trust boundary, bound
  to 127.0.0.1); worker identity is supplied via badge scan at the application
  layer. Admin-only endpoints in this set call `h.requireAdmin(re)` which
  checks `re.Auth.Collection().Name == "admins"`.
- `/api/collections/*` — PocketBase's built-in REST API. Admin SPA views use
  this through `pocketbase` JS SDK (`ui/src/lib/pb.ts`). Collection rules
  defined in the migration restrict everything to the `admins` auth collection.
  `transactions`, `transaction_lines`, and `open_checkouts` are **API-readonly**
  — they are written only by the commit code path.

There are **two distinct admin populations**: PocketBase superusers (manage
collections at `/_/`, created on first visit) and `admins` collection records
(log in at `/admin/login`, manage kiosk data). Don't conflate them. Worker
records live in the `users` auth collection but workers never log in in v1.

**The commit path is the only thing that writes ledger state.** `internal/commit/commit.go`
is the single funnel: cart → DB transaction (one `app.RunInTransaction`) →
events (after commit succeeds). Every kiosk state change goes through it.
Three invariants:

1. `transactions` and `transaction_lines` are append-only after commit. The PB
   collection rules forbid writes via the REST API; the commit hook is the only
   writer.
2. `open_checkouts` is a materialized view of "what's out right now," computed
   from `transaction_lines`. Cardinality: a `checkout` with `qty=N` of a
   non-serialized tool creates **N rows**; a `return` deletes up to N. If
   fewer matched, the line is stamped `uncorrelated=true`. Consumables and
   `return` for consumables never touch `open_checkouts`.
3. Events fire only **after** the DB transaction commits successfully, via
   `events.Publish` (currently slog-only; v2 will add NATS here without
   touching callers). Subject names follow NATS hierarchy: `kiosk.{kiosk_code}.transaction.complete`,
   `kiosk.{kiosk_code}.item.{action}`.

**Kiosk identity is process-global, not request-scoped.** `internal/kioskctx`
holds an `atomic.Pointer[Identity]` set once at startup from config. Every
transaction is stamped with `kiosk_code` + `location_code` from this — the
client never supplies them. This is what makes the system "federation-ready"
without an actual federation yet.

**Cart state is in-memory only.** `internal/cart/store.go` is a mutex-guarded
map keyed by cart ID. Carts expire lazily on access after `session.idle_timeout`
and vanish on process restart. A kiosk has at most one active user at a time,
so contention is nil. Don't add concurrency primitives optimizing for it. The
HTTP handler accepts a `cart_id` from the client and dispatches against this
store; commit drops the cart after successfully promoting to a transaction.

**Action defaulting is in the handler, not commit.** When a worker scans an
item, `handlers.defaultActionFor` (in `internal/handlers/cart.go`) picks the
action: consumable → `consume`; tool already out to this user → `return`;
tool out to someone else → `return` + cross_user_return warning; otherwise →
`checkout`. The cart freely accepts overrides; `commit.Commit` enforces only
structural rules (serialized item must have qty=1, etc.). Returns-policy flags
from config (`returns.allow_cross_user`, `allow_uncorrelated`) are not currently
enforced server-side at commit time — keep this in mind if touching that area.

**Scan resolution lives in its own package** (`internal/scan`) with the
data-access functions injected as `Lookups`. The resolver order encodes
disambiguation: explicit prefix wins; otherwise items first (scanned far more
often than badges), then RFID, then user code. Adding a new scan type means
adding to the dispatch chain in `Resolver.Resolve`, not sprinkling lookups
through handlers.

**Schema is code, not SQL.** `migrations/1779000000_init.go` defines all six
collections (`users`, `admins`, `items`, `transactions`, `transaction_lines`,
`open_checkouts`) including rules, indexes, and bootstrap-admin seeding. To
change schema, write a new `migrations/<unix-ts>_<name>.go` file with an `init()`
that registers via PB's migration API. Migrations run on startup; running
twice is a no-op (PB tracks them in `_migrations`).

Controller-only schema lives in `migrations/2000000000_controller_collections.go`
and is registered via an **explicit** `RegisterControllerMigrations()` call
guarded by `sync.Once` — NOT via an `init()` — because init runs at package
load before tests can set env vars and the kiosk binary transitively imports
the same package. `cmd/controller/main.go` calls the register function; the
kiosk binary doesn't. Tests call it from `setupApp` in the controller package.

**Controller seam.** When extending the central service:

- **Aggregation:** `internal/controller/consumer.go`. The `ProjectTransaction`
  and `ProjectLine` methods are pure-DB functions; `handle` wraps them with
  JetStream ack/nak. Add new event subjects by extending the consumer's
  `FilterSubjects` and the dispatch switch in `handle`.
- **Catalog publishing:** `internal/controller/catalog_publisher.go`. PB
  record-hooks call `kv.Put` / `kv.Delete`. To sync a new collection, add a
  bucket name to `internal/catalog/payload.go` and bind another set of hooks.
- **Kiosk-side projection:** `internal/catalog/watcher.go`. The watcher
  upserts records via the PB DAO (`app.FindFirstRecordByFilter` +
  `app.Save`), which bypasses collection rules cleanly. Kiosk-local fields
  (`quantity_on_hand`, `reorder_threshold`) are intentionally not touched
  on update.
- **Cross-fleet payload shape:** `internal/catalog/payload.go`. Single
  source of truth — both controller publisher and kiosk projector import
  from here. Adding a field is a one-line change that flows through both
  sides; the round-trip test enforces excluded fields stay excluded.

## Frontend notes

Vue 3 + TypeScript + `<script setup>` + Pinia + Reka UI + Tailwind 4. Two
distinct flows in one SPA:

- **Checkout flow** (`views/CheckoutView.vue`, `stores/session.ts`) — anonymous,
  driven by window-level scan events from `composables/useScan.ts`, talks to
  `/api/kiosk/*` via plain `fetch` (`lib/api.ts`).
- **Admin views** (`views/Admin*.vue`, `stores/auth.ts`) — authed via PocketBase
  JS SDK (`lib/pb.ts`), CRUDs the `users` and `items` collections via PB's REST
  API, plus hits `/api/kiosk/integrity` and `/api/kiosk/items/import` for the
  custom admin operations.

The scan composable skips when an `<input>`, `<textarea>`, `<select>`, or
contenteditable has focus. If you're adding a screen where the scan flow
should keep working, don't put a focused input in it.

## Config

`kiosk.yaml` (or `controller.yaml` for the controller binary) in CWD; override
with `KIOSK_CONFIG=/path`. Every YAML key has a `KIOSK_*` env-var override:
prefix `KIOSK_`, dots → underscores, uppercase. Env wins over file. `kiosk.code`
and `kiosk.location_code` are required and validated at startup **except** when
`KIOSK_ROLE=controller` is set (the controller's `main()` sets this before
`config.Load` runs). `kiosk.yaml.example` and `controller.yaml.example` are the
templates.

The `controller:` block (kiosk-side) opts the kiosk into central management:
when `controller.enabled=true`, the kiosk's startup wires up
`internal/catalog.Watcher` against the JetStream KV buckets and the admin
SPA hides catalog mutation buttons (gated on the `managed` flag in the
identity payload). NATS must also be enabled and point at the same broker
the controller publishes to.

`KIOSK_QUIET_BOOTSTRAP=1` suppresses the bootstrap admin credentials print —
used by tests so test output isn't polluted; don't remove it.
