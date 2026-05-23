# Notifications

The admin SPA's **Notifications** tab manages three event types out of
the box: transaction receipts, low-stock alerts, and scheduled
open-checkouts digests. Each event has an editable subject + body (Go
`text/template` syntax) and an editable recipients spec
(`worker_email`, `all_admins`, `extras: []`). Sends are logged one row
per recipient with `status` = `sent` / `failed` / `skipped`.

Where templates are authored, where SMTP credentials live, and where
the audit trail lands depends on whether the kiosk is managed:

| | Standalone kiosk | Managed kiosk |
|---|---|---|
| Template authoring | `/admin/notifications` on this kiosk | `/admin/notifications` on the **controller** |
| SMTP credentials | this kiosk's PocketBase superuser UI (`/_/` → Settings → Mail) | controller's PocketBase superuser UI |
| Send log | this kiosk's `notification_send_log` | controller's `notification_send_log` |
| Schedule rows | kiosk-local | **kiosk-local** (because `open_checkouts` is kiosk-local) |
| Cron timing + digest computation | kiosk | kiosk |
| Render + SMTP dispatch | kiosk | **controller** (over NATS) |

In managed mode the kiosk's local `notification_templates` rows still
exist (they're seeded by the same migrations) but are dormant — the
commit hook publishes a structured context over NATS instead of calling
the local notifier. The aggregator subscribes to:

- `{prefix}.{kiosk_code}.receipt.transaction` — full `ReceiptContext`
  (kiosk, user, transaction, lines). Controller dedupes on
  `transaction_id` via `notification_dedupe` so JetStream redelivery
  never double-sends.
- `{prefix}.{kiosk_code}.alert.lowstock` — `LowStockContext`.
  Day-scoped dedup is the intended semantics: "tell me once per item
  per day," not just a redelivery guard.
- `{prefix}.{kiosk_code}.digest.open_checkouts` — a `DigestEnvelope`
  (`{context, recipients}`) carrying the per-schedule recipients
  override. No dedup: digests are meant to fire each cadence.

Schedule rows stay on each kiosk because the controller doesn't project
`open_checkouts`. The kiosk's scheduler computes the digest locally and
ships the envelope; the controller renders against its own template,
sends to the embedded recipients, and writes its own send-log row. The
kiosk's `scheduled_reports.last_status` reflects the *publish* outcome
— view the controller's "Recent sends" tab for the SMTP outcome.

`recipients.all_admins` resolves against whichever app's `admins`
collection the notifier is bound to — controller admins on the
controller, kiosk admins on standalone. Worker recipients work as long
as `users.email` is populated.
