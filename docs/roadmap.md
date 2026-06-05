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
  JetStream) under `{prefix}.{kiosk_code}.command.<name>`. v1 shipped
  `inventory.adjust` (mutating, idempotent via a server-generated
  `command_id` unique-indexed on `stock_adjustments`) and
  `inventory.snapshot` (read-only); the bus has since grown to cover
  `checkout.close` (admin force-close), the `instance.*` family,
  `integrity.rebuild`, `ledger.republish`, and the RFID
  `cart.start`/`read.trigger` pair (see the Wire reference). Controller
  endpoints fast-fail with 503 when the kiosk's heartbeat is stale, and pass
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
- **Centralized notifications in managed mode.** Two new JetStream
  subjects (`receipt.transaction`, `alert.lowstock`) let managed
  kiosks publish structured context payloads; the controller's
  aggregator dispatches each through its own notifier against
  fleet-global templates and the controller's SMTP. The SPA's
  Notifications view detects role at boot and points its CRUD at
  `/api/controller/notifications` when running on the controller. One
  set of SMTP credentials, one audit trail.
- **Scheduled reports owned by the controller in managed mode.** The
  `scheduled_reports` collection moves to the controller's
  responsibility: cron, computation against the projected
  `open_checkouts` table + ledger, and SMTP all run on the controller.
  The kiosk-side scheduler stays off when `controller.enabled=true`.
  An optional `kiosk_code` column on each schedule row scopes the
  report (empty = fleet-wide, set = one kiosk). The old kiosk →
  controller `DigestEnvelope` NATS hop is gone.
- **Fleet-wide low-stock report.**
  `GET /api/controller/reports/low-stock` fans `inventory.snapshot`
  out to every online managed kiosk in parallel and joins each
  reply with `out` counts computed locally from the controller's
  projected ledger. Offline kiosks surface in an `errors` block so
  the SPA can render "partial result" transparently. SPA's Low-stock
  tab on the controller drops its "on the roadmap" banner for a real
  fleet table.
- **Inventory adjustment audit.** Every `inventory.adjust` event the
  controller receives is now projected into `inventory_audit`
  (denormalized: kiosk, item, delta, prev/new, reason, source,
  actor), idempotent via a unique `source_adjustment_id`. A new
  Reports → Adjustment audit tab on the controller exposes
  filterable, paginated history of every stock change anywhere in
  the fleet.
- **Notifications deliverability tab.** A Reports tab on both
  binaries summarizes the local `notification_send_log` (totals,
  per-event success-rate table, recent failures) so deliverability
  regressions surface without clicking through individual rows.
- **Admin force-close (lost / damaged / returned offline).** Admins
  can now resolve stale `open_checkouts` rows without bypassing the
  ledger. New `action="admin_close"` on `transaction_lines` plus a
  `closure_reason` enum; `lost` / `damaged` drop the item's inventory
  count — for quantity-tracked items by writing a `stock_adjustments`
  row, for serialized items by retiring the instance (whose
  non-retired count the derived quantity follows; no `stock_adjustments` row)
  — atomic with the close. The kiosk endpoint and the
  controller's NATS-forwarded `checkout.close` command both converge
  on `commit.AdminClose`, so a controller-driven close on a remote
  kiosk behaves identically to a local one (including the qty + audit
  side-effects).
- **Instance lifecycle audit + parity.** New `instance_audit`
  collection on each kiosk, written by PB record hooks on
  `item_instances` (create / to_maintenance / return_to_service /
  retire / unretire; cosmetic edits skip). Lifecycle changes also publish
  `instance.lifecycle` events; the controller projects them into a
  fleet-wide `instance_lifecycle_audit` collection (idempotent via
  `source_audit_id`). The SPA's Instance lifecycle Reports tab is
  visible on both binaries against the appropriate collection so a
  managed kiosk's admin gets the same visibility a standalone
  kiosk's admin does.
- **Serialized maintenance workflow.** `item_instances.active` (bool)
  became a 3-state `status` enum (`in_service` / `maintenance` /
  `retired`); units are never hard-deleted (retire instead, so the
  ledger's FKs stay alive). A maintenance unit counts toward
  `quantity_on_hand` but not toward `available`. Returns can route a
  unit straight into maintenance three ways: a per-SKU
  `items.requires_maintenance_on_return` opt-in, a per-line "needs
  maintenance" toggle any worker can set, or a manual admin
  "send to maintenance." A batched `alert.maintenance` notification
  (one email per transaction) and a `digest.maintenance` scheduled
  report ("Items in maintenance") round it out. Controller and kiosk
  reach parity: the single `instance.set_status` command/endpoint
  replaces the old decommission/reactivate pair, and the Instances tab
  shows per-unit status.
- **Worker self-service ("What you have out").** The `/api/kiosk/scan`
  response now hydrates the scanned worker's outstanding
  `open_checkouts`; the CheckoutView surfaces a collapsible panel
  above the cart so a worker reviewing their tally doesn't need an
  admin to look up "what does this user have out right now?"
- **Explicit foreman returns.** Scanning a tool another worker has
  out no longer implicitly switches the action to a cross-user
  return — the natural reading for quantity-tracked tools is "give
  me one too," not "close someone else's checkout." Foremen now
  initiate cross-user returns through a dedicated "Return on behalf
  of…" dialog that lists workers in their group with at least one
  open checkout (hydrated via
  `GET /api/kiosk/cart/foreman-return/options`) and accepts a
  serialized-instance scan as a one-step shortcut. The dialog is the
  sole writer of `Line.original_checkout_user_id`; the commit-time
  foreman+same-group gate stays as the trust boundary.
- **RFID integration (Impinj R700 / LLRP).** Two modes share one
  `*llrp.Client` wrapped in `internal/rfid` (imported from EdgeX
  `device-rfid-llrp-go/pkg/llrp`, pinned at a `@main`
  pseudo-version). **counter_scan** runs an operator-driven 3-5 s
  inventory cycle from a button on `CheckoutView` and folds each
  observed tag into the cart through the existing add path
  (`POST /api/kiosk/cart/rfid-scan`). **enclosure_diff** is
  NATS-orchestrated: external access-control fires `cart.start`
  with `{user_code, door_id}` (idempotent on a secondary cart-store
  index keyed by that pair), then a camera/occupancy system fires
  `read.trigger`; the kiosk runs a pure diff against expected-present
  state via `rfid.Diff` and synthesizes self-return / checkout cart
  lines. Cross-user returns are skip-and-counted (the commit-time
  foreman gate would reject them anyway). Every read publishes
  `event.scan.rfid.observed` for downstream audit. A small SSE
  channel (`GET /api/kiosk/cart/events`) lets the SPA refetch on any
  cart write — the same broker absorbs both modes plus the existing
  HTTP write paths. Trust boundary stays 127.0.0.1; external systems
  only reach the kiosk via NATS (per-subject ACLs gate them). USB
  HID badge readers are unchanged — they emit keyboard events and
  resolve through the existing `scan.Resolver`. See [RFID](rfid.md).

## Roadmap

Items below are still intentionally deferred. Schema and event
subjects are in place to make them additive rather than rewrites.

- **Controller-side qty projection.** The low-stock report above
  uses live snapshot fan-out — one NATS round-trip per online kiosk
  per page load. Fine into the dozens of kiosks; not free. Projecting
  `inventory.adjust` plus the `item.checkout` / `item.consume` qty
  deltas into a controller-side `kiosk_inventory` rollup would make
  the report query O(1) and survive offline kiosks. Deferred until
  fan-out RTT actually hurts.
- **More remote commands.** The command bus and dispatcher are in
  place (`internal/commands/`). Since v1 it has grown well past inventory
  adjust + snapshot (admin force-close, the `instance.*` family,
  `integrity.rebuild`, `ledger.republish`, RFID `cart.start`/`read.trigger`
  — see the shipped entry above). Natural next commands still deferred:
  force a catalog resync, lock a kiosk to a holding screen. Each is a
  single handler on the kiosk side plus a controller endpoint that fires
  `nc.Request` at the appropriate subject.
- **Drift detection.** Periodic state-hash compare between controller
  and each kiosk; surface discrepancies in the controller admin UI
  for triage.
- **Cross-fleet movement of serialized items.** Move a specific
  `item_instances` row from kiosk A to kiosk B with central as the
  arbiter — one serial belongs to one kiosk at a time.
- **Tighten PB collection rules in managed mode.** UI gating is the
  v1 story; a follow-up could lock the collection rules themselves
  so a determined admin poking PB directly can't drift the catalog.
Each of these can be evaluated on demand. None should be built until
there is a concrete user asking for it.
