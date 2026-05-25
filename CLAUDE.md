# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A self-service tool/consumable checkout kiosk. One Go binary (`cmd/kiosk`) that
embeds PocketBase (REST API + SQLite + admin UI at `/_/`) and serves a Vue 3
SPA compiled into the binary via `//go:embed` (see `internal/ui/embed.go`).
No separate API server, no separate database, no external runtime
dependencies — the binary is a single self-contained artifact. Designed to run on a mini-PC with a touchscreen
and USB HID barcode scanner; the SPA listens to window-level keydown to read
scans.

Optionally, a second binary (`cmd/controller`) acts as a central
"kiosk-controller" — same PB stack, no kiosk handlers. It runs a JetStream
durable consumer that aggregates per-kiosk transaction events into its own
ledger, and PB record hooks that publish catalog changes to managed kiosks
via JetStream KV. Item delivery is **membership-driven**: a join collection
`kiosk_items` says which SKUs each kiosk stocks, and item KV keys are
namespaced `<kiosk_code>.<item_code>` so each kiosk subscribes only to its
own slice. Users remain org-wide. Beyond the JetStream channels, the
controller also drives admin commands at remote kiosks over core NATS
request/reply (currently `inventory.adjust` + `inventory.snapshot`), and
receives 45 s heartbeats over a plain pub/sub subject to render live
online status. See the "Central controller" section in README.md for the
operator-facing view.

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
npm run build --prefix ui                # outputs to internal/ui/dist/ (embedded into Go binary at build)
npm run dev --prefix ui                  # Vite dev server on :5173, proxies /api + /_ to :8090
```

The kiosk binary expects `kiosk.yaml` in CWD (or `KIOSK_CONFIG=/path`). On
first run it creates `pb_data/`, applies all migrations, and prints bootstrap
admin credentials to stdout **once**. To reset state: `rm -rf pb_data && ./kiosk-app`.

The controller binary expects `controller.yaml`, uses `pb_data_controller/`,
and sets `KIOSK_ROLE=controller` before config validation runs (relaxes the
`kiosk.code` requirement). It can also be invoked with the `seed-catalog`
subcommand for one-shot CSV bulk import (items/users/groups) — see
`controller.yaml.example` and the README's "Central controller" section.
The same row logic backs the HTTP importer at
`POST /api/kiosk/<kind>/import` (kind ∈ {items, users, groups}) — both
binaries expose it, sharing `internal/csvimport.Run`.

Frontend dev loop: run both the Go binary and `npm run dev` — Vite proxies
`/api` and `/_` to the Go process. Frontend build emits to `internal/ui/dist/`,
which `internal/ui/embed.go` pulls into both binaries via `//go:embed all:dist`.
Production binaries serve the embedded FS via `apis.Static(ui.FS(), true)` with
`indexFallback=true` so client-side routes resolve. **Build order matters:**
`go build` will fail with an embed error if `internal/ui/dist/` is empty, so
run `npm run build --prefix ui` first on a fresh clone.

Branding overrides (`branding.logo_path`, `branding.custom_css_path` in
config) are read from disk at request time by separate handlers
(`internal/handlers/branding.go`) — they intentionally stay outside the
embed so a single binary can be re-skinned per deployment without rebuilding.
That's the **only** thing that needs to sit next to the binary; `pb_data/`
and `pb_data_controller/` are created on first run.

## Tests

```bash
go test ./...                            # all Go tests
go test ./internal/commit/...            # heart of the system
go test ./internal/controller/...        # controller aggregator + heartbeats + idempotency
go test ./internal/catalog/...           # KV payload + kiosk projector
go test ./internal/commands/...          # kiosk-side NATS command handlers
go test ./internal/handlers/...          # stock-adjust refactor + idempotency
go test -run TestCommit_CrossUser ./internal/commit/...   # single test
```

Frontend has no test suite. SPA correctness is verified by `vue-tsc` during
`npm run build`.

The commit/controller/catalog tests all use the same pattern: boot a real
PocketBase app per case via `pocketbase.NewWithConfig` +
`core.NewMigrationsRunner` (see `setupApp` in `internal/commit/commit_test.go`).
`migratecmd`'s `Automigrate` hooks `OnServe`, not `OnBootstrap`, so tests that
don't start a server must apply migrations explicitly via the runner — copy
this pattern for any new PB-backed test. Controller tests pull in the
controller-only migrations via a `migrations_setup_test.go` file that
blank-imports `migrations/controller` for side effect.

## Architecture you can't see from one file

**Three parallel API surfaces, two parallel auth models.**

- `/api/kiosk/*` — custom endpoints registered in `cmd/kiosk/main.go`, served
  by handlers in `internal/handlers/`. The kiosk checkout flow lives here.
  These endpoints are **anonymous** (the kiosk box is the trust boundary, bound
  to 127.0.0.1); worker identity is supplied via badge scan at the application
  layer. Admin-only endpoints in this set call `h.requireAdmin(re)` which
  checks `re.Auth.Collection().Name == "admins"`.
- `/api/controller/*` — controller-binary-only endpoints registered in
  `cmd/controller/main.go`, served by methods on `controller.Handlers`.
  Today: `GET /api/controller/kiosks/heartbeats`,
  `GET /api/controller/kiosks/{code}/inventory`, and
  `POST /api/controller/kiosks/{code}/inventory/adjust`. All admin-gated
  via `controller.Handlers.requireAdmin` (mirrors the kiosk version —
  the duplicate is documented at `internal/controller/handlers.go:37`).
  The inventory endpoints proxy core NATS request/reply commands to the
  target kiosk and pass the reply through unchanged.
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
   `events.Publish`. Every NATS subject follows
   `<prefix>.<kiosk_code>.<family>.<...>` where the family segment is one of
   `event`, `command`, or `heartbeat`. That segment is what determines
   transport — the JetStream stream binds to `<prefix>.*.event.>` and
   nothing else, so commands and heartbeats are outside the stream by
   construction rather than by exclusion-list discipline.

   Event subjects emitted from the commit/admin paths:
   `<prefix>.<kiosk_code>.event.transaction.complete` and
   `<prefix>.<kiosk_code>.event.item.{action}` from the commit hook;
   `<prefix>.<kiosk_code>.event.inventory.adjust` from the admin
   stock-adjust handler (both local HTTP and remote command paths emit
   the same shape via `handlers.PublishInventoryAdjustEvent`, and
   `commit.AdminClose` emits the same subject for the qty side-effect of
   lost/damaged closes); `<prefix>.<kiosk_code>.event.integrity.rebuild`
   from the open_checkouts rebuild handler;
   `<prefix>.<kiosk_code>.event.checkout.admin_close` from
   `commit.AdminClose` (one per row closed; the matching transaction also
   rides `event.transaction.complete` and the line rides
   `event.item.admin_close`-equivalent via the regular `transaction_lines`
   projection); `<prefix>.<kiosk_code>.event.instance.lifecycle` from the
   `item_instances` PB record hooks (create / decommission / reactivate /
   delete; cosmetic edits skip) AND from `commit.AdminClose` when a
   serialized `lost`/`damaged` close decommissions the instance. The
   `event.instance.lifecycle` payload carries `source_audit_id` (the
   kiosk-side `instance_audit.id`) so the controller's projection is
   idempotent under redelivery — same anchor strategy as
   `event.inventory.adjust`'s `adjustment_id`. The prefix is `"kiosk"` by
   default and configurable via `nats.subject_prefix`; subjects are built
   through shared helpers in `internal/events/subjects.go` (single source
   of truth for both the kiosk publisher and the controller's
   stream/consumer filters) — don't re-string-format these at callsites.

   The other two families:

   - **Heartbeats** — `<prefix>.<kiosk_code>.heartbeat`. Built via
     `events.HeartbeatSubject` / `events.HeartbeatFilter`. Last-write-wins,
     no persistence; durability would mask the very signal we care about.
   - **Commands** — `<prefix>.<kiosk_code>.command.<name>` (built via
     `events.CommandSubject` / `events.CommandSubscribePattern`). Request/
     reply, single attempt, ≤5 s reply timeout. The kiosk's dispatcher
     replies on `msg.Reply` with a `{success, error, data}` envelope.

   Adding a new event = put it under `event.` and the stream picks it up
   automatically. Adding anything that needs synchronous request/reply or
   fire-and-forget pub/sub = put it under `command.` (or its own
   non-event family) and it stays outside the stream. Do NOT publish
   commands or heartbeats through `events.Publish` — they're not events,
   and although the stream filter no longer captures them, the slog
   `kiosk.event` line still fires and reads as a lie.

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
otherwise → `checkout`. The cart freely accepts overrides; `commit.Commit`
enforces only structural rules (serialized item must have qty=1, etc.).
Returns-policy flags from config (`returns.allow_cross_user`,
`allow_uncorrelated`) are not currently enforced server-side at commit time
except when explicitly false — keep this in mind if touching that area.

Scanning a tool another worker has out **does not** implicitly become a
cross-user return: for quantity-tracked tools the natural reading of "Bob has
one out, I scan the SKU" is "give me one too," and even for serialized the
action of taking over someone else's open checkout deserves explicit intent.
Foreman-on-behalf-of returns live on a dedicated pair of endpoints in
`internal/handlers/cart.go`:

- `GET /api/kiosk/cart/foreman-return/options?cart_id=…`
  (`Handlers.CartForemanReturnOptions`) — picker payload for the dialog.
  Lists workers in the cart user's group with **≥1 open checkout**,
  hydrated with their `open_checkouts` rows. Inactive workers are
  deliberately included; the whole point of the dialog is closing out
  absent workers' items, which often correlates with "inactive."
- `POST /api/kiosk/cart/foreman-return` (`Handlers.CartForemanReturn`) —
  the sole writer of `Line.OriginalCheckoutUserID`. Two input shapes:
  `target_user_code` set (worker-pick path) or omitted (scan-shortcut
  path: only valid when `item_code` resolves to a serialized instance,
  whose open_checkouts row uniquely identifies the holder). Either way
  the endpoint pre-flights the same rules `commit.Commit` re-enforces
  (cart user is a foreman, has a group, target shares that group). The
  pre-flight is UX only; commit is still the trust boundary.

**Trust invariant: `OriginalCheckoutUserID` is server-resolved, never
client-supplied.** It is populated only by `CartForemanReturn` — either
from `target_user_code` looked up against `users` server-side, or
derived from the resolved instance's open_checkouts row — and is the
marker that triggers the foreman+group cross-user gate in
`commit.Commit`. The ordinary cart-write API paths —
`POST /api/kiosk/cart/add` and the PATCH update path — do NOT touch this
field, by design. If you ever add another cart-write path (rescan-to-
update, batch import, bulk edit), it MUST NOT accept
`OriginalCheckoutUserID` from the request body; route any on-behalf-of
intent through `CartForemanReturn`. Serialized returns are the sensitive
case: `closeCheckoutsForLine` for serialized items targets the
`item_instance` row globally, so a missing/forged
`OriginalCheckoutUserID` on a serialized return would silently bypass
the cross-user check.

One related cart-store invariant: `cart.Store.AddLine` includes
`OriginalCheckoutUserID` in its non-serialized merge key, so a
foreman-return-for-Bob never stacks onto a same-item self-return — merging
would strip the cross-user signal that commit's gate depends on.

`Cart.UserRole` is a denormalized snapshot taken at `Start` time and
refreshed when a cart is resumed within the idle window. The SPA reads
it to gate the "Return on behalf of…" button visibility. The server
re-reads role from the DB at both the foreman-return endpoint and at
commit, so a stale snapshot is at worst a UI hint that fails late — never
an auth bypass. Don't grow more decisions on top of this snapshot
without that property in mind.

**Scan resolution lives in its own package** (`internal/scan`) with the
data-access functions injected as `Lookups`. The resolver order encodes
disambiguation: explicit prefix wins; otherwise instance code → item code →
instance RFID → user code. RFID is **instance-only** — EPCs are per-tag and
live on `item_instances`, never on the SKU. Adding a new scan type means
adding to the dispatch chain in `Resolver.Resolve`, not sprinkling lookups
through handlers.

**Schema is code, not SQL.** `migrations/1779000000_init.go` defines all six
collections (`users`, `admins`, `items`, `transactions`, `transaction_lines`,
`open_checkouts`) including rules, indexes, and bootstrap-admin seeding. To
change schema, write a new `migrations/<unix-ts>_<name>.go` file with an `init()`
that registers via PB's migration API. Migrations run on startup; running
twice is a no-op (PB tracks them in `_migrations`).

Controller-only schema lives in its own sibling package
`migrations/controller/` (Go package name `controllermigrations`). Each
file there self-registers via `init() { m.Register(up, down) }`, same
pattern the kiosk migrations use. The kiosk binary doesn't import this
path, so its DB never sees the controller-only collections. The
controller binary blank-imports both `migrations` and
`migrations/controller` from `cmd/controller/main.go`. Adding a new
controller-only migration means dropping a new file into
`migrations/controller/` and registering it from its own `init()` — no
sync.Once or separate registration function involved.

The current controller-only files: `2000000000_controller_collections.go`
(kiosks registry + source_* idempotency columns on transactions/lines),
`2000100000_add_kiosk_items.go` (kiosk_items membership + opens
`kiosks.CreateRule`), `2000200000_kiosks_last_transaction_at.go`
(DateField on kiosks), `2000300000_inventory_audit.go`,
`2000400000_instance_lifecycle_audit.go`, and
`2000500000_open_checkouts_kiosk_code.go`.

`touchKiosk` in `internal/controller/consumer.go` writes
both `last_seen` (legacy, kept for one release) and `last_transaction_at`
on each `transaction.complete` event; the SPA reads
`last_transaction_at`. **Critical change in this release:** `touchKiosk`
is no longer called from before the dispatch switch — it now fires only
inside the `.transaction.complete` branch. Kiosks that emit only
non-transaction events (heartbeat, inventory.adjust, integrity.rebuild)
no longer auto-register via the aggregator; the heartbeat registry's
first-beat auto-register path (`controller.HeartbeatRegistry.handle`)
covers them.

One kiosk-side migration also landed
(`migrations/1787000000_stock_adjust_remote.go`) — it adds `source`,
`controller_admin_id`, and `command_id` (with a unique-when-non-empty
index) to `stock_adjustments`, and relaxes `admin` to nullable. Both
binaries pick it up via the unconditional `init()` pattern. The unique
index on `command_id` is the anchor of idempotency for the controller's
remote inventory.adjust command — see the comment block in
`internal/handlers/stock_adjust.go::PerformStockAdjustment` for the
upfront-lookup + unique-violation-catch dance.

**Per-kiosk catalog membership.** Controller-side `kiosk_items` is the
source of truth for "which SKUs does kiosk X stock." A row exists →
that kiosk gets that item; no row → it doesn't. New items don't auto-flow
anywhere; admins assign via the `KioskItemsPanel` on the kiosk detail
page's Items tab (plus a "bulk add by category" action). Categories are
**not** a stored rule — bulk-add creates rows at that moment; new items
added later won't auto-fill. The kiosk-side watcher uses
`Watch(<kiosk_code>.>)` on the shared `catalog_items` bucket, so per-kiosk
filtering is enforced server-side and the kiosk never receives keys for
other kiosks. Three paths get a kiosk a row in `kiosks`: **(1)
pre-registered** by an admin via the "New kiosk" button on
AdminKiosksView before phoning home; **(2) self-registered** via the
aggregator's `touchKiosk` on first `transaction.complete` (narrowed from
"any event" — see the Schema-is-code section); **(3) heartbeat
auto-register** via `HeartbeatRegistry.handle`'s first-beat callback,
which covers kiosks that haven't transacted yet. All three converge on
the same row.

**Controller seam.** When extending the central service:

- **Aggregation:** `internal/controller/consumer.go`. The `ProjectTransaction`,
  `ProjectLine`, `ProjectInventoryAudit`, and `ProjectInstanceLifecycle`
  methods are pure-DB functions; `handle` wraps them with JetStream ack/nak.
  `EventPayload` is a permissive superset of every event shape — new event
  fields go there and JSON-decode best-effort. Add new event subjects by
  (1) adding a builder + filter helper in `internal/events/subjects.go`,
  (2) extending the consumer's `FilterSubjects` via those helpers, and
  (3) adding a case to the dispatch switch in `handle`. Today the projected
  events are `transaction.complete` → `transactions`, `item.{action}` →
  `transaction_lines`, `inventory.adjust` → `inventory_audit`, and
  `instance.lifecycle` → `instance_lifecycle_audit`. `integrity.rebuild`
  and `checkout.admin_close` reach the controller but ack-and-log only;
  the admin_close transaction itself rides `transaction.complete` and is
  projected normally, so a future projector for the dedicated subject is
  optional. The audit projections are idempotent against
  unique-when-non-empty indexes on `source_adjustment_id` /
  `source_audit_id` — JetStream redelivery is a no-op. The stream name
  comes from `cfg.NATS.StreamName` (default `events.DefaultStreamName`)
  and is passed to `NewAggregator`.
- **Catalog publishing:** `internal/controller/catalog_publisher.go`.
  Items are **not broadcast** — `publishItemToMembers` loops over
  `KiosksForItem` (in `internal/controller/membership.go`) and writes one
  `<kiosk>.<item>` KV entry per member. The `kiosk_items` create/update
  hooks write a single key for that pair; the delete hook uses the
  capture-in-`OnRecordDelete` / emit-in-`OnRecordAfterDeleteSuccess`
  pattern because cascade deletes can void the FK records between the
  two phases. Item deletes have no direct hook — `CascadeDelete` on
  `kiosk_items.item` triggers the per-pair delete hooks automatically.
  Users still broadcast on a single key. To sync a new collection that
  is also kiosk-scoped, mirror the items pattern: add a membership
  table, add hooks that resolve `<kiosk_code>.<...>` keys.
- **Kiosk-side projection:** `internal/catalog/watcher.go`. `NewWatcher`
  takes the kiosk's own `kiosk_code`; items use
  `Watch(ctx, kioskCode + ".>")` so the wire never carries other kiosks'
  keys. `applyItem` strips the prefix before upserting locally. The
  watcher upserts records via the PB DAO (`app.FindFirstRecordByFilter`
  + `app.Save`), which bypasses collection rules cleanly. Kiosk-local
  fields (`quantity_on_hand`, `reorder_threshold`, `item_instances`)
  are intentionally not touched on update — they survive catalog
  resyncs.
- **Cross-fleet payload shape:** `internal/catalog/payload.go`. Single
  source of truth — both controller publisher and kiosk projector import
  from here. Adding a field is a one-line change that flows through both
  sides; the round-trip test enforces excluded fields stay excluded.
- **Command bus (controller → kiosk):** `internal/commands/` on the kiosk
  side, `internal/controller/inventory.go` on the controller side. The
  kiosk's `Dispatcher` is constructed with built-in handlers for
  `inventory.adjust` and `inventory.snapshot`; new commands register via
  `Dispatcher.HandleFunc(name, handler)`. Use `events.Conn(pub)` to get
  the underlying `*nats.Conn` (do not type-assert; the exported helper
  mirrors `events.JetStream`). The controller endpoint pattern is:
  fast-fail on stale heartbeat via `HeartbeatRegistry.IsLikelyOnline`,
  marshal payload, `nc.Request(subject, data, 5*time.Second)`, decode the
  `{success, error, data}` envelope, pass `data` through as a
  `json.RawMessage` to the SPA. **Commands must always reply** within the
  5 s window — even on validation errors — or the controller renders
  "kiosk offline" instead. Adding a mutating command means deciding on an
  idempotency key (the inventory.adjust pattern is server-generated UUID
  + unique-indexed column).
- **Heartbeats:** `internal/heartbeat/heartbeat.go` (kiosk side) +
  `internal/controller/heartbeats.go` (controller side). 45 s cadence on
  the kiosk; controller subscribes plain (NOT through the JetStream
  aggregator), keeps a mutex-guarded `map[code]time.Time`, exposes
  `Snapshot()`, `LastBeat(code)`, `IsLikelyOnline(code, freshness)`,
  and `StartedAt()` for restart-warmup-window suppression in the SPA.
  First beat from a previously-unknown kiosk triggers the optional
  `TouchFn` callback — wired to `agg.TouchKiosk` so heartbeat-only
  kiosks still appear in the `kiosks` collection. Don't re-subscribe on
  reconnect; `nats.go` re-establishes the sub automatically given
  `MaxReconnects(-1)`.

## Frontend notes

Vue 3 + TypeScript + `<script setup>` + Pinia + Reka UI + Tailwind 4. Two
distinct flows in one SPA:

- **Checkout flow** (`views/CheckoutView.vue`, `stores/session.ts`) — anonymous,
  driven by window-level scan events from `composables/useScan.ts`, talks to
  `/api/kiosk/*` via plain `fetch` (`lib/api.ts`).
- **Admin views** (`views/Admin*.vue`, `stores/auth.ts`) — authed via PocketBase
  JS SDK (`lib/pb.ts`), CRUDs the `users` and `items` collections via PB's REST
  API, plus hits `/api/kiosk/integrity`, the
  `/api/kiosk/{items,users,groups}/import` family (with matching
  `/template` downloads), and the controller-only `/api/controller/*`
  family for fleet liveness and remote inventory adjust.

The scan composable skips when an `<input>`, `<textarea>`, `<select>`, or
contenteditable has focus. If you're adding a screen where the scan flow
should keep working, don't put a focused input in it.

**Kiosk detail page.** On the controller, `/admin/kiosks` is a list view
that polls `/api/controller/kiosks/heartbeats` every 10 s for online
badges; clicking a row navigates to `/admin/kiosks/:code` (the
`AdminKioskDetailView` component) which has three tabs: Overview, Items
(the existing `KioskItemsPanel`), Inventory (new `KioskInventoryPanel`
that fetches a live snapshot via the controller's inventory endpoint and
drives adjust commands). The old `KioskDialog` was reduced to create-only;
edit/items/inventory work belongs on the detail page. The 503 `kiosk_offline`
body is detected via `ApiError.status === 503` and rendered as a banner —
do not treat it as a generic error.

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
