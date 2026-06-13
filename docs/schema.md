# Schema

Collections are defined as code across `migrations/*.go`. The initial
migration creates the first six; subsequent migrations add the
per-instance and audit-log collections, notifications collections, and
the controller-only `kiosks` registry and `kiosk_items` membership table
(plus a few fields on `items`).

| Collection | Purpose |
|---|---|
| `users` | Workers (badge-holders). PB default auth collection, real emails kept for future notifications. Workers don't log in in v1. `group` is an optional FK to `groups`. |
| `admins` | Foremen / admins. Separate PB auth collection. Login via email + password. |
| `groups` | Sub-contractors / trades. `code` is the stable join key (CSV import, cross-fleet sync); metadata fields (`name`, `contact_email`, `contact_phone`, `notes`) are admin-managed and downstream features like email receipts use them. Optional on workers; deletion sets affected `users.group` to null. |
| `items` | Tools and consumables (the SKU). `tracking_mode` is `quantity` or `serialized`. Carries `quantity_on_hand` and `reorder_threshold` (low-stock alert level; 0 disables the alert). `quantity_on_hand` is a stored, admin-adjustable value for `quantity` tools (fleet count) and consumables (current stock); for `serialized` SKUs it is a **derived** materialized view of the **non-retired** `item_instances` count (in_service + maintenance; retired excluded) — kept in sync by the instances recompute hook and not directly editable. Serialized SKUs also carry `requires_maintenance_on_return` (bool): when set, a returned unit is routed into maintenance automatically. |
| `item_instances` | One physical unit of a serialized SKU. Holds the scannable `code`, the printed `serial`, an optional `rfid_epc`, and `status` — the lifecycle enum `in_service` \| `maintenance` \| `retired`. `in_service` is checkout-eligible; `maintenance` is physically present but parked (counts toward on-hand, not available); `retired` absorbs the old "decommissioned" state and the removed hard delete (units are never deleted, since the ledger keeps their FKs alive). "Checked out / out" is **not** a status — it stays derived from `open_checkouts`. FK to the parent `items` row. |
| `instance_audit` | Append-only audit log of `item_instances` lifecycle changes. Written by PB record hooks (REST path) and `instances.SetStatusInTx` (commit / command-bus path): one row per `create` / `to_maintenance` / `return_to_service` / `retire` / `unretire`, carrying `prev_status` + `new_status`. The action verb is derived from the (prev → target) transition. Cosmetic edits (code/serial/notes) intentionally don't audit. Mirrors `stock_adjustments` in role; `commit.AdminClose` also writes a `retire` row when `closure_reason ∈ {lost, damaged}` retires a serialized instance. |
| `transactions` | Append-only ledger. `kiosk_code`, `location_code`, `user`, `started_at`, `completed_at`, `status`. Also carries `closed_by_admin` (FK, populated when the row was created by an admin force-close) and `command_id` (unique-when-non-empty, idempotency anchor for controller-forwarded closes). |
| `transaction_lines` | One per item action within a transaction. `action` is `checkout`, `return`, `consume`, or `admin_close`. Carries optional `item_instance` FK for serialized lines. `original_checkout_user` (the worker whose open_checkout was closed) is set on `return` rows produced by the foreman-return endpoint and on every `admin_close` row. `admin_close` rows additionally carry `closure_reason` (`lost` \| `returned_offline` \| `damaged` \| `other`), `closed_by_admin` (FK for source=local), and `notes`. |
| `open_checkouts` | Materialized view of "what's out right now." One row per unit out. Carries `item_instance` FK for serialized units. Maintained by the commit hook **on the kiosk**. The controller does not maintain this table — it derives "currently out" on demand by replaying `transaction_lines` (`ledger.ReplayOpenRows`). |
| `time_punches` | Append-only timeclock ledger — one row per clock-in/clock-out punch. **API-readonly** (admin list/view; create/update/delete forbidden); written ONLY by the in-process punch funnel `timeclock.PerformPunch`. Fields: `user` (FK), `user_code`, `direction` (`in`\|`out`), `occurred_at` (business time; admin punches may backdate, live punches stamped server-side), `source` (`self`\|`foreman`\|`admin`\|`controller_admin`), exactly one actor field per non-self source (`recorded_by_admin` FK / `recorded_by_user` FK / `controller_admin_id` text), `reason`, `force` (bool), `kiosk_code`, `location_code`, `command_id` (idempotency anchor, unique-when-non-empty), `created` (autodate). "Is this user clocked in" is **derived** — the latest punch by `occurred_at` (`created` breaks ties), merged with the fleet `punch_state` replica; there is deliberately no materialized open-shifts table. Shared kiosk migration, so the controller's DB has it too (the aggregator projects fleet punches into it). |
| `stock_adjustments` | Append-only audit log of changes to a **quantity-tracked** item's `quantity_on_hand` made via `/api/kiosk/items/{id}/adjust` (local) or the controller's `inventory.adjust` command bus (remote). Serialized items are rejected by both paths (their quantity is derived). Stores `delta`, `new_quantity` (snapshot), `reason`, the responsible `admin` (FK, populated for `source=local`), `source` ('local' \| 'controller'), `controller_admin_id` (text — controller's admin id, populated for `source=controller`), and `command_id` (UUID, unique-when-non-empty for idempotent replay of remote commands). |
| `notification_templates` | One row per event type (`receipt.transaction`, `alert.lowstock`, `alert.maintenance`, `digest.open_checkouts`, `digest.daily_activity`, `digest.maintenance`). Admin-editable subject/body Go templates plus a `recipients` JSON column (`{worker_email, all_admins, extras}`). Seeded from compiled-in defaults; rows are append-only — editable, not deletable. Both binaries get the schema; on managed kiosks the local rows are dormant (sends fire from the controller's rows instead). |
| `notification_send_log` | One row per attempted recipient. `status` is `sent` / `failed` / `skipped`. Pruned daily at 90 days. The controller's table holds the fleet-wide audit in managed mode; standalone kiosks log to their own. |
| `notification_dedupe` | Race-free dedup gate for `SendIfFirst`. Unique on `(event_type, ref, day)`. Receipts use `ref = transaction_id`; low-stock uses `ref = item_id`. Scheduled digests intentionally skip this — repeating cadence is the feature. |
| `scheduled_reports` | Admin-edited cron rows (`cadence` daily/weekly/monthly + hour + weekday/day_of_month). Per-row recipients override the template's stored recipients. In standalone deployments the kiosk runs the scheduler against its local data; in managed deployments the controller owns the cron + computation + SMTP and the kiosk's scheduler stays off entirely. The optional `kiosk_code` column scopes a controller row to one kiosk (empty = fleet-wide). |
| `kiosks` | **Controller-only.** Registry of every kiosk in the fleet. A row appears either when an admin pre-registers the kiosk via the "New kiosk" button on AdminKiosksView, auto-populated with `status=unknown` the first time the aggregator sees a `transaction.complete` from a new `kiosk_code`, or auto-populated on the first heartbeat. `last_transaction_at` advances on `transaction.complete` only (its name finally matches its meaning now that heartbeat owns general liveness); the legacy `last_seen` field was dropped after its one-release deprecation window. Used for fleet visibility and as the join target when expanding aggregated transactions to "which kiosk did this come from?" |
| `kiosk_items` | **Controller-only.** Membership rows tying items to kiosks. One row = one (kiosk, item) pair = "this kiosk stocks that SKU." Cascade-deletes from either side. Drives per-kiosk catalog publishing; absent rows mean the kiosk never receives that item over JetStream KV. |
| `inventory_audit` | **Controller-only.** Fleet-wide append-only projection of every `inventory.adjust` event the aggregator sees. One row per adjustment, denormalized (`kiosk_code`, `item_code`, `item_name`, `delta`, `prev_quantity`, `new_quantity`, `reason`, `source`, `admin_id`). `source_adjustment_id` carries the originating kiosk's `stock_adjustments.id` and is unique-when-non-empty so JetStream redelivery never duplicates a row. Drives the Reports → Adjustment audit tab on the controller. |
| `instance_lifecycle_audit` | **Controller-only.** Fleet-wide append-only projection of every `instance.lifecycle` event the aggregator sees. One row per lifecycle event, denormalized (`kiosk_code`, `item_code`, `item_name`, `instance_id`, `instance_code`, `action`, `prev_status`, `new_status`, `reason`, `source`, `admin_id`). `source_audit_id` carries the originating kiosk's `instance_audit.id` and is unique-when-non-empty so JetStream redelivery never duplicates a row. Drives the Reports → Instance lifecycle tab on the controller; the standalone kiosk's Reports tab reads its local `instance_audit` for the same view. |

## Controller-only fields

The controller's `transactions` and `transaction_lines` collections carry
extra fields not present on standalone kiosks:
`source_kiosk_code` + `source_transaction_id` on transactions (unique
pair index, idempotency key for redelivery) and `source_line_id` +
`source_item_instance_id` on transaction_lines (the former unique-when-non-empty
for idempotency; the latter the kiosk-local instance id, so the
open-checkouts replay can pair a serialized checkout with its return).
These — along with the `kiosks`, `kiosk_items`, `inventory_audit`, and
`instance_lifecycle_audit` collections, `kiosks.last_transaction_at`, and
the `open_checkouts.kiosk_code` + `source_item_instance_id` columns — are
added by the controller-only migrations living in the sibling package
`migrations/controller/`
(`2000000000_controller_collections.go`,
`2000100000_add_kiosk_items.go`,
`2000200000_kiosks_last_transaction_at.go`,
`2000300000_inventory_audit.go`,
`2000400000_instance_lifecycle_audit.go`,
`2000500000_open_checkouts_kiosk_code.go`,
`2000700000_tx_lines_source_instance.go`, and the
`2000600000`/`2000800000` create-then-drop of `applied_oc_closes` — the
idempotency guard for the old materialized open-checkouts projection, no
longer needed now that the controller replays the ledger). The controller's
`time_punches` additionally carries `source_punch_id` (the originating
kiosk's `time_punches.id`, unique-when-non-empty — the projection's
idempotency anchor) and `source_actor` (text, for kiosk-admin actors whose
FK can't resolve in the controller's DB), added by
`2001100000_time_punches_source.go`. Each self-registers via `init()`. The
kiosk binary doesn't import that package, so its DB never sees these.

## Virtual-terminal-only schema (`cmd/timeclock`)

The virtual timeclock terminal applies one extra migration that no other
binary imports — `migrations/timeclock/` (Go package `timeclockmigrations`,
same isolation pattern as `migrations/controller/`). It turns the `users`
collection into a real worker-auth surface on that binary's DB only: sets
`AuthRule = "active = true"` and `OAuth2.Enabled = true` so active workers can
sign in (SSO and/or password). Row-level list/view/create/update rules stay
admins-only; this only enables authentication, not data access. Regular kiosks
and the controller never import this package, so worker login stays off there.

## Cardinality rules for `open_checkouts`

- A `checkout` line with `qty=N` for a non-serialized tool creates **N
  rows**.
- A `return` line with `qty=N` deletes up to **N rows** (line marked
  `uncorrelated=true` if fewer matched).
- A serialized line always has `qty=1` and carries an `item_instance`
  FK. At most one open row exists per instance at any time; returning
  targets that exact instance — sibling units of the same SKU are
  untouched.
- Consumables don't generate `open_checkouts` rows. Instead the commit
  hook decrements `items.quantity_on_hand` by `qty` inside the same
  transaction. The value is allowed to go negative — the ledger is
  authoritative, and a worker grabbing more than was recorded shouldn't
  be blocked at the kiosk.

## CSV import format

Three importers, same response shape (per-row outcomes — see
[API reference](api.md)). All available on **both binaries** (the
kiosk's Admin → Import view and the controller's same view; managed
kiosks see a read-only banner pointing to the controller). Each
endpoint has a matching `/template` route that streams a starter CSV
with header + example rows the importer round-trips cleanly.

Common rules:

- Empty cells are nulls. `active` accepts `true|false|1|0|yes|no|y|n`.
- Rows match existing records by `code` (upsert). Records not in the
  CSV are left alone — the importer never deletes.
- Per-row validation. A bad row records an error but doesn't abort the
  rest of the batch.
- Dry-run (`dry_run=true`) is read-only: one snapshot read, no writes,
  and the response classifies every row as `insert` or `update` based
  on the diff against current DB state.

### Items — `POST /api/kiosk/items/import`

```csv
code,name,type,unit,tracking_mode,category,active,notes,quantity_on_hand,reorder_threshold
DR-IMPACT-042,Impact Driver,tool,each,serialized,Power Tools,true,,,0
SCREW-DECK-3IN,Deck Screws 3in,consumable,box of 100,quantity,Fasteners,true,,25,5
```

`quantity_on_hand` and `reorder_threshold` are optional; if omitted,
existing rows keep their current values and new rows default to zero.
For **serialized** rows a `quantity_on_hand` value is ignored (the count
is derived from non-retired instances); `reorder_threshold` still applies.
Per-unit serials and RFID EPCs live on `item_instances`, not on the SKU
row — serialized SKUs created via CSV still need their instances added
through the admin UI's instances panel, which is what drives their
derived quantity.

### Workers — `POST /api/kiosk/users/import`

```csv
code,name,email,role,group,active
WORKER-1,Alice,alice@example.com,worker,electrical,true
FOREMAN-1,Sam,sam@example.com,foreman,electrical,true
```

`role` defaults to `worker`. `group` is the group's **code** (not id);
unknown codes are auto-created on real-run with `code=name=value` and
`active=true`, which admins enrich with contact metadata via the Groups
admin view. Dry-run does *not* auto-create groups — the side effect
kicks in only on actual import.

### Groups — `POST /api/kiosk/groups/import`

```csv
code,name,contact_email,contact_phone,notes,active
electrical,Electrical Crew,lead@example.com,+1-555-0100,Morning shift,true
```

Useful for back-filling contact metadata on groups that were
auto-created by a workers import.
