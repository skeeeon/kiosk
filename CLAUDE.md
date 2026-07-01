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

# Virtual timeclock terminal (optional) — authed self-service punch page
go build -o kiosk-timeclock ./cmd/timeclock
KIOSK_CONFIG=timeclock.yaml ./kiosk-timeclock   # expects timeclock.yaml; pb_data_timeclock/

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
go test ./internal/timeclock/...         # punch funnel + merge rule + pairing
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
  Today: `GET /api/controller/kiosks/heartbeats`, the inventory pair
  `GET .../kiosks/{code}/inventory` + `POST .../inventory/adjust`, the
  instances family `GET .../kiosks/{code}/instances` +
  `POST` (create) + `PATCH .../{instance_code}` (edit, carries `enclosure_id`)
  + `POST .../{instance_code}/status` (set_status), and the read-only
  `GET .../kiosks/{code}/metrics` + `GET .../kiosks/{code}/config` (RFID
  reader topology). All admin-gated
  via `controller.Handlers.requireAdmin` (mirrors the kiosk version —
  the duplicate is documented at `internal/controller/handlers.go:37`).
  The inventory/instance endpoints proxy core NATS request/reply commands
  to the target kiosk. Mutations pass the reply through unchanged; the two
  read snapshots (`inventory`, `instances`) are enriched before returning —
  the controller decorates them with the ledger-derived "out" count + item
  "type" (inventory) and per-instance out-status (instances) it computes
  from its own `ReplayOpenRows`, since the kiosk's snapshot doesn't carry
  them. The split lives in `fetchKioskData` (returns the reply for the
  caller to enrich) vs `dispatchKioskCommand` (sends it through). See
  `internal/controller/snapshot_enrich.go`.
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
   lost/damaged closes of **quantity-tracked** items only — serialized
   closes retire the instance instead and never emit
   `inventory.adjust`); `<prefix>.<kiosk_code>.event.integrity.rebuild`
   from the open_checkouts rebuild handler;
   `<prefix>.<kiosk_code>.event.checkout.admin_close` from
   `commit.AdminClose` (one per row closed; the matching transaction also
   rides `event.transaction.complete` and the line rides
   `event.item.admin_close`-equivalent via the regular `transaction_lines`
   projection); `<prefix>.<kiosk_code>.event.instance.lifecycle` from the
   `item_instances` PB record hooks and the shared `SetStatusInTx` writer on
   every `status` transition (`create` / `to_maintenance` /
   `return_to_service` / `retire` / `unretire`; the verb is derived from the
   prev→target pair; cosmetic edits skip) AND from `commit.AdminClose` when a
   serialized `lost`/`damaged` close retires the instance. The
   `event.instance.lifecycle` payload carries `prev_status`/`new_status` and
   `source_audit_id` (the kiosk-side `instance_audit.id`) so the controller's
   projection is idempotent under redelivery — same anchor strategy as
   `event.inventory.adjust`'s `adjustment_id`.
   Two further managed-mode notification subjects ride the commit path:
   `<prefix>.<kiosk_code>.event.alert.maintenance` — an instant, **batched
   one-per-transaction** alert listing every serialized unit a return routed
   into maintenance (dedup keyed on the transaction id) — and the scheduled
   `digest.maintenance` report (kiosk: local `item_instances` query;
   controller: live `instance.snapshot` fan-out filtered to
   `status=maintenance`). Both are rendered + sent via the controller's SMTP,
   same family as `alert.lowstock`.
   `<prefix>.<kiosk_code>.event.timeclock.punch` fires once per accepted
   punch (any source — self/foreman/admin/controller_admin), published by
   the punch funnel's callers via `handlers.PublishPunchEvent` (skipped on
   idempotent replays). The payload's `punch_id` (kiosk-side
   `time_punches.id`) is the controller projection's idempotency anchor
   (`source_punch_id`) — same strategy as `inventory.adjust`'s
   `adjustment_id`. The payload also carries an optional `job_code` (free-text
   job / work-order tag, supplied on a clock-in) — an optional attribution
   column on the punch ledger (same shape as `transactions.terminal_id`), threaded
   through every punch path and projected onto the controller's `time_punches`;
   display pairing carries it from the "in" punch onto the interval. It also
   carries an optional `note` (free-text per-punch annotation, allowed on
   either direction and any source — distinct from `reason`, which is required
   only for admin/corrective punches); pairing surfaces both ends of an
   interval as `note` (from the "in") and `out_note` (from the "out").
   `<prefix>.<kiosk_code>.event.scan.rfid.observed` fires after every
   LLRP inventory cycle in either RFID mode and carries the full
   deduplicated EPC array (the read-window observability stream —
   no projector consumes it today; reserved for future drift
   detection and analytics). The prefix is `"kiosk"` by default and
   configurable via `nats.subject_prefix`; subjects are built through
   shared helpers in `internal/events/subjects.go` (single source of
   truth for both the kiosk publisher and the controller's
   stream/consumer filters) — don't re-string-format these at callsites.

   The other families:

   - **Heartbeats** — `<prefix>.<kiosk_code>.heartbeat`. Built via
     `events.HeartbeatSubject` / `events.HeartbeatFilter`. Last-write-wins,
     no persistence; durability would mask the very signal we care about.
   - **Sightings** — `<prefix>.<node_code>.sighting.raw`. Built via
     `events.SightingSubject` / `events.SightingFilter`. The lossy
     last-write-wins family for advisory asset **location** (the
     location/sightings feature; see [Location](docs/location-sightings-plan.md)).
     **Gateways are external publishers** (off-platform — RFID-over-MQTT into
     NATS's MQTT interface, or an HTTP→NATS bridge), NOT a kiosk reader mode;
     the platform only consumes. The wire shape is always **raw** (carries a
     `tag_id`, never a resolved instance code) and resolution is
     subscriber-side: a standalone node resolves via the scan resolver
     (`internal/sightings.ApplySighting` + the node subscriber), the controller
     via `instance_epc_index` (`internal/controller.SightingIngest`, plain
     subscribe). Last-observed is mirrored back to the owning node via the
     `last_observed_state` KV bucket, sliced per node like `catalog_items`
     (NOT WatchAll). Publish via the raw conn (`PublishBytes` /
     `sightings.PublishCustodyReads`), never `events.Publish`.
   - **Commands** — `<prefix>.<kiosk_code>.command.<name>` (built via
     `events.CommandSubject` / `events.CommandSubscribePattern`). Request/
     reply, single attempt, ≤5 s reply timeout. The kiosk's dispatcher
     replies on `msg.Reply` with a `{success, error, data}` envelope.
     Built-ins today: `inventory.adjust`, `inventory.snapshot`,
     `checkout.close`, the `instance.*` family
     (`create`/`edit`/`set_status`/`snapshot` — `set_status` carries the
     target status as data and covers send-to-maintenance / return-to-service
     / retire / un-retire in one command — `create`/`edit` also carry the
     unit's `enclosure_id`, the enclosure_diff cabinet assignment),
     `metrics.snapshot`, `config.snapshot` (read-only RFID reader/enclosure
     topology — drives the controller's Readers tab), `integrity.rebuild`,
     `ledger.republish`, the timeclock pair
     `timeclock.punch` + `timeclock.republish` (controller-admin punch
     recorded AT the kiosk — kiosks are the only punch writers — and the
     punch-events backfill walk; both reach config + the punch-state
     fleet replica through `Dispatcher.KioskHandlers`), and the RFID
     enclosure_diff pair `cart.start` + `read.trigger`. The
     enclosure_diff handlers reach the cart store / SSE broker /
     reader through `Dispatcher.KioskHandlers`, set in
     `cmd/kiosk/main.go` after both `*handlers.Handlers` and the
     dispatcher exist.

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
and vanish on process restart. A single screen has one active user at a time; a
multi-terminal node (several doors / screens on one binary) can hold several
concurrent carts, but they never collide — every method takes the store mutex
and carts are keyed by `cart_id`, with the badge `Start` path resolving one cart
per `UserID`. The mutex already covers this; don't add further concurrency
primitives. The HTTP handler accepts a `cart_id` from the client and dispatches
against this store; commit drops the cart after successfully promoting to a
transaction. A secondary `byUserEnclosure` index maps
`(user_code, enclosure_id) → *Cart` for the RFID `enclosure_diff` flow's
`cart.start` idempotency — only populated by `Store.StartByExternal`;
counter_scan and badge-driven carts don't touch it. (`terminal_id` is
attribution-only and never a cart-session key; the enclosure is the partition.)

**Cart state changes notify subscribers via SSE.** `internal/cartevents`
is a tiny per-cart pub/sub broker: Subscribe/Tickle/Close keyed by
`cart_id`. Every cart write path (CartAdd, CartUpdateLine, CartDeleteLine,
CartForemanReturn, RFIDScan, PerformReadTrigger) calls `h.CartEvents.Tickle`
exactly once at the end; CartCommit and CartCancel call `Close`. The
SPA opens an EventSource on `GET /api/kiosk/cart/events?cart_id=…`,
listens for `cart.updated` / `cart.gone`, and refetches via
`GET /api/kiosk/cart` on every tickle — "push the signal, pull the
data." This exists so server-driven writes (RFID's NATS-orchestrated
`cart.start` / `read.trigger`, future controller-side cart writes,
spectator clients) can keep the SPA in sync without polling. The
shared `addCodeToCart` helper deliberately does NOT tickle — RFIDScan
calls it in a loop and we want one tickle per HTTP write, not N.

**RFID lives in `internal/rfid`.** A node configures a **map of readers**
(`rfid.readers`, keyed by `reader_id`), each its own long-lived LLRP client
(EdgeX `device-rfid-llrp-go/pkg/llrp`, imported at a `@main`
pseudo-version because EdgeX's v4 tags are unreachable to Go's
module system). Mode is **per-reader**, so one node can host `counter_scan` +
`enclosure_diff` readers at once. `handlers.Readers` (a `map[reader_id]*ReaderHandle`,
each `{Reader, Mode, EnclosureID}`) is the runtime view; selection goes through
`ReaderByID` (empty id → the sole reader; explicit id → that one — this is what
the counter_scan `?reader=` URL param drives) and `ReaderForEnclosure`
(enclosure_diff, matches the cabinet by `enclosure_id`). `counter_scan` is
button-driven (`POST /api/kiosk/cart/rfid-scan?cart_id=…&reader=…`);
`enclosure_diff` is NATS-driven via the `cart.start` and `read.trigger` commands
above. `internal/rfid/diff.go` is a pure function reconciling
observed EPCs against expected-present state (non-retired serialized
instances + open_checkouts) — no I/O, no kiosk state, exhaustively
table-tested. When a node hosts more than one cabinet
(`enclosureCount() > 1`), `handlers.expectedInstanceStates` scopes that set to
the cabinet being read via `item_instances.enclosure_id`; a single-cabinet node
(or any unassigned instance) keeps the whole-inventory set, so existing
deployments are unaffected and `rfid.Diff` itself is untouched. Each expected
instance carries an `Eligible` flag
(`status == in_service`): a **maintenance** unit is expected-present
(physically in the enclosure) but if it leaves it is skip-and-counted
(`SkippedIneligible`), never synthesized as a checkout — commit would
reject a checkout of a non-in_service unit. The cross-user-return policy
is likewise "skip and count" rather than synthesize (the commit-time
foreman gate would reject the line anyway); both counts surface in the
SPA toast. See [RFID](docs/rfid.md) for the full design.

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
instance RFID → instance BLE → user code. RFID/BLE are **instance-only** —
EPCs and BLE beacon ids are per-tag and live on `item_instances`
(`rfid_epc` / `ble_id`), never on the SKU. The BLE leg makes sighting
resolution source-agnostic (RFID vs BLE differs only here). Adding a new scan
type means adding to the dispatch chain in `Resolver.Resolve`, not sprinkling
lookups through handlers.

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
`2000400000_instance_lifecycle_audit.go`,
`2000500000_open_checkouts_kiosk_code.go`,
`2000600000_applied_oc_closes.go` (created the open_checkouts
close-projection idempotency guard — now removed, see below),
`2000700000_tx_lines_source_instance.go` (`source_item_instance_id` on
transaction_lines, so the ledger replay can pair a serialized
checkout with its return — see the Controller seam section), and
`2000800000_drop_applied_oc_closes.go` (drops the now-unused guard table:
the controller no longer materializes open_checkouts, it replays the
ledger on demand), and `2000900000_instance_lifecycle_audit_status.go`
(widens `instance_lifecycle_audit.action` to the status verb set and
replaces `prev_active`/`new_active` with `prev_status`/`new_status`, mirroring
the kiosk-side change).

Recent kiosk-side migrations for the instance-status / maintenance work:
`1794000000_instance_status.go` (adds `item_instances.status`, backfills from
`active`, drops `active`), `1795000000_items_requires_maintenance.go`
(`items.requires_maintenance_on_return` bool), `1796000000_instance_audit_status.go`
(widens `instance_audit.action`, swaps `prev_active`/`new_active` →
`prev_status`/`new_status`), `1797000000_maintenance_alerts.go` (seeds the
`alert.maintenance` template), and `1798000000_maintenance_digest.go` (extends
`scheduled_reports.report_key` with `maintenance` + seeds the `digest.maintenance`
template).

Timeclock migrations: kiosk-side `1799000000_time_punches.go` (the
append-only punch ledger — API-readonly, funnel-only writes),
`1799100000_timeclock_digest.go` (extends `report_key` with `timeclock` +
seeds the `digest.timeclock` template), and
`1799200000_timeclock_self_digest.go` (extends `report_key` with
`timeclock_self` + seeds the per-worker `digest.timeclock_self` template —
see the scheduler fan-out note below),
`1799300000_time_punches_job_code.go` (adds the optional `job_code` column),
and `1799400000_time_punches_note.go` (adds the optional `note` column — both
shared kiosk migrations, so the controller/timeclock DBs get them too and the
aggregator projects the fleet's values through); controller-side
`2001100000_time_punches_source.go` (adds `source_punch_id`
unique-when-non-empty — the projection's idempotency anchor — plus
`source_actor` for kiosk-admin actors whose FK can't resolve in the
controller's DB, and a `(kiosk_code, occurred_at)` index).

Asset-tracker migrations (the jobsite custody + RFID-partition generalization —
see `docs/asset-tracker-plan.md`): kiosk-side
`1801000000_rename_door_to_terminal.go` (renames `transactions.door_id` →
`terminal_id` in place + reindexes, adds nullable `enclosure_id` + index) and
`1802000000_instance_enclosure_id.go` (adds `item_instances.enclosure_id` +
index — the enclosure_diff cabinet a serialized unit lives in, nullable, no
backfill); controller-side `2001300000_rename_door_to_terminal.go` which
**converges** rather than blind-renames (the controller runs both kiosk and
controller migrations on the shared `transactions` collection, so it renames
only if `terminal_id` is absent, drops any leftover `door_id`, and ensures the
`terminal_id` index + `enclosure_id`).

Location/sightings migrations (the advisory asset-location layer — see
`docs/location-sightings-plan.md`): kiosk-side
`1803000000_instance_last_observed.go` (adds the advisory
`item_instances.last_observed_*` columns — `_at`/`_zone`/`_gateway`/`_lat`/`_lon`,
nullable, kiosk-local) and `1804000000_instance_ble_id.go` (adds
`item_instances.ble_id`, the BLE analog of `rfid_epc`); controller-side
`2001400000_instance_location.go` (the fleet `instance_location` view, unique on
`(kiosk_code, instance_code)`) and `2001500000_instance_epc_index.go`
(the `rfid_epc` → owning-unit map the `SightingIngest` resolves against).

**Scheduled-report delivery shapes.** `internal/scheduler` dispatches each
`scheduled_reports` row through one of two registries: `reportRunners` (the
default — 1 row → 1 rendered context → 1 send, recipients taken from the row)
and `fanOutRunners` (1 row → N sends — the runner is handed the `Sender` and
performs its own per-recipient sends; `runOnce` only stamps the row's status,
and only a *structural* error fails the row, never a single recipient's
bounce). `timeclock_self` (the per-worker timesheet, event type
`digest.timeclock_self`) is the first fan-out: it groups paired day-totals per
worker and emails each **active** worker their own scoped summary via
`Recipients{WorkerEmail:true}` + the context's `WorkerEmail()` (the row's
recipients column is intentionally ignored on this path; the SPA hides the
recipients editor for it). Like `runTimeclockDigest` it is pure-DB and runs
unchanged on both binaries — standalone reads local punches, the controller its
fleet projection — with **no** NATS / `RegisterRunner` override (unlike the
maintenance / open-checkouts fan-outs, which need live per-kiosk snapshots), so
a fleet-wide controller row gives each worker their fleet-complete timesheet.
Both `cmd/kiosk` and `cmd/timeclock` run the scheduler in standalone mode and
skip it entirely in managed mode (the controller owns the schedule rows, cron,
and SMTP send there); so for any one deployment exactly one binary runs it and
there is no double-send. On a standalone virtual timeclock terminal this is what
makes the `digest.timeclock` / `digest.timeclock_self` reports actually fire.

`touchKiosk` in `internal/controller/consumer.go` advances
`last_transaction_at` only from the event's own `completed_at`,
monotonically (never wall-clock `now()`) — so a redelivery or a
`ledger.republish` of an old transaction can't drag it forward, and the
heartbeat auto-register path (which has no transaction time) leaves it
empty until a real transaction lands. The SPA reads `last_transaction_at`. **Critical change in this release:** `touchKiosk`
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

**Instance lifecycle is a `status` enum, not a bool.** `item_instances.status`
is `in_service | maintenance | retired` (migration 1794 replaced the old
`active` bool + the removed hard delete). `in_service` is checkout-eligible;
`maintenance` is owned-but-parked (counts toward on-hand, excluded from
`available`); `retired` is terminal-but-reversible (un-retire allowed) and is
the destination for what used to be a delete — **units are never hard-deleted**
(the ledger keeps their FKs alive). "Out / checked out" is **not** in the enum;
it stays derived from `open_checkouts`. The single in-transaction status writer
is `instances/status.SetStatusInTx` (a leaf sub-package so `internal/commit` can
drive a transition without importing the whole `instances` package and closing
an import cycle; `internal/instances` re-exports its names via aliases). It
writes the `instance_audit` row (`prev_status`/`new_status` + a verb derived
from the transition) and the post-commit `instance.lifecycle` event. The
HTTP/command path uses `instances.PerformSetStatus` (target as data; idempotent
on `command_id`). Maintenance is entered by (a) per-SKU
`items.requires_maintenance_on_return`, (b) a per-line "needs maintenance"
toggle any worker can set on a serialized return (both applied inside
`commit.Commit` after the open_checkouts row is deleted), or (c) a manual
admin send-to-maintenance via `set_status`.

**Serialized `quantity_on_hand` is derived, not stored-by-hand.** For
`tracking_mode="serialized"` items, `quantity_on_hand` is a materialized
view of the **non-retired** `item_instances` count — `in_service` +
`maintenance`; `retired` excluded (`instances.CountNonRetired`, same spirit as
`open_checkouts`). `PerformStockAdjustment` rejects serialized items
outright (`ErrSerializedNotAdjustable` → HTTP 400 / NATS reply error), so
their count moves only through the instance lifecycle. The recompute
lives in **one place**: model-level `OnRecordAfter{Create,Update,Delete}Success`
hooks on `item_instances` in `internal/instances/hooks.go` call
`instances.RecomputeItemQuantity` (a full re-count from source, no-op for
non-serialized / missing items). Because PB defers in-transaction
after-success hooks to post-commit on the parent app (see `core/db.go`),
this single binding covers every write path — admin SPA / superuser REST,
the controller command-bus `Perform*` mutations, and `commit.AdminClose`'s
retire — so neither `mutations.go` nor `admin_close.go` recomputes
explicitly (admin_close just skips the qty stock-adjustment for serialized
and lets the retire drive the count). The `item_instances` create hook also
defaults `status` to `in_service` when empty, so seeds / CSV / superuser
quick-add satisfy the Required field. CSV import ignores a
`quantity_on_hand` column for serialized rows. The one-shot backfill
`migrations/1793000000_backfill_serialized_qty.go` reconciles pre-existing
rows.

**Timeclock is a second append-only ledger with the same discipline as the
tool ledger.** `time_punches` (one row per clock-in/out punch) is written
ONLY by `timeclock.PerformPunch` in `internal/timeclock` — a LEAF package
(imports only events/kioskctx/dberr + PB core, same cycle-avoidance
precedent as `instances/status`) so `internal/commit` can consult
clocked-in state. Dependency direction: commit/handlers/commands/controller
→ timeclock; timeclock never imports cart/commit/handlers. There is
deliberately **no materialized open-shifts table**: "is this user clocked
in" = latest punch by `occurred_at` (`created` breaks ties), merged with
the fleet replica — `timeclock.CurrentState` is THE single merge-rule
function ("fresher of local ledger vs `punch_state` KV replica wins").
Funnel rules: live (self/foreman) punches enforce in/out alternation and
target-active, are always stamped `now()`, and can never force; foremen
punch only crew in their own group (role+group re-read inside the txn —
commit's gate pattern); admin/controller_admin punches may backdate and
bypass alternation but always require a reason; `force=true` (admin only)
bypasses the open-checkouts clock-out block — the escape hatch for "drove
home with a tool." Idempotency on `command_id` uses the exact
stock_adjust two-layer dance. **Interlocks** (config-gated, default off):
`require_clock_in_for_checkout` rejects whole commits containing
checkout/consume lines when the cart user isn't clocked in (inside
commit's txn via `Policy.RequireClockInForCheckout` + `Policy.PunchFleet`;
returns-only carts pass by construction; `CartCommit` maps the wrapped
`timeclock.ErrNotClockedIn` to a 409 `{error:"not_clocked_in"}` the SPA
turns into a one-tap "clock in now?" — keep that carve-out when touching
commit error handling). `block_clock_out_with_open_checkouts` rejects
clock-outs while the worker has open checkouts. The funnel merges THIS
kiosk's local `open_checkouts` with the fleet-wide replica (the
controller-written `open_checkouts_state` KV bucket, hydrated into a
`timeclock.CheckoutFleet` via `timeclock.CheckoutWatcher` — sibling of the
punch-state replica), partitioned by `kiosk_code` so the two views are
disjoint and never double-count. So a clock-out at any surface sees tools out
at OTHER kiosks; the error carries the per-row `kiosk_code` ("return it at
that building"). It's eventually consistent and fail-open: a nil/empty replica
(standalone, KV down) blocks on local rows only. Admin `force=true` overrides;
a **self/foreman `force=true`** is the worker's "clock out anyway"
acknowledgment (same column, told apart by `source` — see
`PerformPunch`/`PunchInput.Force`). The funnel only counts; the HTTP layer
hydrates the merged display list via `Handlers.mergedOpenCheckoutsForUser`.
**Timeclock-only mode** (`timeclock.timeclock_only`, requires `enabled`)
turns the device into a dedicated punch station: the SPA replaces the
checkout splash with a persistent `TimeclockPanel` (`standalone` prop)
and routes badge scans straight to it; item scans toast. Presentation
only — no backend changes, carts simply never start. A punch-only station
writes no local `open_checkouts`, so the clock-out block has nothing LOCAL to
block on — but in managed mode it still blocks on the fleet replica (tools out
at other kiosks); standalone, it's a no-op (no replica).
**Cross-kiosk punches work in managed mode**: the controller projects
each punch into its own `time_punches` and broadcasts per-user state into
the `punch_state` KV bucket (key = `user_code`, like `catalog_users`;
monotonic on `occurred_at` via `shouldReplacePunchState`); kiosks hydrate
an in-memory `timeclock.Fleet` via `timeclock.Watcher` (WatchAll replays
the bucket on start, so restarts recover; the replica is advisory — KV
failures degrade to local-only and self-heal). Reporting: pairing punches
into intervals/day-totals is DISPLAY logic (`timeclock.Pair`, pure,
table-tested); the raw-punch CSV is the payroll contract — never add
rounding/overtime/pay-period logic anywhere in this codebase (same stance
as billing).

**Virtual timeclock terminal** is a THIRD binary, `cmd/timeclock`
(`pb_data_timeclock/`, `timeclock.yaml`), modeled on `cmd/controller`: it
reuses the internal packages but registers a deliberately narrow route set.
It's a publicly-reachable, per-user-**authenticated** self-service punch
page (workers clock in/out from phones — no badge scan, no hardware). The
trust model is INVERTED from a kiosk: instead of the box being the trust
boundary, each worker authenticates and the punched identity is read from
`re.Auth`, NEVER the body — the same server-resolved-identity discipline as
`OriginalCheckoutUserID`. The authed endpoints live in
`internal/handlers/timeclock_self.go` (`SelfTimeclockStatus` /
`SelfTimeclockPunch` / `SelfTimeclockHistory`, gated by `requireWorker`
which mirrors `requireAdmin` but checks the `users` collection + `active`)
and force `SourceSelf` — no foreman/admin/backdate/force powers from a
phone. Beyond that authed self-service surface the binary registers
identity/branding, the **admin-gated** timeclock reporting + notification
endpoints the admin SPA needs (`/api/kiosk/timeclock/{now,history,admin-punch}`,
`/api/kiosk/reports/timeclock.csv`, the `/api/kiosk/notifications` template
trio), and admin user-import — and, in standalone mode, runs the scheduler
(see the scheduled-report section). The anonymous `/api/kiosk/*`
checkout/cart/inventory surface is still never registered, so it can't be
exposed (security by construction, the `cmd/controller`-has-no-kiosk-handlers
precedent). The shared admin SPA further hides its checkout-world views
(Items, Metrics, the non-timeclock Report tabs, non-worker imports) and scopes
the notification-template editor to the timeclock digests when
`identity.timeclock_virtual` is set. Worker login is enabled
by a `cmd/timeclock`-only migration package `migrations/timeclock`
(`package timeclockmigrations`, same isolation pattern as
`migrations/controller`) that sets `users.AuthRule = "active = true"` +
`OAuth2.Enabled = true` on that DB only; a runtime
`OnRecordAuthWithOAuth2Request` guard in `main()` rejects `IsNewRecord`
(match-only — un-provisioned IdP accounts can't self-enroll). Both OAuth2
SSO and password auth are supported (`UserPayload.Email` already syncs, so
OAuth2-by-email needs no payload change; password workers use PB's
reset-by-email flow since the catalog seeds a random password). The flag is
`timeclock.virtual` (requires `enabled`; surfaced to the SPA as
`identity.timeclock_virtual`, which routes the SPA to
`VirtualTimeclockView` — login screen → self-punch panel, using the
persistent `pbWorker` client + `useWorkerAuthStore`, with `lib/api.ts`
attaching the worker token for `/api/self/*` by URL prefix). It supports the
SAME three modes as `cmd/kiosk` (standalone / standalone+NATS /
controller-managed) with identical best-effort wiring — in unmanaged modes
workers are provisioned locally and `PunchFleet`/`CheckoutFleet` are nil
(local-only state); managed mode adds the catalog-synced workers + the
`punch_state` and `open_checkouts_state` replicas. The fleet-wide clock-out
gate (above) is what makes a phone clock-out — which has no local
`open_checkouts` at all — able to see and block on a worker's tools out across
the fleet.

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
  `transaction_lines`, `inventory.adjust` → `inventory_audit`,
  `instance.lifecycle` → `instance_lifecycle_audit`,
  `timeclock.punch` → `time_punches` (idempotent on `source_punch_id`;
  after a successful project the aggregator also writes the user's state
  to the `punch_state` KV bucket — monotonic, advisory, never blocks the
  ack; no `touchKiosk` from this branch), and
  `checkout.admin_close` → `transactions` + `transaction_lines` (via
  `ProjectAdminCloseToLedger`, which mirrors the ledger rows
  `commit.AdminClose` writes locally: a completed transaction + one
  `admin_close` line. admin_close publishes ONLY `checkout.admin_close`,
  never `transaction.complete` or `item.*`, so that subject is the
  controller's sole view of an admin close — projecting it as a line is
  what lets the replay drop the row). The controller does **not**
  materialize an `open_checkouts` table; "what's currently out" is computed
  on demand by replaying `transaction_lines` (`ledger.ReplayOpenRows`, the
  same path the kiosk uses), so it is convergent by construction and cannot
  drift from a kiosk's. Serialized returns pair via `source_item_instance_id`
  on the projected line; the matching mirrors commit exactly —
  target-user-only on returns, **no** cross-user borrowing
  (`ledger.removeRows`). After a successful line projection (and the
  admin_close line), the aggregator also recomputes the affected user's
  fleet-wide open set — `ledger.ReplayOpenRowsForUser` (a per-user replay
  over the union of the user's own transactions plus any transaction whose
  return/close names them as `original_checkout_user`, so a foreman's
  cross-user return still closes the holder's row) — and writes it to the
  `open_checkouts_state` KV bucket keyed by `user_code`
  (`refreshOpenCheckoutsState`, always writing incl. the empty case so a
  return clears the gate). That bucket is the cross-kiosk clock-out gate's
  replica; advisory + best-effort, same posture as `punch_state`. `integrity.rebuild` reaches the controller
  ack-and-log only — there is nothing materialized to rebuild. The two
  managed-mode notification subjects `receipt.transaction` and
  `alert.lowstock` ride the same durable consumer but are dispatched in
  `handle()` before the flat `EventPayload` decode (they carry nested
  context payloads) and are rendered + sent via the controller's central
  SMTP rather than projected into the ledger. The audit projections are
  idempotent against unique-when-non-empty indexes on
  `source_adjustment_id` / `source_audit_id`; the ledger projections
  (`transactions`, `transaction_lines`, including the admin_close pair) are
  idempotent against `source_transaction_id` / `source_line_id`. JetStream
  redelivery is a no-op across all of them. The stream name comes from
  `cfg.NATS.StreamName` (default `events.DefaultStreamName`) and is passed
  to `NewAggregator`.
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
`AdminKioskDetailView` component) which has tabs: Overview, Items
(the existing `KioskItemsPanel`), Inventory (`KioskInventoryPanel`),
Instances (`KioskInstancesPanel`), and Metrics (`KioskMetricsPanel`, a
live operational + activity snapshot proxied via the `metrics.snapshot`
command). The Inventory and Instances panels
fetch an enriched live snapshot via the controller's endpoints and show
on-hand / out / available / low-stock and per-instance status at the
same fidelity as the local kiosk admin UI — out/available are derived
controller-side from the projected ledger and computed in the SPA via the
shared `ui/src/lib/inventory.ts` helper (the same one `AdminItemsView`
uses, so the two views can't drift). `availableFor` subtracts a `maintenance`
count (carried per-item in the inventory snapshot) alongside `out`. The
Instances panel renders the lifecycle status badge and offers the
transitions valid for the row's status (send to maintenance / return to
service / retire / un-retire), each posting to
`POST /api/controller/kiosks/{code}/instances/{instance_code}/status` with a
required reason — the verb-visibility logic lives in the shared
`ui/src/lib/instanceStatus.ts` helper, reused by the local `ItemInstancesPanel`.
The old `KioskDialog` was reduced to
create-only; edit/items/inventory work belongs on the detail page. The 503 `kiosk_offline`
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
