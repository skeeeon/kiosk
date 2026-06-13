# Central controller (kiosk-controller)

Optional. Stand-alone kiosks work without any of this. Bring up a
controller when you have more than one kiosk and want a single source
of truth for catalog plus a unified transaction ledger.

## What it does

- **Catalog down → kiosks.** The controller is the source of truth for
  `items` and `users`. Items are **not broadcast** — each kiosk's stock
  is governed by explicit `kiosk_items` membership rows on the
  controller. A row exists → that kiosk sees that item; no row → it
  doesn't. New SKUs do not auto-flow anywhere; admins add them via the
  kiosk's "Stocked items" panel or the bulk-add-by-category action. The
  shared `catalog_items` KV bucket uses namespaced keys
  `<kiosk_code>.<item_code>`, and each kiosk's watcher subscribes only
  to its own prefix (`Watch("KIOSK01.>")`), so the wire never carries
  items the kiosk shouldn't see. Users remain org-wide on
  `catalog_users`. Kiosk-local state (`quantity_on_hand`,
  `reorder_threshold`, `item_instances`) is intentionally not synced
  and survives catalog updates untouched.
- **Transactions up → controller.** Every kiosk already publishes
  `{prefix}.{code}.event.transaction.complete` and
  `{prefix}.{code}.event.item.{action}` when NATS is enabled (`{prefix}`
  defaults to `kiosk`). The controller runs a JetStream durable
  consumer (`controller-aggregator`) over the `KIOSK_EVENTS` stream
  (configurable via `nats.stream_name`) that projects every incoming
  event into its own `transactions` / `transaction_lines` rows.
  Idempotency keys (`source_kiosk_code + source_transaction_id` on
  transactions, `source_line_id` on lines) make redelivery safe.
- **Timeclock punches up, clocked-in state down.** When a managed kiosk
  records a punch it publishes `{prefix}.{code}.event.timeclock.punch`; the
  aggregator projects it into the controller's own `time_punches`
  (idempotent on `source_punch_id`) and writes the worker's merged state to
  the `punch_state` KV bucket (keyed by `user_code`, monotonic on
  `occurred_at`). Every kiosk watches that bucket into an in-memory replica,
  so clocked-in state is fleet-wide — a worker can clock in at one kiosk and
  out at another. The **virtual timeclock terminal** (`cmd/timeclock`) is
  just another managed kiosk on this path: it authenticates workers, writes
  punches to its own ledger, and rides the same projection + replica — so a
  phone punch shows up fleet-wide and vice-versa. Kiosks remain the only
  punch writers; a controller-admin punch is recorded *at* the target kiosk
  via the `timeclock.punch` command.
- **Kiosks registry.** Three ways a kiosk gets a row in the
  controller's `kiosks` collection: (1) **pre-registered** by an admin
  via the "New kiosk" button on AdminKiosksView — required if you want
  to assign items before the kiosk has phoned home; (2)
  **self-registered** the first time the aggregator sees a
  `transaction.complete` event from a `kiosk_code` it doesn't know;
  (3) **heartbeat auto-register** — the first heartbeat from a new
  kiosk also creates the row, so kiosks that haven't yet transacted
  still appear in the registry. All three converge on the same row
  with `status=unknown`. `last_transaction_at` advances on
  `transaction.complete` only; live online status comes from the
  in-memory heartbeat map (see below), not from the persisted timestamp.
- **Heartbeat + online status.** Each managed kiosk publishes a small
  JSON beacon on `{prefix}.{kiosk_code}.heartbeat` every 45 s using
  core NATS (not JetStream — missing a beat is the entire point of the
  signal). The controller subscribes plainly, keeps a mutex-guarded
  `map[code]time.Time` in memory, and serves it at
  `GET /api/controller/kiosks/heartbeats`. The SPA polls every 10 s
  and renders three-state badges: **online** (<90 s), **stale**
  (90 s–5 min), **offline** (>5 min). For ~90 s after a controller
  restart the SPA shows "unknown" to avoid painting a fleet red while
  beats catch up.
- **Inventory commands**
  (`POST /api/controller/kiosks/{code}/inventory/adjust`,
  `GET /api/controller/kiosks/{code}/inventory`). The controller
  proxies admin clicks to the target kiosk over core NATS
  request/reply on `{prefix}.{kiosk_code}.command.<name>`. The kiosk
  runs the same `PerformStockAdjustment` business logic the local
  HTTP path does — including rejecting serialized items, whose
  quantity is derived from their non-retired instance count (manage those
  via the Instances tab) — then publishes the same `inventory.adjust`
  event the aggregator already knows about. Idempotency is server-side: the
  controller generates a UUID `command_id`; the kiosk's
  `stock_adjustments` schema has it as a unique column, so a retried
  command returns the prior result instead of double-applying. The
  endpoints fast-fail with **503 `{error: "kiosk_offline",
  kiosk_code, command_id}`** when the kiosk's heartbeat is stale, so
  the SPA doesn't wait 5 s for a NATS timeout to render "offline."
- **Instance management commands** mirror the inventory family for
  serialized units: `instance.snapshot` (read), `instance.create`,
  `instance.edit` (cosmetic — no audit, no lifecycle event), and
  `instance.set_status` (one command for every lifecycle transition —
  send to maintenance, return to service, retire, un-retire — with the
  target `status` ∈ {in_service, maintenance, retired} carried as data
  and the audit verb derived kiosk-side from the transition). Mutations
  write to the kiosk's `item_instances` + `instance_audit` and emit the
  same `instance.lifecycle` event the SPA-driven PB-hook path emits; the
  controller's existing projection (`instance_lifecycle_audit`) can't
  tell where the mutation originated except via the `source` field
  (`local` vs `controller`). Idempotency anchor is the unique-when-non-
  empty `command_id` index on `instance_audit`. The Instances tab on
  the controller's per-kiosk detail page drives all four.
- **Maintenance commands.** Two operator tools for fixing or refilling
  state on a remote kiosk:
  - `integrity.rebuild`
    (`POST /api/controller/kiosks/{code}/integrity/rebuild`) wipes the
    kiosk's `open_checkouts` and replays it from the ledger. Use when
    drift is suspected.
  - `ledger.republish`
    (`POST /api/controller/kiosks/{code}/ledger/republish` with optional
    `{from, to}` RFC3339 window) re-emits `transaction.complete` +
    `item.{action}` events. The controller's projection is unique-
    indexed on `source_line_id`, so duplicates are no-ops. Use when
    events were lost (NATS outage during the kiosk's startup window).
  Both share the kiosk-side business logic the local
  `/api/kiosk/integrity/rebuild` + `/api/kiosk/ledger/republish`
  endpoints use, so behavior is identical to a kiosk admin running
  them at the touchscreen. Buttons live in the Overview tab of the
  per-kiosk detail page.
- **Notifications, centralized.** Managed kiosks publish three
  notification subjects on the same JetStream stream —
  `receipt.transaction` (one per commit), `alert.lowstock` (one per
  threshold cross), and `alert.maintenance` (one per transaction that
  routes returned serialized units into maintenance, batched). The
  aggregator dispatches each to the controller's
  `notifications.Notifier`, which renders against the controller's
  fleet-global `notification_templates` rows, sends via the
  controller's PocketBase SMTP, and writes to the controller's
  `notification_send_log`. Scheduled digests don't ride NATS at all in
  managed mode — the controller owns the `scheduled_reports` rows and
  the scheduler; the kiosk's scheduler stays off entirely. A new
  "Items in maintenance" digest (`digest.maintenance`) fans out
  `instance.snapshot` to every online kiosk, filtered to
  `status=maintenance`, listing offline kiosks as excluded. The timeclock
  digests need no such fan-out — the punch ledger is fully projected, so
  `digest.timeclock` (one admin email) and the per-worker
  `digest.timeclock_self` (one private email per active worker) both run
  pure-DB against the controller's `time_punches`; a fleet-wide row gives
  each worker their fleet-complete hours. The CRUD
  endpoints (`/api/controller/notifications`) mirror the kiosk's; the
  SPA detects role at boot and points the Templates tab at the right
  base URL. See [Notifications](notifications.md) for the full picture.
- **Inventory adjustment audit.** Every `inventory.adjust` event the
  aggregator receives is projected into the controller's
  `inventory_audit` collection — denormalized
  (`kiosk_code`, `item_code`, `item_name`, `delta`, `prev_quantity`,
  `new_quantity`, `reason`, `source`, `admin_id`). Idempotent via a
  unique-when-non-empty `source_adjustment_id` index, so JetStream
  redelivery is a no-op. Drives the Reports → Adjustment audit tab so
  operators can answer "every stock change, every kiosk, who did it
  and when" without hopping kiosks.
- **Instance lifecycle audit.** Every `instance.lifecycle` event the
  aggregator receives is projected into the controller's
  `instance_lifecycle_audit` collection — denormalized
  (`kiosk_code`, `item_code`, `instance_id`, `instance_code`, `action`,
  `prev_status`, `new_status`, `reason`, `source`, `admin_id`).
  Idempotent via a unique-when-non-empty `source_audit_id` index that
  carries the kiosk-side `instance_audit.id` of the row that generated
  the event. Drives the Reports → Instance lifecycle tab; the
  standalone-kiosk SPA shows the same tab against its local
  `instance_audit`, so feature parity holds across managed and
  unmanaged deployments.
- **Admin force-close forwarding.**
  `POST /api/controller/kiosks/{code}/checkouts/{source_line_id}/close`
  forwards a force-close to a remote kiosk over NATS request/reply
  (`checkout.close` command). The kiosk-side handler converges on
  `commit.AdminClose`, so the close behaves identically to a locally
  initiated close: same ledger writes, same qty side-effect for
  `lost` / `damaged`, same `instance.lifecycle` event (action=`retire`)
  when a `lost` / `damaged` close retires a serialized unit. The
  controller server-generates the `command_id` so
  retries are idempotent end-to-end (the kiosk's `transactions.command_id`
  unique-when-non-empty index catches duplicates). An admin close publishes
  only the `checkout.admin_close` event — never `transaction.complete` or
  `item.*` — so the aggregator projects it specifically:
  `ProjectAdminCloseToLedger` writes the same ledger rows the kiosk records
  locally (a completed `transactions` row + one `admin_close`
  `transaction_lines` row), idempotent on `source_transaction_id` /
  `source_line_id`. Because "what's currently out" is computed by replaying
  `transaction_lines`, projecting the close as a line is what makes the
  holder's row drop on the controller — there is no separate
  `open_checkouts` mutation.
- **Fleet-wide low-stock report.**
  `GET /api/controller/reports/low-stock` fans `inventory.snapshot` to
  every currently-online managed kiosk in parallel, joins each kiosk's
  reply with `out` counts computed by replaying the projected
  `transaction_lines` ledger (`ledger.ReplayOpenRows`), and returns rows
  whose `available ≤ reorder_threshold`. Offline kiosks surface under
  `errors` so the SPA can show "partial result — N kiosks excluded"
  instead of hiding the limitation. Honors `?kiosk_code=` so the
  page-level kiosk filter scopes the fan-out to a single target.

What it **doesn't** do in v1 (deliberately out of scope):

- Drift detection / state-hash compare between controller and kiosk.
- Controller-side per-kiosk `quantity_on_hand` projection. The
  snapshot fan-out used for the low-stock report is live-per-request;
  a persistent projection (consuming `inventory.adjust` deltas plus
  `item.checkout` / `item.consume` qty deltas to maintain a
  `kiosk_inventory` rollup) is on the roadmap for when fan-out RTT
  becomes the bottleneck.
- Cross-fleet movement of serialized items.
- Tightening PB collection rules on managed kiosks (the projector uses
  the DAO, so rules don't matter; UI gating handles the admin
  experience).
- Other remote admin commands — only inventory adjust + snapshot,
  checkout close, and instance management (create / edit / set_status /
  snapshot) ship today. The dispatcher's `HandleFunc`
  registry makes adding a new command a one-handler change.

## Reports surface

The controller's Reports view exposes eight tabs. Five are backed by the
projected ledger — three read `transactions` + `transaction_lines` directly,
and "Currently out" / "Aging" read the projected `open_checkouts` table
(maintained by the aggregator from `item.{action}` and `checkout.admin_close`
events). The remaining three project from dedicated event subjects:

| Tab | Source | Notes |
|---|---|---|
| Currently out | `GET /api/kiosk/reports/open-checkouts` (controller impl reads from the projected `open_checkouts` table) | Honors `?kiosk_code=`; per-row "Close…" affordance forwards to the remote kiosk via the admin force-close endpoint |
| Aging | Same source as Currently out, bucketed by user with oldest-out-first sort | Per-user rollup; threshold is a display hint, not a filter; per-row "Close…" affordance same as above |
| Low stock | `GET /api/controller/reports/low-stock` (snapshot fan-out) | Offline kiosks listed under `errors` for partial-result transparency |
| Group activity | `transactions` + `transaction_lines` + `groups` via pb-sdk, rolled up client-side | Date range filter |
| Recent transactions | `transactions` via pb-sdk, paginated | Click to drill into a transaction |
| Adjustment audit | `inventory_audit` via pb-sdk, paginated | Filters: from / to / source (local vs controller); kiosk filter from the page header |
| Instance lifecycle | `instance_lifecycle_audit` via pb-sdk, paginated | Filters: from / to / action (create/to_maintenance/return_to_service/retire/unretire) / source; kiosk filter from the page header. Same tab works on standalone kiosks against their local `instance_audit` |
| Notifications | `notification_send_log` via pb-sdk, rolled up client-side | Per-event success-rate table, recent-failures panel; same tab works on standalone kiosks against their own send log |

Every tab has an **Export CSV** button that respects the same filters the
table is showing on screen. Server-side endpoints live under
`/api/kiosk/reports/<name>.csv` (or `/api/controller/reports/<name>.csv`
for the controller-only fleet variants) — see
[docs/api.md](api.md) for the full list and query params.

## NATS provisioning

The controller and kiosks both connect to a `nats-server` you run
separately. JetStream must be enabled (`nats-server -js`). Provision
the stream and KV buckets once, out of band:

```bash
nats stream add KIOSK_EVENTS --subjects 'kiosk.*.event.>' --retention limits --max-age 168h
nats kv add catalog_items --history 1
nats kv add catalog_users --history 1
nats kv add catalog_groups --history 1
```

(The controller will auto-create these on first start as well; the
manual form is here so operators have a record of what's provisioned.)

The stream binds to only the `event.` subject family. Commands
(`kiosk.*.command.>`) and heartbeats (`kiosk.*.heartbeat`) ride core
NATS — they're outside the stream's filter by construction, so the
controller→kiosk request/reply path never races against a JetStream
PubAck. Adding a future durable subject means putting it under
`event.` so it lands in the stream automatically; anything that needs
synchronous request/reply or fire-and-forget pub/sub belongs under
`command.` or its own non-event family.

The controller's durable consumer uses `AckExplicitPolicy` and acks
each event it processes — that's the cursor advancement that survives
restarts, independent of stream-level PubAck behavior.

Names above are the defaults. On a shared NATS cluster where `kiosk.>`
or `KIOSK_EVENTS` are already taken, override via `nats.subject_prefix`
and `nats.stream_name` (and the `controller.catalog_*_bucket` keys for
the KV buckets). The kiosk and controller must agree on the subject
prefix; the stream name is consumed only by the controller. Substitute
your overrides into the provisioning commands above.

**Run exactly one controller per stream.** The durable JetStream
consumer is named `controller-aggregator` (a constant in
`internal/controller/consumer.go`), so two `kiosk-controller` processes
pointed at the same `nats.stream_name` will fight for the same
consumer cursor and projection writes will be unpredictable.
JetStream's durable-consumer model is single-owner by design; this
isn't a place we want HA via duplication. If you need redundancy, run
the controller under a supervisor that restarts on crash — durable
means restarts resume from the last-acked sequence with no event loss.
To scale horizontally across separate fleets, give each its own
`nats.stream_name` (and matching `nats.subject_prefix`) so the
controllers don't overlap.

## Controller setup

```bash
# Build
go build -o kiosk-controller ./cmd/controller

# Config
cp controller.yaml.example controller.yaml      # set nats.url + auth

# First run: applies migrations, prints bootstrap admin creds once.
./kiosk-controller

# Or, for a one-shot CSV seed before serving:
./kiosk-controller seed-catalog --items=items.csv --users=users.csv --groups=groups.csv
```

The controller binary uses the **same** `migrations/` package as the
kiosk plus the controller-only sibling package
`migrations/controller/`, which holds nine additional migrations
(kiosks registry, kiosk_items membership, kiosks.last_transaction_at,
inventory_audit, instance_lifecycle_audit, the
open_checkouts.kiosk_code/source_item_instance_id columns,
source_item_instance_id on transaction_lines, and the create-then-drop
of the `applied_oc_closes` idempotency guard — which the now
replay-based open-checkouts view no longer needs). Each one
self-registers via `init()`; `cmd/controller/main.go` blank-imports
both packages. The kiosk binary doesn't blank-import
`migrations/controller`, so its DB never sees the controller-only
collections. The controller's data dir is `pb_data_controller/` so a
kiosk and controller can co-exist in one working directory during
development without colliding.

The controller's PocketBase admin UI lives at the same paths as a
kiosk's: `/_/` for the PB superuser, `/admin/login` for the kiosk
admin. The Kiosks list view (`/admin/kiosks`) shows the fleet with
online badges; clicking a row opens the per-kiosk detail view at
`/admin/kiosks/<code>`, which has four tabs:

- **Overview** — editable location, status, and notes; the live online
  indicator and last-transaction timestamp.
- **Items** — the "Stocked items" membership panel (lifted from the
  old dialog).
- **Inventory** — a live snapshot from the kiosk via the
  `inventory.snapshot` NATS command, showing on-hand / out / available /
  reorder threshold and a low-stock highlight — the same fidelity as the
  local kiosk's Items view. On-hand and threshold come from the kiosk;
  "out" is derived controller-side from the projected ledger
  (`ReplayOpenRows`), and available + low-stock are computed in the SPA
  from those via the shared `ui/src/lib/inventory.ts` helper. A per-row
  Adjust button (quantity-tracked items only) drives the corresponding
  `inventory.adjust` command. Serialized rows show a "serialized" marker
  instead — their count is derived and managed on the Instances tab.
- **Instances** — the serialized-unit roster fetched via
  `instance.snapshot`, with a per-unit status (in_service / maintenance /
  retired) and an out-status ("currently out" / "available") column
  derived from the same ledger replay. Create and edit (cosmetic) map to
  matching NATS commands; every lifecycle transition (send to maintenance,
  return to service, retire, un-retire) goes through the single
  `instance.set_status` command, which requires a reason that lands in
  the audit log. The endpoint is
  `POST /api/controller/kiosks/{code}/instances/{instance_code}/status`
  with body `{status, reason}` (status validated ∈ {in_service,
  maintenance, retired}).

Use the Items view to add/edit items globally; user edits fan out to
every managed kiosk. Item edits fan out only to the kiosks that stock
them — open a kiosk's detail page and use the Items tab (or "Bulk add
by category") to assign SKUs first. The Items view shows a "Stocked
at" chip list inside each item's edit dialog so you can see the
inverse projection at a glance.

## Assigning items to kiosks

Once you have items and kiosks on the controller, decide which SKUs
each kiosk stocks. There are two paths in the admin UI:

1. **New kiosk button** on AdminKiosksView pre-registers a kiosk
   record by `kiosk_code` + `location_code` before the kiosk itself
   has phoned home. This unblocks the next step on day-one
   deployments. After creation, the SPA navigates straight to the new
   kiosk's detail page.
2. **Items tab** on the kiosk detail page (`/admin/kiosks/<code>`).
   From there:
   - **Add item** — search the global catalog and click an item to add it.
   - **Bulk add by category** — pick a category, preview the matching
     SKUs, confirm. The result is just rows in `kiosk_items` — there
     is no stored "category rule," so items added to the catalog
     later will not auto-flow to this kiosk. Click the button again
     when you want to top up.
   - **Remove** — drops the membership row. The kiosk receives a KV
     delete on its key and soft-deactivates the item locally; its
     `item_instances` and any transaction history stay intact.

Inversely, the **Stocked at** chip list on each item's edit dialog
(controller mode only) shows which kiosks currently carry that SKU.
Read-only — the source of truth is per-kiosk membership.

## CSV seed format

`seed-catalog` reads up to three optional CSV files (`--items`,
`--users`, `--groups`) and shares its row-validation logic with the
HTTP importer at `POST /api/kiosk/<kind>/import`. The column shapes,
auto-group-create behavior, and per-row error reporting are identical;
the CLI just emits a one-line summary per kind plus error log lines for
bad rows, where the HTTP path returns a JSON response with per-row
outcomes. Day-to-day catalog edits are easier through the SPA's
**Admin → Import** view (which the controller now also exposes) — the
CLI is for one-shot bulk seeds before the SPA is reachable, or for
scripted re-imports.

See [Schema → CSV import format](schema.md#csv-import-format) for the
full column schemas. Groups are seeded first so user rows referencing
a group code land on the just-imported metadata rather than
auto-creating a minimal row.

On shared sites with multiple trades, set each worker's group so a
foreman can return tools across users **only within their own
group**; an ungrouped foreman can't act for anyone. See
[Returns policy](configuration.md#returns-policy) for the full rules.

`--no-publish` skips the KV fan-out (useful for first-time seeding
before the broker is reachable; the next normal startup re-emits
whatever needs emitting when an admin edits a record).

## Opting a kiosk in

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
   (`Watch("<my_kiosk_code>.>")`) and the shared `catalog_users`
   bucket, then project the snapshot into local `items` and `users`
   (matching by `code`). Items absent from membership simply never
   arrive.
2. Publish its own transaction events to NATS as before — the
   controller's consumer picks them up automatically and the kiosk
   shows up in the controller's `kiosks` registry (if it wasn't
   pre-registered already).
3. Hide catalog edit affordances in the admin SPA; the banner
   "Catalog managed by controller" appears on every admin page.

**Upgrade note.** Earlier builds used flat keys (`<item_code>`) in
`catalog_items` and broadcast every SKU to every kiosk.
Membership-driven publishing changes the key shape and meaning, so on
upgrade: wipe each kiosk's `pb_data/` and the controller's
`catalog_items` KV bucket (`nats kv rm catalog_items`, then let the
controller re-create it), then assign items to kiosks via the new
"Stocked items" panel. The app isn't in production yet so no compat
shim is provided.

If `controller.enabled=true` but NATS is unreachable, the kiosk still
boots and serves checkouts against whatever catalog state it has —
the watcher logs a warning and proceeds without sync until the broker
comes back. The local ledger is always authoritative.

## Reconciling catalog drift

The controller's PB record hooks `Put` to the catalog KV buckets after
each save. If the broker was briefly unreachable during a save, the DB
row lands but the KV `Put` fails silently (logged at warn level). Over
time this can leave KV slightly out of sync with the controller's DB.
Symmetric scenarios produce the same problem: a controller restored
from an older backup is "out of sync" against newer KV state; a fresh
controller pointed at a pre-existing bucket inherits whatever was
there.

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
controller blowing away their edit on next restart. After a rollback
or bucket-adoption, the runbook entry is: hit `reconcile` with
`delete_orphans: true`.
