# Shipped & roadmap

## Shipped since v1

These started as deferred roadmap items and are now live in the binary:

- **Stock tracking with audit log.** `items.quantity_on_hand` /
  `reorder_threshold`, automatic decrement on `consume`, low-stock
  report, `/items/{id}/adjust` endpoint with `stock_adjustments` audit
  table.
- **Per-instance serialized tracking.** `item_instances` collection,
  scan resolver precedence, per-instance returns, admin instances panel.
- **Returns policy enforcement.** `allow_cross_user` /
  `allow_uncorrelated` flags are honored at commit time and roll back
  the transaction when set to `false`.
- **NATS publishing.** `events.Publish` dual-publishes to NATS when
  `nats.enabled=true`; supports all `nats.go` auth modes; unreachable
  servers don't block the kiosk from booting.
- **Ledger rebuild.** Admin button + `/integrity/rebuild` endpoint
  repopulate `open_checkouts` from the ledger.
- **Central controller (MVP).** New `cmd/controller` binary with a
  JetStream durable consumer aggregating per-kiosk transactions into a
  central ledger plus PB hooks publishing item/user catalog changes
  down to managed kiosks via JetStream KV. Kiosks opt in via the
  `controller:` config block; the admin SPA gates catalog mutation
  affordances in managed mode. See [Central controller](controller.md).
- **Per-kiosk catalog membership.** Controller-side `kiosk_items` join
  collection plus namespaced KV keys (`<kiosk_code>.<item_code>`) and
  a prefix-filtered kiosk-side watch. Admin UI on the controller has
  a "Stocked items" panel per kiosk with add/remove and a "Bulk add
  by category" snapshot action, plus a "Stocked at" reverse view on
  each item. Kiosks can also be pre-registered by an admin before
  they phone home so memberships can be assigned ahead of time.
- **Controller→kiosk command bus.** Core NATS request/reply (not
  JetStream) under `{prefix}.{kiosk_code}.command.<name>`. Two
  commands ship in v1: `inventory.adjust` (mutating, idempotent via a
  server-generated `command_id` unique-indexed on `stock_adjustments`)
  and `inventory.snapshot` (read-only). Controller endpoints
  fast-fail with 503 when the kiosk's heartbeat is stale, and pass
  the kiosk's reply through unchanged to the SPA.
- **Heartbeat + online status.** Each kiosk publishes a 45 s heartbeat
  on `{prefix}.{kiosk_code}.heartbeat` (core NATS, no persistence).
  The controller keeps an in-memory map and exposes
  `GET /api/controller/kiosks/heartbeats`; the SPA polls every 10 s
  and renders online/stale/offline badges in the list view and on the
  per-kiosk detail page.
- **Kiosk detail page.** New `/admin/kiosks/<code>` route on the
  controller SPA replaces the cramped edit dialog. Three tabs
  (Overview, Items, Inventory) gather all per-kiosk admin work behind
  a single deep link. The old `KioskDialog` shrank to create-only.
- **`kiosks.last_transaction_at`.** New field that means what its
  name says: "when did this kiosk last actually transact?"
  `touchKiosk` is now narrowed to `transaction.complete` events only
  — general liveness moved to the heartbeat. `last_seen` writes
  alongside it for one release as a deprecation window.
- **Notifications system.** Admin-edited templates for transaction
  receipts, low-stock alerts, and scheduled open-checkouts digests
  (`notification_templates`), with per-event recipients specs, a
  daily dedup gate (`notification_dedupe`), and a one-row-per-recipient
  send log (`notification_send_log`) pruned at 90 days. Scheduled
  rows (`scheduled_reports`) drive the digest cron with per-row
  recipients overrides.
- **Centralized notifications in managed mode.** Three new JetStream
  subjects (`receipt.transaction`, `alert.lowstock`,
  `digest.open_checkouts`) let managed kiosks publish structured
  context payloads; the controller's aggregator dispatches each
  through its own notifier against fleet-global templates and the
  controller's SMTP. The SPA's Notifications view detects role at
  boot and points its CRUD at `/api/controller/notifications` when
  running on the controller. One set of SMTP credentials, one audit
  trail.

## Roadmap

Items below are still intentionally deferred. Schema and event
subjects are in place to make them additive rather than rewrites.

- **Controller-side qty projection.** The kiosk already publishes
  `{prefix}.{kiosk_code}.inventory.adjust` for every admin adjustment
  (local or controller-driven), and the controller ack-and-logs it.
  What's still deferred is projecting those adjustments (and
  `item.checkout` / `item.consume` qty deltas) into a controller-side
  per-kiosk `quantity_on_hand` so the controller has a fleet-wide
  low-stock view without re-querying each kiosk via
  `inventory.snapshot`.
- **More remote commands.** The command bus and dispatcher are in
  place (`internal/commands/`); v1 wires inventory adjust + snapshot.
  Natural next commands: force a catalog resync, lock a kiosk to a
  holding screen, integrity rebuild from the controller, ledger
  republish. Each is a single handler on the kiosk side plus a
  controller endpoint that fires `nc.Request` at the appropriate
  subject.
- **Drift detection.** Periodic state-hash compare between controller
  and each kiosk; surface discrepancies in the controller admin UI
  for triage.
- **Cross-fleet movement of serialized items.** Move a specific
  `item_instances` row from kiosk A to kiosk B with central as the
  arbiter — one serial belongs to one kiosk at a time.
- **Tighten PB collection rules in managed mode.** UI gating is the
  v1 story; a follow-up could lock the collection rules themselves
  so a determined admin poking PB directly can't drift the catalog.
- **Per-subject NATS ACLs.** Today any holder of the NATS credentials
  can publish to `{prefix}.*.command.>`. Locking the command pattern
  to controller-only credentials (and the event subjects to
  kiosk-only credentials) is a deployment-time tightening worth
  doing before any multi-tenant scenario.
- **RFID reader integration.** Impinj reader publishes scans to
  `kiosk.{kiosk_code}.scan.rfid`. The scan dispatcher already
  resolves `rfid_epc` against `item_instances` — no new dispatch
  logic needed.

Each of these can be evaluated on demand. None should be built until
there is a concrete user asking for it.
