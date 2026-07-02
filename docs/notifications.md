# Notifications

The admin SPA's **Notifications** tab manages nine event types out of
the box: transaction receipts, low-stock alerts, maintenance alerts
(a serialized unit routed into maintenance on return), scheduled
open-checkouts digests, scheduled daily-activity digests, a scheduled
"items in maintenance" digest, a scheduled custody-vs-location
reconciliation digest, a scheduled timeclock digest (every
worker's hours in one admin email), and a scheduled per-worker timeclock
summary (each active worker emailed their own timesheet). Each
event has an editable subject + body (Go `text/template` syntax) and
an editable recipients spec (`worker_email`, `all_admins`, `extras:
[]`). Sends are logged one row per recipient with `status` = `sent` /
`failed` / `skipped`.

Where templates are authored, where SMTP credentials live, and where
the audit trail lands depends on whether the kiosk is managed:

| | Standalone kiosk | Managed kiosk |
|---|---|---|
| Template authoring | `/admin/notifications` on this kiosk | `/admin/notifications` on the **controller** |
| SMTP credentials | this kiosk's PocketBase superuser UI (`/_/` → Settings → Mail) | controller's PocketBase superuser UI |
| Send log | this kiosk's `notification_send_log` | controller's `notification_send_log` |
| Schedule rows | kiosk-local | **controller** (with optional per-kiosk scope) |
| Cron timing + digest computation | kiosk | **controller** |
| Render + SMTP dispatch | kiosk | controller |

In managed mode the kiosk's local `notification_templates` rows still
exist (they're seeded by the same migrations) but are dormant — the
commit hook publishes a structured context over NATS instead of calling
the local notifier. The aggregator subscribes to:

- `{prefix}.{kiosk_code}.event.receipt.transaction` — full `ReceiptContext`
  (kiosk, user, transaction, lines). Controller dedupes on
  `transaction_id` via `notification_dedupe` so JetStream redelivery
  never double-sends.
- `{prefix}.{kiosk_code}.event.alert.lowstock` — `LowStockContext`.
  Day-scoped dedup is the intended semantics: "tell me once per item
  per day," not just a redelivery guard.
- `{prefix}.{kiosk_code}.event.alert.maintenance` — `MaintenanceContext`,
  batched one-per-transaction: a cart returning N serialized units flagged
  for maintenance produces a single email listing all N. Controller
  dedupes on the transaction id (`Ref`) so JetStream redelivery collapses
  to one send. Recipients = all admins.

Scheduled digests (`open_checkouts`, `daily_activity`, `maintenance`,
`reconciliation`, `timeclock`, `timeclock_self`) no longer ride NATS. The controller owns
the `scheduled_reports` rows,
the cron, the digest computation, and the SMTP send. The kiosk's
scheduler is skipped entirely when `controller.enabled=true`; the SPA on
the kiosk side shows a banner directing operators to the controller.
Standalone kiosks keep the local scheduler + local SMTP path unchanged.

The `maintenance` digest (`digest.maintenance`, "Items in maintenance",
`MaintenanceDigestContext`) lists serialized units currently parked in
maintenance. A standalone run queries the local `item_instances` table
(`status=maintenance`); the controller runs a live `instance.snapshot`
fan-out across online kiosks filtered to `status=maintenance`, listing
offline kiosks as excluded so the operator knows the digest is partial.
The `open_checkouts` and `daily_activity` digests compute against the
controller's projected ledger — "what's out" is derived on demand by
replaying `transaction_lines` (`ledger.ReplayOpenRows`), the same path
the kiosk's integrity check uses; the controller does not materialize an
`open_checkouts` table.

The `reconciliation` digest (`digest.reconciliation`,
`ReconciliationDigestContext`) emails the custody-vs-location
discrepancy set from the reconciliation report — `not_taken` / `stale` /
`unaccounted` rows (see the [location & sightings
plan](location-sightings-plan.md)). It runs pure-DB on both binaries:
standalone against the node's own `item_instances.last_observed_*`
columns, the controller against its fleet `instance_location`
projection.

The two timeclock digests pair raw punches into display totals (the
raw-punch CSV stays the payroll contract). `digest.timeclock`
("Timeclock digest", `TimeclockDigestContext`) is one admin email listing
every worker's day-totals for the window. `digest.timeclock_self`
("Timeclock summary (per worker)", `TimeclockSelfDigestContext`) is a
**fan-out**: a single schedule row expands into one private email per
active worker with punches in the window, each scoped to and delivered
only to that worker. This is the scheduler's one-row-to-many delivery
shape (`fanOutRunners`, beside the default one-row-to-one
`reportRunners`) — the runner owns its own per-recipient sends, the row's
recipients column is ignored (the SPA hides the recipients editor for
it), and a single worker's bounce is logged without failing the whole
schedule. Both run pure-DB on either binary with no live-snapshot
fan-out (the timeclock ledger is fully projected), so a fleet-wide
controller row gives each worker their fleet-complete hours across every
kiosk.

Schedule rows carry an optional `kiosk_code` column. Empty = fleet-wide
(one digest covering every kiosk's data); set = scoped to one kiosk.
The dialog on the controller-side SPA exposes a "Scope" dropdown
populated from the `kiosks` collection; on a standalone kiosk the
field stays empty and out of the UI.

`recipients.all_admins` resolves against whichever app's `admins`
collection the notifier is bound to — controller admins on the
controller, kiosk admins on standalone. Worker recipients work as long
as `users.email` is populated.

## Operator visibility

Two SPA surfaces watch the send log:

- **Notifications → Recent sends** (`/admin/notifications/log`) — every
  attempted recipient in chronological order, filterable by event type
  and status. Read each row in detail.
- **Reports → Notifications** — aggregated tally over a selectable
  window (24h / 7d / 30d / 90d) plus a "Recent failures" panel showing
  the last 10 failed sends with their error strings. The success rate
  per event type is color-coded so a regression jumps out at a glance.
  Same tab works on standalone kiosks against their local
  `notification_send_log` and on the controller against the fleet-wide
  log.
