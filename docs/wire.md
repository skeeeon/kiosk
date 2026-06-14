# Wire reference

Every NATS subject this codebase publishes or subscribes to, in one
place. If you're integrating an external system (access-control gate,
camera/occupancy monitor, third-party metrics consumer) with the
kiosk, this is the spec. If you're working on the kiosk itself, the
authoritative sources are [`internal/events/subjects.go`](../internal/events/subjects.go)
and the handlers under [`internal/commands/`](../internal/commands/);
this doc is the readable mirror.

## Subject namespace

```
<prefix>.<kiosk_code>.<family>.<...>
```

`<prefix>` defaults to `"kiosk"` and is configurable via
`nats.subject_prefix`. Both kiosk and controller must agree on it.
`<kiosk_code>` is the per-kiosk identifier stamped on every
transaction.

`<family>` segment is what determines transport:

| Family | Direction | Transport | Persistence |
|---|---|---|---|
| `event.<...>` | kiosk → world | JetStream-bound | Stream (`KIOSK_EVENTS`, 7 d default retention) |
| `command.<name>` | controller / external → kiosk | Core NATS request/reply | None (synchronous) |
| `heartbeat` | kiosk → world | Core NATS pub/sub | Last-write-wins, no history |

The stream binds to `<prefix>.*.event.>` and nothing else, so commands
and heartbeats are outside its filter space **by construction** rather
than by exclusion-list discipline.

## Reply envelope

Every command reply is the same JSON shape:

```json
{
  "success": true,
  "error": "",
  "data": { /* command-specific */ }
}
```

- `success` is the only field guaranteed to be present.
- `error` is set when `success` is false; absent on success.
- `data` carries the command-specific result. Shape per command below.
- Reply timeout is **5 s**. Silence is treated as "kiosk offline" by
  the caller. Handlers MUST reply within this window even on
  validation error — see each command's error semantics below.

## Commands

All commands ride core NATS request/reply at
`<prefix>.<kiosk_code>.command.<name>`. The kiosk dispatcher
([`internal/commands/dispatcher.go`](../internal/commands/dispatcher.go))
routes on the `<name>` suffix.

### `inventory.adjust`

Mutate `items.quantity_on_hand` and write a `stock_adjustments` audit
row. Same business logic as the local `POST /api/kiosk/items/{id}/adjust`
endpoint — including rejecting **serialized** items (their quantity is
derived from the non-retired instance count), which replies `success=false`.

**Publisher.** controller

**Payload.**
```json
{
  "command_id": "uuid",
  "controller_admin_id": "admin record id",
  "item_code": "WIDGET-001",
  "mode": "delta" | "absolute",
  "value": 5,
  "reason": "free-form text"
}
```

**Reply data.**
```json
{
  "adjustment_id": "audit row id",
  "item_id": "items record id",
  "item_code": "WIDGET-001",
  "delta": 5,
  "new_quantity": 12,
  "prev_quantity": 7
}
```

**Idempotency.** `command_id` is unique-indexed on
`stock_adjustments`. Retried calls return the prior result without
re-applying.

**Errors.** Missing required fields, item not found, unknown mode.

### `inventory.snapshot`

Read-only — returns the kiosk's current on-hand quantities. Used by
the controller SPA to populate the inventory tab before/after an
adjust.

**Publisher.** controller

**Payload.**
```json
{
  "item_codes": ["A", "B"]   // optional; empty/missing = all stocked items
}
```

**Reply data.**
```json
{
  "items": [
    {
      "item_code": "WIDGET-001",
      "item_name": "Widget",
      "quantity_on_hand": 12,
      "reorder_threshold": 5,
      "tracking_mode": "quantity" | "serialized" | "consumable",
      "active": true
    }
  ]
}
```

**Idempotency.** Read-only; replay-safe by definition.

### `checkout.close`

Force-close one `open_checkouts` row. Writes a `transactions` row +
one `transaction_lines` row (`action="admin_close"`) and deletes the
open row in a single DB transaction. Same business logic as
`POST /api/kiosk/checkouts/by-line/{id}/close`.

**Publisher.** controller

**Payload.**
```json
{
  "command_id": "uuid",
  "controller_admin_id": "admin record id",
  "transaction_line_id": "the original checkout line id",
  "reason": "lost" | "returned_offline" | "damaged" | "other",
  "notes": "optional free-form"
}
```

**Reply data.**
```json
{
  "transaction_id": "new admin_close transaction id",
  "line_id": "new transaction_lines row id",
  "open_checkout_id": "deleted row id",
  "item_id": "...",
  "item_code": "...",
  "user_id": "...",
  "user_code": "...",
  "closure_reason": "lost"
}
```

For non-serialized rows with qty=N, one call closes one row — call N
times to close all. For serialized rows, the line is closed exactly
once.

**Idempotency.** `command_id` is unique-indexed on the new
`transactions` row. Retried calls return the prior result.

**Errors.** Missing fields, open_checkouts row not found.

### `instance.create`

Create one `item_instances` row + an `instance_audit` row + publish
an `instance.lifecycle` event.

**Publisher.** controller

**Payload.**
```json
{
  "command_id": "uuid",
  "controller_admin_id": "admin record id",
  "item_code": "WIDGET-001",
  "code": "WIDGET-001-A",
  "serial": "SN-12345",            // optional
  "rfid_epc": "abc123...",         // optional
  "notes": "optional",
  "active": true                    // optional, defaults to true
}
```

**Reply data.** Instance mutation result (instance_id, code, audit row id).

**Idempotency.** `command_id` is unique-indexed on `instance_audit`.

### `instance.edit`

Cosmetic-only field updates (code, serial, rfid_epc, notes). No audit
row, no `instance.lifecycle` event — these fields don't change inventory
semantics. Retries overwrite with the same values. `rfid_epc` is stored
normalized to lower-case, trimmed hex regardless of submitted case (a
model hook on `item_instances`), so the reader's lower-case observations
match it.

**Publisher.** controller

**Payload.**
```json
{
  "instance_code": "WIDGET-001-A",
  "code": "new code",        // any subset; null/missing = leave alone
  "serial": "new serial",
  "rfid_epc": "new EPC",
  "notes": "new notes"
}
```

**Reply data.** Updated instance fields.

**Idempotency.** Replay-safe by content; no `command_id` because there
is no audit anchor.

### `instance.set_status`

Set `item_instances.status` to a target state. Writes an audit row +
publishes an `instance.lifecycle` event. One command covers every
lifecycle transition — sending a unit to maintenance, returning it to
service, retiring it, or un-retiring it — because the target status is
data, not a separate verb.

**Publisher.** controller

**Payload.**
```json
{
  "command_id": "uuid",
  "controller_admin_id": "admin record id",
  "instance_code": "WIDGET-001-A",
  "status": "in_service" | "maintenance" | "retired",
  "reason": "free-form, required"
}
```

**Reply data.** Mutation outcome (new `status`, audit row id).

**Idempotency.** `command_id` unique on `instance_audit`.

The audit `action` verb is derived kiosk-side from the (prev → target)
transition rather than supplied by the caller:

| prev → target | action |
|---|---|
| in_service → maintenance | `to_maintenance` |
| maintenance → in_service | `return_to_service` |
| in_service / maintenance → retired | `retire` |
| retired → in_service | `unretire` |

### `instance.snapshot`

Read-only — list instances optionally filtered by `item_code`.

**Publisher.** controller

**Payload.**
```json
{
  "item_code": "WIDGET-001"   // optional; empty = all instances
}
```

**Reply data.**
```json
{
  "instances": [
    {
      "instance_id": "...",
      "instance_code": "WIDGET-001-A",
      "item_code": "WIDGET-001",
      "item_name": "Widget",
      "serial": "SN-12345",
      "rfid_epc": "...",
      "status": "in_service" | "maintenance" | "retired",
      "notes": "",
      "created": "RFC3339",
      "updated": "RFC3339"
    }
  ]
}
```

**Idempotency.** Read-only.

### `integrity.rebuild`

Wipe `open_checkouts` and rebuild it from the kiosk's own ledger.
Publishes `event.integrity.rebuild` after success.

**Publisher.** controller

**Payload.** All fields optional.
```json
{
  "command_id": "uuid",
  "controller_admin_id": "admin record id"
}
```

**Reply data.** Rebuild stats (`{deleted, inserted}`).

**Idempotency.** Replay produces the same state (the rebuild is
deterministic from the ledger).

### `ledger.republish`

Re-emit `event.transaction.complete` + `event.item.<action>` for every
completed transaction in the optional window. Used to backfill the
controller's projection after a NATS outage.

**Publisher.** controller

**Payload.** All fields optional.
```json
{
  "command_id": "uuid",
  "controller_admin_id": "admin record id",
  "from": "RFC3339 timestamp",
  "to": "RFC3339 timestamp"
}
```

**Reply data.** `{transactions_published, lines_published, skipped}`.

**Idempotency.** The controller's aggregator dedupes on
`source_kiosk_code + source_transaction_id` (for transactions) and
`source_line_id` (for lines), so repeated publishes are safe.

### `timeclock.punch`

Record a clock-in/out punch AT the kiosk on behalf of a controller admin.
The kiosk is the only punch writer — the controller's own ledger learns
about the punch by consuming the resulting `event.timeclock.punch`, same
as every other punch. Backdating (`occurred_at`) and `force` (bypass the
open-checkouts clock-out block) follow the kiosk's admin punch rules;
`reason` is always required. Replies with an error when timeclock is not
enabled on the kiosk.

**Publisher.** controller

**Payload.**
```json
{
  "command_id": "uuid",
  "controller_admin_id": "admin record id",
  "user_code": "EMP-2",
  "direction": "in" | "out",
  "reason": "forgot to clock out",
  "occurred_at": "RFC3339 (optional; empty = now)",
  "force": false
}
```

**Reply data.** The punch result (`{punch_id, user_id, user_code,
user_name, direction, occurred_at, recorded_at, source, clocked_in,
replayed?}`). `clocked_in` is the user's merged state AFTER the punch —
a backdated correction may leave it unchanged.

**Idempotency.** `command_id` unique-when-non-empty on `time_punches`; a
replayed command returns the prior result with `replayed: true` and does
not publish a second event.

### `timeclock.republish`

Re-emit `event.timeclock.punch` for every punch in the optional window.
Sibling of `ledger.republish` for the punch ledger.

**Publisher.** controller

**Payload.** All fields optional.
```json
{
  "from": "RFC3339 timestamp",
  "to": "RFC3339 timestamp"
}
```

**Reply data.** `{published, from?, to?}`.

**Idempotency.** The controller's projection dedupes on `source_punch_id`,
and its `punch_state` KV write is monotonic on `occurred_at`, so repeated
publishes are safe and old punches cannot drag fleet state backwards.

### `metrics.snapshot`

Read-only — returns the kiosk's point-in-time operational + activity
snapshot. Proxied by the controller's
`GET /api/controller/kiosks/{code}/metrics`. Same shape as the local
`GET /api/kiosk/metrics` endpoint (single source of truth in
`internal/metrics`).

**Publisher.** controller

**Payload.** None.

**Reply data.**
```json
{
  "kiosk_code": "...",
  "generated_at": "RFC3339, UTC",
  "operational": {
    "uptime_seconds": 0,
    "nats_connected": true,
    "rfid_enabled": false,
    "rfid_mode": "counter_scan",
    "rfid_connected": false,
    "active_carts": 0
  },
  "ledger": {
    "items_out": 0,
    "users_with_items_out": 0,
    "low_stock_skus": 0,
    "transactions_today": 0,
    "transactions_week": 0
  }
}
```

**Idempotency.** Read-only; replay-safe by definition.

### `cart.start`

**RFID `enclosure_diff` mode only.** Start (or reuse) a cart keyed
`(user_code, door_id)` so the subsequent `read.trigger` has a cart to
write into. Typically fired by an access-control system when a worker
badges into the enclosure door.

**Publisher.** external (access-control)

**Payload.**
```json
{
  "user_code": "W-042",
  "door_id": "cabinet-a",
  "command_id": "uuid"   // optional, for caller-side traceability
}
```

**Reply data.**
```json
{
  "cart_id": "...",
  "user_code": "W-042",
  "door_id": "cabinet-a",
  "reused": false   // true if a previous cart matched the (user, door) key
}
```

**Idempotency.** Built into the cart store's `(user_code, door_id)`
secondary index: a re-fire within the cart's idle window returns the
same `cart_id` with `reused: true`. After commit or idle expiry, the
next `cart.start` for the same key creates a fresh cart.

**Errors.** User not found, user inactive, missing fields.

### `read.trigger`

**RFID `enclosure_diff` mode only.** Run one LLRP inventory cycle,
diff observed EPCs against expected-present state, and synthesize cart
lines. Typically fired by a camera/occupancy system when the worker
steps out of the enclosure.

**Publisher.** external (camera / occupancy)

**Payload.** One of two shapes:
```json
{ "cart_id": "...", "command_id": "uuid" }
```
or
```json
{ "user_code": "W-042", "door_id": "cabinet-a", "command_id": "uuid" }
```

**Reply data.**
```json
{
  "cart": { /* full cart object */ },
  "added_lines": [ /* cart.Line[] */ ],
  "observed_epcs": ["..."],
  "unresolved_epcs": ["..."],
  "skipped_cross_user_count": 0
}
```

**Idempotency.** Re-firing produces the same diff against the same
expected state. Lines already in the cart from a previous trigger are
no-ops (cart store rejects duplicate instance writes). The manual
"Re-read enclosure" SPA button calls the same logic over HTTP — same
result.

**Errors.** No active cart for the supplied key (anonymous reads are
**rejected** by design — the doc rationale: failing loud surfaces
mis-wired access-control events). Reader unreachable. Missing fields.
Read exceeded the deadline (a slow/half-open reader).

**Deadline.** The handler bounds the LLRP read with a 4.5 s timeout
(`commands.ReadTriggerBudget`, below the 5 s reply window) so a wedged
reader can't hold the reader's serialization lock past the window — past
it the read unwinds and replies with an error rather than timing the caller
out. Config rejects `rfid.read_window > 3.5 s` in `enclosure_diff` mode for
the same reason (the read runs synchronously inside this reply window).

## Events

All events ride JetStream at `<prefix>.<kiosk_code>.event.<...>`. Every
event is also logged via `slog.Info("kiosk.event", ...)` regardless of
whether NATS is enabled.

### `event.transaction.complete`

Fires once per successful cart commit, after the DB transaction.

**Payload.**
```json
{
  "transaction_id": "...",
  "kiosk_code": "...",
  "location_code": "...",
  "user_id": "...",
  "user_code": "...",
  "user_group": "optional",
  "user_name": "optional",
  "started_at": "RFC3339",
  "completed_at": "RFC3339",
  "lines_count": 3,
  "checked_out": 2,
  "returned": 1,
  "consumed": 0
}
```

### `event.item.<action>`

One event per `transaction_lines` row. `<action>` is one of
`checkout`, `return`, `consume`, `admin_close`. Same payload shape
across actions; `action` field discriminates downstream.

**Payload.**
```json
{
  "transaction_id": "...",
  "line_id": "...",
  "item_id": "...",
  "item_code": "...",
  "item_name": "...",
  "action": "checkout",
  "qty": 1,
  "serial": "optional, serialized items only",
  "uncorrelated": false,
  "original_checkout_user_code": "optional, cross-user returns only",
  "item_instance_id": "optional",
  "kiosk_code": "...",
  "location_code": "...",
  "user_code": "...",
  "completed_at": "RFC3339"
}
```

### `event.inventory.adjust`

Fires after every accepted stock adjustment, whether the local HTTP
endpoint or the `inventory.adjust` command was the source.

**Payload.**
```json
{
  "adjustment_id": "...",
  "admin_id": "optional, local-source only",
  "controller_admin_id": "optional, controller-source only",
  "mode": "delta" | "absolute",
  "value": 5,
  "delta": 5,
  "prev_quantity": 7,
  "new_quantity": 12,
  "reason": "free-form",
  "source": "local" | "controller",
  "command_id": "optional",
  "item_id": "...",
  "item_code": "...",
  "kiosk_code": "...",
  "location_code": "..."
}
```

`adjustment_id` is the controller's idempotency anchor when projecting
into `inventory_audit`.

### `event.integrity.rebuild`

Fires after a successful `open_checkouts` rebuild, whether triggered
locally or by command.

**Payload.** `{deleted, inserted, source ("local" | "controller"),
controller_admin_id?, command_id?, kiosk_code, location_code}`.

### `event.checkout.admin_close`

Fires once per row closed via `commit.AdminClose`. Distinct from
`event.item.return` so reports can separate "worker returned it" from
"admin closed it without a return" without filtering on a
discriminator.

**Payload.**
```json
{
  "open_checkout_id": "...",
  "closure_reason": "lost" | "returned_offline" | "damaged" | "other",
  /* + the usual transaction/line/item/user/kiosk envelope */
}
```

The same close also rides `event.transaction.complete` (one
`admin_close` transaction) and `event.item.admin_close` (one line) —
the dedicated subject is in addition, not instead.

### `event.instance.lifecycle`

Fires on every `item_instances` status transition: `create` /
`to_maintenance` (in_service→maintenance) / `return_to_service`
(maintenance→in_service) / `retire` (in_service or maintenance→retired)
/ `unretire` (retired→in_service). Cosmetic edits (code, serial,
rfid_epc, notes) and status-unchanged updates do **not** publish.

**Payload.** Carries `prev_status` / `new_status` (the transition) plus
`source_audit_id` (the kiosk-side `instance_audit.id`) which the
controller uses as the idempotency anchor when projecting into
`instance_lifecycle_audit`.

### `event.timeclock.punch`

Fires once per accepted punch, any source (`self` / `foreman` / `admin` /
`controller_admin`). Idempotent replays of command-bus punches do **not**
re-publish.

**Payload.**
```json
{
  "punch_id": "kiosk-side time_punches.id (idempotency anchor)",
  "kiosk_code": "...",
  "location_code": "...",
  "user_id": "kiosk-local users.id",
  "user_code": "EMP-2",
  "user_name": "Bob",
  "direction": "in" | "out",
  "occurred_at": "RFC3339 — business timestamp (backdatable by admins)",
  "source": "self" | "foreman" | "admin" | "controller_admin",
  "recorded_by_user_code": "foreman's code (source=foreman)",
  "admin_id": "kiosk admins.id (source=admin)",
  "controller_admin_id": "controller admin id (source=controller_admin)",
  "reason": "...",
  "force": false,
  "command_id": "...",
  "recorded_at": "RFC3339 — when the row was written"
}
```

The controller projects this into its own `time_punches`
(`source_punch_id` dedupe) and then broadcasts the user's clocked-in
state into the **`punch_state` JetStream KV bucket** (key = `user_code`,
value `{user_code, clocked_in, occurred_at, source_punch_id}`), written
monotonically on `occurred_at`. Managed kiosks watch the bucket into an
in-memory replica; every clocked-in decision uses "fresher of local
ledger vs replica" — which is what lets a worker clock in at one kiosk
and out at another.

A sibling bucket carries the **clock-out gate**: after projecting each
checkout/return/admin-close line the controller recomputes the affected
worker's fleet-wide open checkouts and writes them to the
**`open_checkouts_state` JetStream KV bucket** (key = `user_code`, value
`{user_code, rows:[{item_code, item_name, serial, kiosk_code}]}`). Managed
kiosks and the phone terminal watch it into a `CheckoutFleet`; the punch
funnel merges it with this kiosk's local `open_checkouts` (partitioned by
`kiosk_code`) to block a clock-out while tools are out anywhere — fail-open
when the replica is absent, and bypassable by a worker "clock out anyway"
acknowledgment (`acknowledge:true` → `force` on the punch).

### `event.scan.rfid.observed`

Fires after every completed LLRP inventory cycle in either RFID mode.
Cheap observability — no projector consumes it today; the stream
captures it for future drift detection and analytics.

**Payload.**
```json
{
  "kiosk_code": "...",
  "location_code": "...",
  "cart_id": "...",
  "door_id": "cabinet-a",   // enclosure_diff only
  "mode": "counter_scan" | "enclosure_diff",
  "observed_epcs": ["..."],
  "observed_at": "RFC3339"
}
```

### `event.receipt.transaction` (managed mode only)

Fires alongside `event.transaction.complete` when the kiosk is
managed by a controller. Carries pre-rendered receipt context for the
controller's notifier to template + send. See
[Notifications](notifications.md).

### `event.alert.lowstock` (managed mode only)

Fires when an item crosses its `reorder_threshold` downward. Carries
low-stock context for the controller's notifier. See
[Notifications](notifications.md).

### `event.alert.maintenance` (managed mode only)

Fires when a cart commit routes one or more returned serialized units
into maintenance (per-SKU `requires_maintenance_on_return`, a per-line
"needs maintenance" toggle, or both). **Batched one per transaction** —
a cart returning N flagged units produces a single event listing all N,
not N events. Carries `MaintenanceContext` for the controller's notifier
to template + send (recipients = all admins). Rides the same JetStream
durable consumer as `alert.lowstock`. See [Notifications](notifications.md).

**Payload.**
```json
{
  "Kiosk": { /* kiosk info */ },
  "Units": [
    {
      "ItemCode": "WIDGET-001",
      "ItemName": "Widget",
      "InstanceCode": "WIDGET-001-A",
      "Serial": "SN-12345",
      "Reason": "free-form"
    }
  ],
  "Trigger": "return",
  "Ref": "transaction id"
}
```

The controller dedupes on `Ref` (the transaction id) via `SendIfFirst`,
so JetStream redelivery of the same batch collapses to one email. A
companion `digest.maintenance` scheduled report ("Items in maintenance")
lists units currently parked in maintenance — see
[Notifications](notifications.md).

## Heartbeats

```
<prefix>.<kiosk_code>.heartbeat
```

Core NATS publish (not JetStream). The controller's
`HeartbeatRegistry` keeps the most recent beat per kiosk in memory and
serves it at `GET /api/controller/kiosks/heartbeats`. No persistence
by design — durability would mask the very signal we care about.

**Cadence.** 45 s.

**Payload.**
```json
{
  "ts": "RFC3339",
  "kiosk_code": "...",
  "version": "optional, build version when ldflags-injected"
}
```

First beat from a previously-unknown kiosk also triggers
auto-registration of a `kiosks` row on the controller side, parallel
to the aggregator's `touchKiosk` path.

## Subscribing for ad-hoc inspection

Tail every event:
```bash
nats sub "kiosk.*.event.>"
```

Tail one kiosk:
```bash
nats sub "kiosk.KC-MAIN-01.>"
```

Tail commands going to one kiosk (catches retries and external pubs):
```bash
nats sub "kiosk.KC-MAIN-01.command.>"
```

Tail heartbeats:
```bash
nats sub "kiosk.*.heartbeat"
```
