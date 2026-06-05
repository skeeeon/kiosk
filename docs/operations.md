# Operations

## Deploying a new kiosk

Minimum viable deploy:

```bash
# On the kiosk host (Linux example)
mkdir -p /opt/kiosk
cp kiosk-app /opt/kiosk/
cp kiosk.yaml /opt/kiosk/                 # customize kiosk.code / location_code
cd /opt/kiosk && ./kiosk-app
```

The binary creates `pb_data/` next to itself on first run, applies
migrations, and prints the bootstrap admin credentials. Save them, then
point Chromium (or any browser, in kiosk mode if appropriate) at
`http://localhost:8090/`.

To auto-start on boot, wrap the binary in whatever supervisor your host
uses (systemd, runit, OpenRC, Windows service, etc.). The binary needs:

- A working directory containing `kiosk.yaml` (or
  `KIOSK_CONFIG=/path/to/yaml`)
- Write access to that directory (for `pb_data/`)
- A reachable port (default 8090, bound to `127.0.0.1`)

## Backups

The SQLite file at `pb_data/data.db` is the entire kiosk state.

```bash
# Simple hot copy — safe enough for a low-write system like this:
cp /opt/kiosk/pb_data/data.db /backups/data-$(date +%Y%m%d-%H).db
```

For belt-and-suspenders, use SQLite's online backup:

```bash
sqlite3 /opt/kiosk/pb_data/data.db ".backup /backups/data-$(date +%Y%m%d-%H).db"
```

Schedule it however your host schedules things. Hourly is plenty for a
tool crib; the kiosk write rate is human-paced.

## Verifying ledger integrity

After any suspected hiccup (power loss, manual DB edit, hook bug), an
admin can hit `GET /api/kiosk/integrity` (via the admin UI's PB SDK
auth) and inspect the diff. Clean ledger:

```json
{
  "checked_lines": 247,
  "expected_open": 18,
  "actual_open": 18,
  "missing_in_table": [],
  "extra_in_table": []
}
```

If `missing_in_table` or `extra_in_table` is non-empty, the
`transaction_lines` ledger is authoritative — the `open_checkouts`
table can be rebuilt from it.

To rebuild: in the admin SPA, go to **Reports → Currently out** and
click **Rebuild from ledger** at the bottom of the list. A confirm
modal warns that this wipes and repopulates `open_checkouts` inside a
single transaction. Or call `POST /api/kiosk/integrity/rebuild`
directly with an admin token. The response reports `{ deleted,
inserted }` counts. Each rebuilt row carries the source checkout line's
`completed_at` as `checked_out_at` and a FK back to the originating
`transaction_line`, so aging and audit reports stay meaningful after a
rebuild.

## Resyncing the kiosk ledger to the controller

When the controller's projected ledger has drifted from a kiosk's local
one — typically because NATS was briefly unreachable and an event was
lost on kiosk restart — the kiosk can re-emit its history:

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
re-emits one `transaction.complete` event plus one `item.{action}`
event per line, with payloads rebuilt from the persisted records and
the kiosk_code/location_code that were stamped at original commit time.

Safe to re-run: the controller's aggregator dedupes on
`(source_kiosk_code, source_transaction_id)` for transactions and on
`source_line_id` for lines. Re-publishing the entire history is the
brute-force recovery; the `{from, to}` scope is friendlier for routine
ops. Response is `{ transactions_published, lines_published, skipped }`.

## NATS failure modes

NATS is best-effort on the kiosk and hard-required on the controller.
This section enumerates what survives each failure and what gets lost,
so you can decide what to monitor.

| Scenario | Kiosk behavior | Controller behavior |
|---|---|---|
| Broker unreachable at kiosk startup | Boots normally; `events.Connect` returns a buffering connection that dials in the background. The local ledger is authoritative; checkouts work without NATS. Catalog sync (if enabled) logs a warning and proceeds without sync until the broker is reachable. | N/A |
| Broker unreachable at controller startup | N/A | Controller fails to start (NATS is required). Operator must bring the broker up first. |
| Broker dies mid-publish (kiosk → controller) | The event is queued in the NATS client's in-memory buffer. If the buffer overflows or the kiosk process restarts before reconnect, the event is **lost** from the controller's view — but the kiosk's local ledger still has the underlying transaction. Re-emit the kiosk's history with `POST /api/kiosk/ledger/republish` (or the controller-driven `POST /api/controller/kiosks/{code}/ledger/republish`), optionally scoped to a `{from,to}` window — see "Resyncing the kiosk ledger to the controller" above; the controller's projection is idempotent (admin force-closes included). | Misses the event; its projected ledger silently drifts from the kiosk's. Surface via cross-checking aggregate counts kiosk-side vs. controller-side. |
| Broker dies mid-publish (controller → kiosk catalog KV) | If the kiosk is offline at the time, the kiosk re-syncs from the bucket's current value on next connect (`Watch` replays the latest value per key). No loss. | The `Put` call fails; the controller logs a warning. The DB record is already saved, so the controller's state is correct — only the KV propagation failed. Re-save the record (any edit) to retry, or restart the controller to re-publish. |
| Broker recovers after outage | Buffered events flush automatically. Durable consumer (controller side) resumes from last-acked sequence — no replay storm. KV watchers reattach and project any keys that changed during the outage. | Same. |
| Controller process restart | N/A | Durable consumer `controller-aggregator` resumes from last-acked sequence. KV `CreateOrUpdateKeyValue` is idempotent. Hooks re-bind on the next save. No event loss across restarts; events that arrived *during* the outage are still in the JetStream stream (retention default: 7 days). |
| Kiosk process restart | In-memory cart is lost (documented). NATS publisher reconnects. Watcher (if managed) re-projects the current KV snapshot. Local ledger is intact on disk. | N/A |
| JetStream stream retention expires | N/A | Events older than `MaxAge` (default 7 days) drop off the stream. Re-running the consumer won't replay them. For long-term ledger archival, rely on the kiosk's local ledger or controller's projected ledger — both are persistent SQLite. |

The two big takeaways:

1. **Local ledgers are authoritative.** The kiosk's `pb_data/data.db`
   and the controller's `pb_data_controller/data.db` are the source of
   truth for their respective views. NATS is a transport, not a
   database.
2. **The controller's projected ledger can drift if events are lost in
   flight.** Today this is detected manually by spot-checking aggregate
   counts. A drift-detection job (compare per-kiosk
   `transactions.count` between controller and kiosk) is on the roadmap.

## Adjusting stock

For quantity-tracked tools and consumables, admins should change
`items.quantity_on_hand` through the admin UI's **Adjust…** button (next
to the read-only quantity in the item dialog), not by editing the value
directly. The Adjust dialog supports a signed delta or an absolute "set
to N" mode, both requiring a free-form reason. Each submission writes a
`stock_adjustments` row in the same transaction as the item update,
capturing who/what/why. Past adjustments for an item are viewable via
the **View adjustment history** link below the threshold field.

Serialized items have **no** Adjust button: their `quantity_on_hand` is
derived from the count of non-retired instances (in service + maintenance)
and isn't directly editable. The item dialog shows it read-only as "Units
in service"; change it by adding instances or moving them through their
lifecycle — send to maintenance, return to service, retire, un-retire (see
[Managing instances of serialized tools](#managing-instances-of-serialized-tools)
below). The `/adjust` endpoint and the controller's `inventory.adjust`
command both reject serialized items.

The Low Stock tab on Reports surfaces every active item whose available
quantity is at or below its `reorder_threshold`. For serialized SKUs,
"available" is `active instance count − currently-out instance count`;
for quantity items, it's `quantity_on_hand − count(open_checkouts)`;
for consumables, just `quantity_on_hand`.

## Adjusting stock from the controller (remote)

Controller admins can adjust a kiosk's stock without walking to it.
From the controller's **Kiosks → \<kiosk\> → Inventory** tab, the SPA
fetches a live snapshot (`inventory.snapshot` over NATS) and offers a
per-row **Adjust** button that opens the same delta/absolute/reason
dialog. Submitting fires the `inventory.adjust` command at the target
kiosk; the kiosk runs the same `PerformStockAdjustment` business logic
the local endpoint uses, writes a `stock_adjustments` row with
`source='controller'` and the controller admin's id in
`controller_admin_id`, and publishes the usual `inventory.adjust` event
back through JetStream. The controller-side audit log therefore sees
one event shape regardless of origin.

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
  click; the future "reconcile" tool will accept an external
  `command_id` for explicit replay.

## Managing instances of serialized tools

In the admin SPA, opening (or creating + saving) a serialized item
shows an **Instances** panel below the form. Each row is one physical
unit:

- **Code** — the barcode physically on the unit (e.g. `DR-042-A`). What
  workers actually scan. Wins over the SKU code on collision.
- **Serial** — the printed serial label (informational).
- **RFID EPC** — optional tag; resolves through the same scan dispatcher.
- **Status** — the lifecycle state: `in_service` (checkout-eligible),
  `maintenance` (parked at the bench — still owned/on-hand but not
  available), or `retired` (out of service, reversibly). Each row offers
  only the transitions valid for its current status: **Send to
  maintenance** / **Return to service** / **Retire** / **Un-retire**.

Units are **never hard-deleted** — retire them instead, which preserves
ledger history. A `requires_maintenance_on_return` flag on the SKU (and a
per-return "needs maintenance" toggle any worker can set) routes returned
units straight into maintenance. The Items list shows an `N inst` badge
(plus an amber `N maint` badge when any units are in maintenance) next to
the tracking mode for serialized rows so an admin can see at a glance how
many physical units exist per SKU.

Each of these lifecycle changes — create, to_maintenance,
return_to_service, retire, unretire — recomputes the parent SKU's
`quantity_on_hand` to the current count of **non-retired** instances
(in service + maintenance). That's the only way a serialized SKU's
quantity moves; it can't be hand-adjusted. A `lost`/`damaged` admin
close of a serialized unit retires its instance the same way, so the
derived quantity drops by one without writing a `stock_adjustments`
row.

## Resetting the bootstrap admin password

The bootstrap admin email is `admin@kiosk.local`. If you've lost the
password:

1. Open `http://localhost:8090/_/` and sign in as the PocketBase superuser.
2. Open the `admins` collection, find the `admin@kiosk.local` record, click it.
3. Set a new password, save.

The superuser at `/_/` is separate from the kiosk's `admins`
collection. The superuser is created interactively on first visit to
`/_/` and is for managing PocketBase itself (collections, settings,
system tables).
