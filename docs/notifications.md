# Notifications

The admin SPA's **Notifications** tab manages four event types out of
the box: transaction receipts, low-stock alerts, scheduled
open-checkouts digests, and scheduled daily-activity digests. Each
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

Scheduled digests (`open_checkouts`, `daily_activity`) no longer ride
NATS. The controller owns the `scheduled_reports` rows, the cron, the
digest computation (against its projected `open_checkouts` + ledger),
and the SMTP send. The kiosk's scheduler is skipped entirely when
`controller.enabled=true`; the SPA on the kiosk side shows a banner
directing operators to the controller. Standalone kiosks keep the
local scheduler + local SMTP path unchanged.

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
