# API reference

## Custom `/api/kiosk/*` endpoints

All custom endpoints serve the kiosk checkout flow or admin operations.
PB's `/api/collections/*` is used for PB-native CRUD.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/kiosk/identity` | none | Returns `{kiosk_code, location_code, branding, max_qty, managed, rfid_enabled, rfid_mode}` — `managed` is true when opted into central control; `rfid_enabled` + `rfid_mode` gate the SPA's "RFID scan" / "Re-read enclosure" buttons (`counter_scan` shows the former, `enclosure_diff` shows the latter; both omitted when RFID is off) |
| `POST` | `/api/kiosk/scan` | none | Resolves a raw scan to `user`, `item`, or `unknown` |
| `POST` | `/api/kiosk/cart/start` | none | Returns existing or new cart for a user code |
| `POST` | `/api/kiosk/cart/add` | none | Appends or stacks a line; computes default action. Defaults: consumable → `consume`; tool already out to the cart's user → `return`; otherwise → `checkout`. Never sets `original_checkout_user_id` — cross-user returns flow through the dedicated endpoint below. |
| `GET` | `/api/kiosk/cart/foreman-return/options` | none | Returns the picker payload for the "Return on behalf of…" dialog: workers in the cart user's group who have ≥1 open checkout, hydrated with their outstanding rows. Requires the cart user to be a `foreman` with a non-empty group. Query: `?cart_id=…`. |
| `POST` | `/api/kiosk/cart/foreman-return` | none | Adds a return line on behalf of another worker. Body: `{cart_id, item_code, target_user_code?}`. `target_user_code` is optional only when `item_code` resolves to a **serialized** instance — the server derives the holder from the instance's open_checkouts row. Pre-flights cart user is a `foreman` with a group and target is in the same group; same checks the commit gate re-enforces. **Only writer of `Line.original_checkout_user_id`.** |
| `PATCH` | `/api/kiosk/cart/lines/{id}` | none | Update qty and/or action on a line |
| `DELETE` | `/api/kiosk/cart/lines/{id}` | none | Remove a line |
| `GET` | `/api/kiosk/cart` | none | Refetch the cart by `?cart_id=`. The SPA hits this on every SSE tickle — payload pull, signal push. |
| `GET` | `/api/kiosk/cart/events` | none | Server-Sent Events stream for one cart. Query: `?cart_id=…`. Emits `cart.updated` on every cart write and `cart.gone` on commit/cancel. 15 s SSE-comment heartbeats keep proxies from closing idle connections. The SPA refetches via `GET /api/kiosk/cart` on every signal. |
| `POST` | `/api/kiosk/cart/cancel` | none | Discard an in-progress cart |
| `POST` | `/api/kiosk/cart/commit` | none | Promote cart to transaction + side effects + events |
| `POST` | `/api/kiosk/cart/rfid-scan` | none | **RFID counter_scan mode only.** Query: `?cart_id=…`. Runs one LLRP inventory cycle, resolves each observed EPC through the same scan path `cart/add` uses, and adds matched instances to the cart. Per-EPC failures (already in cart, inactive instance, unknown tag) are skip-and-logged so one bad tag doesn't fail the batch. Returns `{cart, added_lines, observed_epcs, unresolved_epcs}`. 503 when the reader connection is down; publishes `event.scan.rfid.observed` regardless of outcome. |
| `POST` | `/api/kiosk/cart/read-trigger` | none | **RFID enclosure_diff mode only.** Query: `?cart_id=…`. Runs one LLRP inventory cycle and reconciles observed EPCs against expected-present state via `rfid.Diff`, synthesizing checkout lines (expected, not observed) and self-return lines (held by cart user, observed). Cross-user returns are skip-and-counted. Returns `{cart, added_lines, observed_epcs, unresolved_epcs, skipped_cross_user_count}`. Also reachable as the NATS `read.trigger` command — this HTTP form is the manual-retry path on the outside-enclosure screen. |
| `GET` | `/api/kiosk/integrity` | admin | Diff expected vs actual `open_checkouts` |
| `POST` | `/api/kiosk/integrity/rebuild` | admin | Wipe `open_checkouts` and rebuild it from the ledger |
| `POST` | `/api/kiosk/ledger/republish` | admin | Re-emit transaction.complete + item.{action} events for completed transactions in an optional `{from, to}` ISO8601 window. Aggregator is idempotent so safe to re-run. |
| `POST` | `/api/kiosk/items/import` | admin | Multipart CSV upload, upsert by `code`. **Available on both binaries.** Body: `file` (CSV), `dry_run` (`true`/`false`). Returns `{dry_run, rows_total, rows_inserted, rows_updated, rows_errored, rows: [{row, code, name, action: "insert"\|"update"\|"error", errors?: [{code, message}]}]}` — one entry per input row, classified by diff against a snapshot of existing records. Dry-run is read-only and reports `insert`/`update` as the action that *would* happen. |
| `GET` | `/api/kiosk/items/import/template` | admin | Downloadable CSV template — same columns the importer accepts plus example rows. **Available on both binaries.** |
| `POST` | `/api/kiosk/users/import` | admin | Workers CSV importer. Same response shape as items. Unknown `group` codes are auto-created on real-run (not dry-run). **Available on both binaries.** |
| `GET` | `/api/kiosk/users/import/template` | admin | Workers CSV template. **Available on both binaries.** |
| `POST` | `/api/kiosk/groups/import` | admin | Groups CSV importer. Same response shape. **Available on both binaries.** |
| `GET` | `/api/kiosk/groups/import/template` | admin | Groups CSV template. **Available on both binaries.** |
| `POST` | `/api/kiosk/items/{id}/adjust` | admin | Change `quantity_on_hand` + write a `stock_adjustments` audit row in one transaction |
| `POST` | `/api/kiosk/checkouts/by-line/{transaction_line_id}/close` | admin | Admin force-close one `open_checkouts` row. Body: `{reason: "lost"\|"returned_offline"\|"damaged"\|"other", notes?}`. Writes one `transactions` row + one `transaction_lines` row (`action="admin_close"`) and deletes the open row, all in one DB transaction. `lost` / `damaged` also decrement `quantity_on_hand`, write a `stock_adjustments` row, and (for serialized items) flip `item_instances.active=false` plus an `instance_audit` decommission row. |
| `GET` | `/api/kiosk/transactions.csv` | admin | Export completed transactions as CSV (optional `from=` / `to=` ISO8601 query params) |
| `GET` | `/api/kiosk/items.csv` | admin | Export the items catalog in the same column shape the items importer accepts |
| `GET` | `/api/kiosk/reports/open-checkouts.csv` | admin | CSV companion to `reports/open-checkouts`; optional `?kiosk_code=` filter |
| `GET` | `/api/kiosk/reports/low-stock.csv` | admin | Kiosk-local low-stock CSV (active items at or below `reorder_threshold`). **Controller binary publishes its fleet variant at `/api/controller/reports/low-stock.csv`** |
| `GET` | `/api/kiosk/reports/group-activity.csv` | admin | Per-group transaction rollup as CSV. Query: `?from=` / `?to=` (YYYY-MM-DD), optional `?kiosk_code=` |
| `GET` | `/api/kiosk/reports/instance-lifecycle.csv` | admin | Instance lifecycle audit as CSV. Kiosk binary reads local `instance_audit`; controller reads `instance_lifecycle_audit`. Filters: `?from`, `?to` (YYYY-MM-DD), `?action`, `?source`, controller-only `?kiosk_code` |
| `GET` | `/api/kiosk/reports/notifications.csv` | admin | `notification_send_log` rows for the requested `?lookback_days` window (default 7, max 90) |
| `GET` | `/api/kiosk/catalog/integrity` | admin | **Controller only.** Diff catalog DB vs JetStream KV; returns `missing_in_kv` + `extra_in_kv` per bucket |
| `POST` | `/api/kiosk/catalog/reconcile` | admin | **Controller only.** Push DB → KV (always); delete orphaned KV keys when body `{delete_orphans: true}` |
| `GET` | `/api/controller/kiosks/heartbeats` | admin | **Controller only.** Returns `{controller_started_at, kiosks: {code: lastSeenISO}}` — the SPA polls every 10s to render online/stale/offline badges |
| `GET` | `/api/controller/kiosks/{code}/inventory` | admin | **Controller only.** Fires the `inventory.snapshot` command over NATS request/reply; returns the kiosk's live on-hand for every stocked item. 503 `{error: "kiosk_offline", kiosk_code}` when stale heartbeat or NATS timeout. |
| `POST` | `/api/controller/kiosks/{code}/inventory/adjust` | admin | **Controller only.** Server-generates a `command_id`, fires `inventory.adjust` to the kiosk over NATS request/reply. Body: `{item_code, mode, value, reason}`. Idempotent via `command_id`; 503 on offline. |
| `POST` | `/api/controller/kiosks/{code}/checkouts/{source_line_id}/close` | admin | **Controller only.** Forwards an admin force-close to a remote kiosk over NATS request/reply (`checkout.close` command). `source_line_id` is the kiosk-side `transaction_lines.id` from the projected ledger. Body: `{reason, notes?}`. Server-generates `command_id` for idempotent replay; 503 `{error: "kiosk_offline"}` when the kiosk is offline. Kiosk-side converges on `commit.AdminClose` so behavior is identical to a local close. |
| `GET` | `/api/controller/kiosks/{code}/instances` | admin | **Controller only.** Fires the `instance.snapshot` command. Optional `?item_code=` filters. Returns `{instances: [{instance_id, instance_code, item_code, item_name, serial, rfid_epc, active, notes, created, updated}]}`. 503 on offline. |
| `POST` | `/api/controller/kiosks/{code}/instances` | admin | **Controller only.** Fires `instance.create`. Body: `{item_code, code, serial?, rfid_epc?, notes?, active?}`. Idempotent via server-generated `command_id`. 503 on offline. |
| `PATCH` | `/api/controller/kiosks/{code}/instances/{instance_code}` | admin | **Controller only.** Fires `instance.edit` — cosmetic-only (no audit, no lifecycle event). Body: any subset of `{code, serial, rfid_epc, notes}`. 503 on offline. |
| `POST` | `/api/controller/kiosks/{code}/instances/{instance_code}/decommission` | admin | **Controller only.** Fires `instance.decommission`. Body: `{reason}` (required). Writes audit + emits `instance.lifecycle`. Idempotent via `command_id`; 503 on offline. |
| `POST` | `/api/controller/kiosks/{code}/instances/{instance_code}/reactivate` | admin | **Controller only.** Fires `instance.reactivate`. Same body + idempotency shape as decommission. |
| `POST` | `/api/controller/kiosks/{code}/integrity/rebuild` | admin | **Controller only.** Fires `integrity.rebuild` — wipes the kiosk&rsquo;s `open_checkouts` and rebuilds from its ledger. Idempotent on its own (replay produces same state). Empty body. 503 on offline. |
| `POST` | `/api/controller/kiosks/{code}/ledger/republish` | admin | **Controller only.** Fires `ledger.republish` — re-emits transaction.complete + item.{action} events for every completed transaction in the optional `{from, to}` window. The controller&rsquo;s projection dedupes on `source_line_id`, so duplicates are no-ops. 503 on offline. |
| `GET` | `/api/controller/reports/low-stock` | admin | **Controller only.** Fleet-wide low-stock report. Fans `inventory.snapshot` to every online managed kiosk in parallel, joins each kiosk's snapshot with `out` counts derived from the controller's projected ledger, and returns rows whose `available ≤ reorder_threshold`. Optional `?kiosk_code=` scopes to one kiosk. Response shape: `{rows: [...], errors: [{kiosk_code, error}]}` — offline kiosks appear in `errors` so partial results are explicit. |
| `GET` | `/api/controller/reports/low-stock.csv` | admin | **Controller only.** CSV companion to the fleet low-stock report. Same fan-out path; the CSV is data-only (offline-kiosk errors are not embedded — use the JSON endpoint for status). |
| `GET` | `/api/controller/reports/adjustment-audit.csv` | admin | **Controller only.** `inventory_audit` collection as CSV. Filters: `?from` / `?to` (YYYY-MM-DD), `?kiosk_code`, `?source` (`local` \| `controller`). |
| `GET` | `/api/controller/notifications` | admin | **Controller only.** List notification templates. Same DTO as the kiosk's `/api/kiosk/notifications`. |
| `PATCH` | `/api/controller/notifications/{event_type}` | admin | **Controller only.** Update subject/body/enabled/recipients on a template. |
| `GET` | `/api/controller/notifications/{event_type}/defaults` | admin | **Controller only.** Compiled-in defaults for "Reset to defaults" affordance. |
| `GET` | `/health` | none | Returns `{status: "ok"}` — for liveness probes |

Admin endpoints require a token from the `admins` auth collection in the
`Authorization` header (the PB SDK handles this automatically after
login).

The kiosk checkout endpoints are anonymous on the assumption that the
kiosk is physically secured and bound to `127.0.0.1`. Worker
identification happens at the application layer via badge scan — `code`
resolves to a `users` record.

## PocketBase collection rules

| Collection | List/View | Create/Update/Delete |
|---|---|---|
| `users` (workers) | admin | admin |
| `admins` | self only | superuser only (via `/_/`) |
| `items` | admin | admin |
| `item_instances` | admin | admin |
| `transactions` | admin | forbidden via API; written by commit hook only |
| `transaction_lines` | admin | forbidden via API |
| `open_checkouts` | admin | forbidden via API |
| `stock_adjustments` | admin | forbidden via API; written by `/items/{id}/adjust` only |
| `instance_audit` | admin | forbidden via API; written by the `item_instances` record hooks (and by `commit.AdminClose` for serialized retire-on-close) |
| `inventory_audit` | admin | **Controller only.** Forbidden via API; written by the aggregator from `inventory.adjust` events |
| `instance_lifecycle_audit` | admin | **Controller only.** Forbidden via API; written by the aggregator from `instance.lifecycle` events |

The kiosk checkout flow never touches PB's REST API. Every operation
goes through a custom `/api/kiosk/*` endpoint that runs in-process and
bypasses collection rules.

## Events, commands, heartbeats

`internal/events.Publish(subject, payload)` is called from the commit
hook for every state change. It always emits a structured `slog.Info`
line. When `nats.enabled=true`, it also publishes the JSON-encoded
payload to the same subject via a buffering NATS connection. Errors from
the NATS publish are logged at warn level; commit paths are never
blocked or failed by them.

The full subject namespace — every event, command, and heartbeat with
payload and reply shapes — is in [Wire reference](wire.md). The short
version:

- **Events:** `{prefix}.{kiosk_code}.event.<...>` — JetStream-bound,
  durable. The stream binds to `{prefix}.*.event.>` and nothing else.
- **Commands:** `{prefix}.{kiosk_code}.command.<name>` — core NATS
  request/reply, ≤5 s reply timeout. Reply envelope is
  `{success, error, data}`.
- **Heartbeats:** `{prefix}.{kiosk_code}.heartbeat` — core NATS
  publish, 45 s cadence, last-write-wins.

`{prefix}` is `"kiosk"` by default and configurable via
`nats.subject_prefix` (both kiosk and controller must agree). Override
only to avoid collisions on a shared NATS cluster where another
application already owns the `kiosk.>` subject space.
