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
| `items` | Tools and consumables (the SKU). `tracking_mode` is `quantity` or `serialized`. Carries `quantity_on_hand` (fleet count for tools / current stock for consumables) and `reorder_threshold` (low-stock alert level; 0 disables the alert). |
| `item_instances` | One physical unit of a serialized SKU. Holds the scannable `code`, the printed `serial`, an optional `rfid_epc`, and `active`. FK to the parent `items` row. |
| `instance_audit` | Append-only audit log of `item_instances` lifecycle changes. Written by PB record hooks: one row per `create` / `decommission` (active true→false) / `reactivate` (active false→true) / `delete`. Cosmetic edits (code/serial/notes) intentionally don't audit. Mirrors `stock_adjustments` in role; `commit.AdminClose` also writes a row when `closure_reason ∈ {lost, damaged}` retires a serialized instance. |
| `transactions` | Append-only ledger. `kiosk_code`, `location_code`, `user`, `started_at`, `completed_at`, `status`. Also carries `closed_by_admin` (FK, populated when the row was created by an admin force-close) and `command_id` (unique-when-non-empty, idempotency anchor for controller-forwarded closes). |
| `transaction_lines` | One per item action within a transaction. `action` is `checkout`, `return`, `consume`, or `admin_close`. Carries optional `item_instance` FK for serialized lines. `admin_close` rows additionally carry `closure_reason` (`lost` \| `returned_offline` \| `damaged` \| `other`), `closed_by_admin` (FK for source=local), `notes`, and `original_checkout_user` (the worker whose row was closed). |
| `open_checkouts` | Materialized view of "what's out right now." One row per unit out. Carries `item_instance` FK for serialized units. Maintained by the commit hook. |
| `stock_adjustments` | Append-only audit log of changes to `items.quantity_on_hand` made via `/api/kiosk/items/{id}/adjust` (local) or the controller's `inventory.adjust` command bus (remote). Stores `delta`, `new_quantity` (snapshot), `reason`, the responsible `admin` (FK, populated for `source=local`), `source` ('local' \| 'controller'), `controller_admin_id` (text — controller's admin id, populated for `source=controller`), and `command_id` (UUID, unique-when-non-empty for idempotent replay of remote commands). |
| `notification_templates` | One row per event type (`receipt.transaction`, `alert.lowstock`, `digest.open_checkouts`). Admin-editable subject/body Go templates plus a `recipients` JSON column (`{worker_email, all_admins, extras}`). Seeded from compiled-in defaults; rows are append-only — editable, not deletable. Both binaries get the schema; on managed kiosks the local rows are dormant (sends fire from the controller's rows instead). |
| `notification_send_log` | One row per attempted recipient. `status` is `sent` / `failed` / `skipped`. Pruned daily at 90 days. The controller's table holds the fleet-wide audit in managed mode; standalone kiosks log to their own. |
| `notification_dedupe` | Race-free dedup gate for `SendIfFirst`. Unique on `(event_type, ref, day)`. Receipts use `ref = transaction_id`; low-stock uses `ref = item_id`. Scheduled digests intentionally skip this — repeating cadence is the feature. |
| `scheduled_reports` | Admin-edited cron rows (`cadence` daily/weekly/monthly + hour + weekday/day_of_month). Per-row recipients override the template's stored recipients. Runs only on kiosks (the scheduler reads `open_checkouts`); in managed mode the kiosk publishes a `digest.open_checkouts` envelope and the controller does the SMTP send. |
| `kiosks` | **Controller-only.** Registry of every kiosk in the fleet. A row appears either when an admin pre-registers the kiosk via the "New kiosk" button on AdminKiosksView, auto-populated with `status=unknown` the first time the aggregator sees a `transaction.complete` from a new `kiosk_code`, or auto-populated on the first heartbeat. `last_transaction_at` advances on `transaction.complete` only (its name finally matches its meaning now that heartbeat owns general liveness); `last_seen` writes alongside it for one release as a deprecation window. Used for fleet visibility and as the join target when expanding aggregated transactions to "which kiosk did this come from?" |
| `kiosk_items` | **Controller-only.** Membership rows tying items to kiosks. One row = one (kiosk, item) pair = "this kiosk stocks that SKU." Cascade-deletes from either side. Drives per-kiosk catalog publishing; absent rows mean the kiosk never receives that item over JetStream KV. |
| `inventory_audit` | **Controller-only.** Fleet-wide append-only projection of every `inventory.adjust` event the aggregator sees. One row per adjustment, denormalized (`kiosk_code`, `item_code`, `item_name`, `delta`, `prev_quantity`, `new_quantity`, `reason`, `source`, `admin_id`). `source_adjustment_id` carries the originating kiosk's `stock_adjustments.id` and is unique-when-non-empty so JetStream redelivery never duplicates a row. Drives the Reports → Adjustment audit tab on the controller. |
| `instance_lifecycle_audit` | **Controller-only.** Fleet-wide append-only projection of every `instance.lifecycle` event the aggregator sees. One row per lifecycle event, denormalized (`kiosk_code`, `item_code`, `item_name`, `instance_id`, `instance_code`, `action`, `prev_active`, `new_active`, `reason`, `source`, `admin_id`). `source_audit_id` carries the originating kiosk's `instance_audit.id` and is unique-when-non-empty so JetStream redelivery never duplicates a row. Drives the Reports → Instance lifecycle tab on the controller; the standalone kiosk's Reports tab reads its local `instance_audit` for the same view. |

## Controller-only fields

The controller's `transactions` and `transaction_lines` collections carry
two extra fields not present on standalone kiosks:
`source_kiosk_code` + `source_transaction_id` on transactions (unique
pair index, idempotency key for redelivery) and `source_line_id` on
transaction_lines (unique-when-non-empty index). These — along with the
`kiosks`, `kiosk_items`, `inventory_audit`, and
`instance_lifecycle_audit` collections plus
`kiosks.last_transaction_at` — are added by five controller-only
migrations (`2000000000_controller_collections.go`,
`2000100000_add_kiosk_items.go`,
`2000200000_kiosks_last_transaction_at.go`,
`2000300000_inventory_audit.go`, and
`2000400000_instance_lifecycle_audit.go`), all registered via a single
`sync.Once` body in `RegisterControllerMigrations`. The plain kiosk
binary never invokes it, so its DB never gets these.

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

`POST /api/kiosk/items/import` expects:

```csv
code,name,type,unit,tracking_mode,category,active,notes,quantity_on_hand,reorder_threshold
DR-IMPACT-042,Impact Driver,tool,each,serialized,Power Tools,true,,1,0
SCREW-DECK-3IN,Deck Screws 3in,consumable,box of 100,quantity,Fasteners,true,,25,5
```

Empty cells are nulls. `active` accepts `true|false|1|0|yes|no|y|n`. The
`quantity_on_hand` and `reorder_threshold` columns are optional; if
omitted, existing rows keep their current values and new rows default
to zero. Items are matched by `code` (upsert). Items not in the CSV are
left alone. Per-unit serials and RFID EPCs live on `item_instances`,
not on the SKU row — serialized SKUs created via CSV still need their
instances added through the admin UI's instances panel.
