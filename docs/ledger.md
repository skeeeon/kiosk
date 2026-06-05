# Ledger

This doc describes how the kiosk records state changes, why it's modeled
as a ledger rather than as direct mutation of inventory rows, and how the
same model extends to the controller's fleet-wide view. It assumes
familiarity with [Schema](schema.md) and the architecture overview in
[CLAUDE.md](../CLAUDE.md).

## What "the ledger" is

The ledger is two collections plus one derived view:

| Collection | Role |
|---|---|
| `transactions` | One row per committed cart. Header-level facts: who, where, when, status. Append-only after commit. |
| `transaction_lines` | One row per item action inside a transaction. Action is `checkout`, `return`, `consume`, or `admin_close`. Carries `item_instance` for serialized lines, `original_checkout_user` for cross-user returns and every admin close. Append-only after commit. |
| `open_checkouts` | "What is out right now." One row per unit currently checked out. Maintained by the commit hook; rebuildable from the two collections above. |

The first two are the ledger proper: an immutable sequence of facts. The
third is a cache of "current state" derived from those facts —
materialized so the kiosk UI can answer "what's out?" in a single SELECT
instead of replaying history per page load.

Two collections in the same database act as supporting audit logs and
follow the same append-only discipline: `stock_adjustments` (every
admin adjustment to a quantity-tracked item's `quantity_on_hand`) and
`instance_audit` (every lifecycle change on a serialized unit — which
is also what moves a serialized SKU's derived `quantity_on_hand`). They
aren't part of the ledger's checkout state machine, but they share the
design property that every mutation leaves a permanent row behind.

## The single write path

Every state-changing action a worker can take — checkout, return,
consume, or any combination in one cart — flows through one function:
[`commit.Commit`](../internal/commit/commit.go). Admin force-closes use
a sibling entry point, [`commit.AdminClose`](../internal/commit/admin_close.go),
which writes the same shape of rows.

The structure is:

1. Open a single PocketBase database transaction (`app.RunInTransaction`).
2. Write one `transactions` row.
3. For each cart line:
   - Validate the action is legal for the item type (`consume` only on
     consumables; serialized items must have qty=1 and an instance FK; etc.).
   - Write one `transaction_lines` row.
   - Apply the per-action side effect on `open_checkouts`:
     - `checkout` → insert N rows.
     - `return` → delete up to N rows (mark line `uncorrelated=true` if
       fewer matched). For serialized items the row is uniquely
       identified by `item_instance`; for non-serialized, only rows
       belonging to the `original_checkout_user` (or the cart user if
       unset) are eligible — the resolver never silently borrows from
       another user's open rows.
     - `consume` → decrement `items.quantity_on_hand`. No
       `open_checkouts` write.
4. Commit the database transaction.
5. **After** the database commit succeeds, publish one
   `transaction.complete` event plus one `item.{action}` event per line
   via `events.Publish` (slog always, NATS when configured).

The structure encodes three invariants:

- **`transactions` and `transaction_lines` are append-only.** PocketBase
  collection rules forbid writes via the REST API; the commit code path
  is the only writer. There is no "edit a transaction" or "delete a
  line" affordance anywhere in the SPA, by design.
- **`open_checkouts` is a function of the ledger.** A row in
  `open_checkouts` always has a backing `transaction_line` FK; deleting
  the table and replaying the ledger yields the same content. This is
  enforced by giving `commit.Commit` exclusive write access to the
  table — `open_checkouts` collection rules are similarly closed.
- **Events fire only after the database commit succeeds.** If the
  database transaction rolls back, no event is published. There is no
  "queued event that didn't really happen" failure mode.

A consequence worth calling out: a `checkout` of `qty=N` for a
non-serialized tool creates **N rows** in `open_checkouts`, one per
physical unit. A serialized checkout creates exactly one row and that
row is uniquely keyed by the `item_instance` FK. This is what lets a
return of "DR-IMPACT-042 unit A" close out *that exact unit* without
disturbing units B and C of the same SKU.

A second consequence: consumables and consumable returns never touch
`open_checkouts`. The state of a consumable is entirely captured by
`items.quantity_on_hand`; the ledger holds the audit trail of the
decrements.

## What the design buys

The ledger model is not the only way to track a kiosk's state. A simpler
alternative would be to mutate `items.quantity_on_hand` directly on each
checkout/return and skip the line-by-line audit table entirely. The
ledger trades a small amount of write-time work for several properties
the simpler design can't provide:

- **Reconstruction.** `open_checkouts` can be rebuilt from
  `transaction_lines` at any time. The integrity tooling
  (`GET /api/kiosk/integrity` and
  `POST /api/kiosk/integrity/rebuild`) exists precisely because the
  derived view is treated as untrusted relative to the ledger. If the
  hook is buggy, if a manual DB edit happens, if a power loss interrupts
  a write — replay produces the right answer. See
  [`expectedOpenCheckouts`](../internal/handlers/integrity.go) for the
  replay rules.
- **Audit.** Every action carries who (user FK), where (kiosk_code +
  location_code stamped from process-global identity), when (timestamps
  on both `started_at` and `completed_at`), and why (action + reason
  fields for admin closes). Deletes are not part of the API surface; a
  retraction is a new row, not a removal of an old one.
- **No "double-spend" race.** Because checkout/return and the
  corresponding `open_checkouts` insert/delete happen in one DB
  transaction, there's no window where the ledger says "out" and the
  view says "not out" or vice versa. Crashes between steps cannot leave
  a partially-applied state — SQLite either commits the lot or rolls
  the lot back.
- **Federation-ready by construction.** Every transaction is stamped
  with `kiosk_code` and `location_code` at write time, from the
  process-global identity set at startup ([`internal/kioskctx`](../internal/kioskctx)).
  Clients don't supply these fields. Two kiosks at different sites can
  both write transactions for the same SKU without any coordination,
  and a downstream consumer can disambiguate by inspection.
- **Event publishing is a fan-out of ledger writes, not a separate
  protocol.** The `event.transaction.complete` and
  `event.item.{action}` subjects carry the same facts the ledger rows
  do, in the same shape. There is no second commit hook for "tell the
  outside world" — the publish step reads from the freshly-written
  records.

What the design costs:

- **Two tables to read instead of one.** Reporting code joins
  `transactions` and `transaction_lines` (and often `open_checkouts`)
  rather than pivoting on a single row. List views pre-paginate to keep
  this from becoming a problem; `transactions.lines_count` is
  denormalized at commit time so list pages don't need an N+1
  `COUNT(*)`.
- **Storage grows monotonically.** A kiosk that processes 200
  transactions a day at 3 lines each accumulates ~220k rows per year on
  the lines table. This is well under SQLite's comfort zone and not a
  near-term concern, but a 10-year-old kiosk will have a non-trivial
  database file. There is no retention policy in v1 — the ledger is
  intentionally kept whole.
- **Hot-path validation is in commit, not in the schema.** Rules like
  "serialized line must carry an instance" or "consume only valid for
  consumables" live in Go, not in collection rules. The trade-off is
  that the same code path runs both interactive commits and ledger
  replays — and replays don't re-validate.

## `open_checkouts` as a derived view

The materialized-view treatment of `open_checkouts` deserves a closer
look because it's where the "ledger is source of truth" property is
operationalized.

Replay rules (from
[`expectedOpenCheckouts`](../internal/handlers/integrity.go)):

- For each completed transaction, walk lines in order.
- `checkout`: increment the bucket keyed by `(item, item_instance, user)` by `qty`.
- `return`: decrement the bucket keyed by `(item, item_instance, original_checkout_user OR transaction.user)` by `qty`.
- `admin_close`: decrement, identical to `return` — the line always carries
  `original_checkout_user` (the holder whose row was force-closed). Both the
  integrity *check* ([`expectedOpenCheckouts`](../internal/handlers/integrity.go))
  and the *rebuild* ([`ledger.ReplayOpenRows`](../internal/ledger/replay.go))
  subtract on it, so an admin force-close shows no false drift and the rebuild
  doesn't resurrect the closed row.
- `consume`: ignored (consumables don't open rows).

Returns (and admin closes) target the holder's rows **only** — there is no
borrowing from another user when a return over-subtracts. A shortfall leaves
other users' rows intact and the line is stamped `uncorrelated`. The replay
mirrors `commit.candidateOpenRows` exactly on this point; an earlier version
of the rebuild/projection borrowed FIFO from any user, which silently diverged
from commit.

Diff against the current `open_checkouts` table:

- `missing_in_table` — replay says a row should exist; actual table has
  none. Hook bug or interrupted write.
- `extra_in_table` — actual table has a row replay doesn't expect.
  Manual DB edit or stale row.

Healthy result: both arrays empty, `expected_open == actual_open`.

Rebuild (`POST /api/kiosk/integrity/rebuild`) wipes the table inside one
DB transaction and re-inserts from replay. Each rebuilt row carries the
source line's `completed_at` as `checked_out_at` and an FK back to the
originating `transaction_line`, so aging and audit reports remain
meaningful after a rebuild.

## Events: ledger writes, broadcast

Every ledger write produces events on a NATS subject. Subjects follow
`<prefix>.<kiosk_code>.<family>.<...>`; the ledger contributes the
`event.` family:

- `event.transaction.complete` — one per commit.
- `event.item.{action}` — one per line. `{action}` is `checkout`,
  `return`, `consume`, or `admin_close`.

Two additional event subjects ride alongside but originate outside the
commit path:

- `event.inventory.adjust` — from admin stock adjustments
  ([`stock_adjust.go`](../internal/handlers/stock_adjust.go)) and as
  the qty side-effect of `lost`/`damaged` admin closes of
  **quantity-tracked** items. Serialized closes don't emit it — the
  unit's removal rides `event.instance.lifecycle` instead, and the
  derived quantity follows the active-instance count.
- `event.instance.lifecycle` — from `item_instances` PB record hooks
  and from `commit.AdminClose` when retiring a serialized unit.

The bind for the controller's JetStream stream is
`kiosk.*.event.>` — commands (`command.`) and heartbeats (`heartbeat`)
ride core NATS and stay outside by construction rather than by
filter-list discipline. Adding a new event subject means
(1) adding a builder + filter helper in
[`internal/events/subjects.go`](../internal/events/subjects.go) and
(2) extending the consumer's `FilterSubjects` and dispatch switch. The
stream picks the new subject up automatically.

When NATS is disabled or unreachable, `events.Publish` still emits a
slog line (`kiosk.event`). The ledger and the slog stream both remain
authoritative; the NATS broadcast is best-effort transport for
downstream consumers.

## The controller's projected ledger

The controller binary embeds the same migrations as the kiosk plus a
small set of controller-only collections. Its `transactions` and
`transaction_lines` collections gain two extra fields each, used as
idempotency anchors:

| Field | On | Purpose |
|---|---|---|
| `source_kiosk_code` + `source_transaction_id` | `transactions` (unique pair index) | One row per (originating kiosk, originating transaction). Redelivery is a no-op. |
| `source_line_id` | `transaction_lines` (unique-when-non-empty) | Same idea, line scope. |

A JetStream durable consumer named `controller-aggregator`
([`internal/controller/consumer.go`](../internal/controller/consumer.go))
binds to the event subject family and projects:

- `event.transaction.complete` → one `transactions` row.
- `event.item.{action}` → one `transaction_lines` row. The controller
  answers "currently-out" by replaying these lines on demand
  (`ledger.ReplayOpenRows`) rather than materializing an `open_checkouts`
  table — convergent by construction, so it cannot drift from a kiosk's.
- `event.inventory.adjust` → one `inventory_audit` row (idempotent
  via `source_adjustment_id`).
- `event.instance.lifecycle` → one `instance_lifecycle_audit` row
  (idempotent via `source_audit_id`).
- `event.checkout.admin_close` → one `transactions` row + one `admin_close`
  `transaction_lines` row (`ProjectAdminCloseToLedger`, mirroring
  `commit.AdminClose`'s local writes). An admin close publishes **only**
  this subject — never `transaction.complete` or `item.*` — so this event
  is the controller's sole view of it; projecting it as a line is what
  makes the replay drop the holder's row.
- `event.integrity.rebuild` — acknowledged and logged today (audit hook
  for a future ops view); there's nothing to rebuild now that the
  controller replays the ledger instead of materializing open_checkouts.

The projection is purely additive. The controller never writes back to
the kiosk's ledger; the data flow is one-directional kiosk → controller
for the ledger surface.

The kiosk's `users` and `items` flow in the opposite direction (controller
→ kiosk over JetStream KV), but those are catalog records, not ledger
records. The split is deliberate: catalog is managed centrally and
distributed; ledger is generated at the edge and aggregated centrally.

## Idempotency

Every write the controller does on behalf of an event is idempotent
because the storage layer enforces uniqueness on a stable key:

| Surface | Key | Effect on duplicate |
|---|---|---|
| `transactions` projection | unique(`source_kiosk_code`, `source_transaction_id`) | Lookup-first, insert-on-miss; unique-violation on race is caught and treated as already-projected. |
| `transaction_lines` projection | unique-when-non-empty `source_line_id` | Same pattern. |
| `inventory_audit` | unique-when-non-empty `source_adjustment_id` | Same. |
| `instance_lifecycle_audit` | unique-when-non-empty `source_audit_id` | Same. |
| `checkout.admin_close` projection | unique(`source_kiosk_code`, `source_transaction_id`) + `source_line_id` | Projected as a `transactions` + `admin_close` `transaction_lines` pair (same anchors as the rows above), so a redelivery no-ops. The controller no longer materializes `open_checkouts`, so there are no guarded deletes — the replay drops the holder's row from the projected line. |
| Remote `inventory.adjust` command | unique-when-non-empty `command_id` on `stock_adjustments` | Kiosk-side: a retried command finds the prior row and replies with the prior result instead of double-applying. |
| Remote `checkout.close` command | unique-when-non-empty `command_id` on `transactions` | Same pattern, transaction-scoped. |
| Remote `instance.{create,set_status}` | unique-when-non-empty `command_id` on `instance_audit` | Same. |

JetStream's at-least-once delivery means the consumer can see the same
event twice. The unique indexes turn that into a no-op rather than a
double-write.

## Drift recovery

The controller's projected ledger is a copy maintained over an unreliable
transport (NATS, with `MaxAge=7d` retention in v1). Three things can
cause it to drift from a kiosk's local ledger:

1. NATS was unreachable when an event was published. The publish is
   queued in the client's in-memory buffer; if the buffer overflows or
   the kiosk process restarts before reconnect, the event is lost from
   the controller's view. The kiosk's local ledger still has the
   transaction.
2. The controller process was down past the JetStream retention window
   (default 7 days). Events older than `MaxAge` drop off the stream and
   re-running the consumer can't replay them.
3. A bug or manual edit on either side. (Manual edits are blocked by
   PB collection rules in the normal API surface, but a PB superuser
   can still touch rows via `/_/`.)

Two operator tools close this gap:

- `POST /api/kiosk/ledger/republish` — re-emits, per completed
  transaction in an optional `{from, to}` ISO8601 window, the same events
  the live commit path sent: `transaction.complete` + `item.{action}` for
  ordinary transactions, and a single `checkout.admin_close` for admin
  force-close transactions (matching `commit.AdminClose`'s live shape).
  Payloads are rebuilt from persisted rows with original timestamps and the
  `kiosk_code`/`location_code` stamped at original commit time. Safe to
  re-run because the controller's projection is idempotent.
- `POST /api/controller/kiosks/{code}/ledger/republish` — same thing,
  driven over NATS request/reply from the controller (the operator
  doesn't need shell access to the kiosk).

For the inverse — the kiosk's `open_checkouts` has drifted from its
own ledger — use the integrity rebuild path described above.

There is no automatic drift detection in v1. Compare aggregate counts
(`transactions.count` per `kiosk_code` on the controller vs. count on
the kiosk) by hand if drift is suspected.

## Standalone vs. managed: the same ledger model

A standalone kiosk has the full ledger (transactions, transaction_lines,
open_checkouts) and writes through the same `commit.Commit` path. Events
fire and are emitted to slog; if NATS isn't configured, they stop there.
Reports read from the local ledger.

A managed kiosk has the same ledger, the same commit path, the same
events. NATS is configured and the events also reach the controller's
durable consumer, which projects them. Catalog records flow in from the
controller's KV buckets, but the kiosk's local ledger remains
authoritative for what happened at that kiosk. If the controller is
unreachable, the kiosk continues to operate normally — checkouts work,
events buffer, and the controller's projection catches up on
reconnect (or on republish if it can't).

The controller is not a database upgrade for the kiosk. It is a
projection target that happens to share the same row shapes for the
ledger surface, plus its own audit collections for the non-ledger event
families. Deleting the controller's `pb_data_controller/` and rebuilding
it from a `ledger.republish` run across every kiosk reproduces the
transaction / line surface — and therefore the open-checkouts view the
controller derives from it by replay — modulo events older than JetStream
retention, which the next manual export pass would need to cover.

Admin force-closes are reproduced faithfully: republish detects an
admin-close transaction (its single `admin_close` line) and re-emits the
same `checkout.admin_close` event the live path sends — *not* the
`transaction.complete` + `item.{action}` pair the regular walk uses. The
controller projects that event into the same ledger rows the kiosk holds (a
completed transaction + one `admin_close` line), so a replay drops the
holder's open row. Both projections are idempotent on
`source_transaction_id` / `source_line_id`, so re-running a republish stays a
no-op.

## Pointers

- Write path: [`internal/commit/commit.go`](../internal/commit/commit.go)
- Admin force-close: [`internal/commit/admin_close.go`](../internal/commit/admin_close.go)
- Integrity / replay: [`internal/handlers/integrity.go`](../internal/handlers/integrity.go)
- Republish: [`internal/handlers/ledger_republish.go`](../internal/handlers/ledger_republish.go)
- Controller aggregator: [`internal/controller/consumer.go`](../internal/controller/consumer.go)
- Event subjects: [`internal/events/subjects.go`](../internal/events/subjects.go)
- Collection shapes: [`migrations/1779000000_init.go`](../migrations/1779000000_init.go) and siblings
