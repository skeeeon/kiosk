# API reference

## Custom `/api/kiosk/*` endpoints

All custom endpoints serve the kiosk checkout flow or admin operations.
PB's `/api/collections/*` is used for PB-native CRUD.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/kiosk/identity` | none | Returns `{kiosk_code, location_code, branding, max_qty, managed}` — `managed` is true when this kiosk is opted into central control |
| `POST` | `/api/kiosk/scan` | none | Resolves a raw scan to `user`, `item`, or `unknown` |
| `POST` | `/api/kiosk/cart/start` | none | Returns existing or new cart for a user code |
| `POST` | `/api/kiosk/cart/add` | none | Appends or stacks a line; computes default action |
| `PATCH` | `/api/kiosk/cart/lines/{id}` | none | Update qty and/or action on a line |
| `DELETE` | `/api/kiosk/cart/lines/{id}` | none | Remove a line |
| `POST` | `/api/kiosk/cart/cancel` | none | Discard an in-progress cart |
| `POST` | `/api/kiosk/cart/commit` | none | Promote cart to transaction + side effects + events |
| `GET` | `/api/kiosk/integrity` | admin | Diff expected vs actual `open_checkouts` |
| `POST` | `/api/kiosk/integrity/rebuild` | admin | Wipe `open_checkouts` and rebuild it from the ledger |
| `POST` | `/api/kiosk/ledger/republish` | admin | Re-emit transaction.complete + item.{action} events for completed transactions in an optional `{from, to}` ISO8601 window. Aggregator is idempotent so safe to re-run. |
| `POST` | `/api/kiosk/items/import` | admin | Multipart CSV upload, upsert by `code` |
| `POST` | `/api/kiosk/items/{id}/adjust` | admin | Change `quantity_on_hand` + write a `stock_adjustments` audit row in one transaction |
| `GET` | `/api/kiosk/transactions.csv` | admin | Export completed transactions as CSV (optional `from=` / `to=` ISO8601 query params) |
| `GET` | `/api/kiosk/catalog/integrity` | admin | **Controller only.** Diff catalog DB vs JetStream KV; returns `missing_in_kv` + `extra_in_kv` per bucket |
| `POST` | `/api/kiosk/catalog/reconcile` | admin | **Controller only.** Push DB → KV (always); delete orphaned KV keys when body `{delete_orphans: true}` |
| `GET` | `/api/controller/kiosks/heartbeats` | admin | **Controller only.** Returns `{controller_started_at, kiosks: {code: lastSeenISO}}` — the SPA polls every 10s to render online/stale/offline badges |
| `GET` | `/api/controller/kiosks/{code}/inventory` | admin | **Controller only.** Fires the `inventory.snapshot` command over NATS request/reply; returns the kiosk's live on-hand for every stocked item. 503 `{error: "kiosk_offline", kiosk_code}` when stale heartbeat or NATS timeout. |
| `POST` | `/api/controller/kiosks/{code}/inventory/adjust` | admin | **Controller only.** Server-generates a `command_id`, fires `inventory.adjust` to the kiosk over NATS request/reply. Body: `{item_code, mode, value, reason}`. Idempotent via `command_id`; 503 on offline. |
| `GET` | `/api/controller/reports/low-stock` | admin | **Controller only.** Fleet-wide low-stock report. Fans `inventory.snapshot` to every online managed kiosk in parallel, joins each kiosk's snapshot with `out` counts derived from the controller's projected ledger, and returns rows whose `available ≤ reorder_threshold`. Optional `?kiosk_code=` scopes to one kiosk. Response shape: `{rows: [...], errors: [{kiosk_code, error}]}` — offline kiosks appear in `errors` so partial results are explicit. |
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
| `inventory_audit` | admin | **Controller only.** Forbidden via API; written by the aggregator from `inventory.adjust` events |

The kiosk checkout flow never touches PB's REST API. Every operation
goes through a custom `/api/kiosk/*` endpoint that runs in-process and
bypasses collection rules.

## Events

`internal/events.Publish(subject, payload)` is called from the commit
hook for every state change. It always emits a structured `slog.Info`
line. When `nats.enabled=true`, it also publishes the JSON-encoded
payload to the same subject via a buffering NATS connection. Errors from
the NATS publish are logged at warn level; commit paths are never
blocked or failed by them.

| Trigger | Subject |
|---|---|
| Transaction completed | `{prefix}.{kiosk_code}.transaction.complete` |
| Checkout line | `{prefix}.{kiosk_code}.item.checkout` |
| Return line | `{prefix}.{kiosk_code}.item.return` |
| Consume line | `{prefix}.{kiosk_code}.item.consume` |
| Admin stock adjustment | `{prefix}.{kiosk_code}.inventory.adjust` |
| Open-checkouts rebuild | `{prefix}.{kiosk_code}.integrity.rebuild` |
| Receipt context (managed mode only) | `{prefix}.{kiosk_code}.receipt.transaction` |
| Low-stock alert (managed mode only) | `{prefix}.{kiosk_code}.alert.lowstock` |
| Scheduled digest (managed mode only) | `{prefix}.{kiosk_code}.digest.open_checkouts` |

`{prefix}` is `"kiosk"` by default and configurable via
`nats.subject_prefix` (both kiosk and controller must agree). Override
only to avoid collisions on a shared NATS cluster where another
application already owns the `kiosk.>` subject space. Subscribe locally
with the `nats` CLI to confirm publishing:

```bash
nats sub "kiosk.>"
```

Two more NATS subject families exist alongside the events above but
don't ride JetStream:

| Subject | Direction | Transport |
|---|---|---|
| `{prefix}.{kiosk_code}.heartbeat` | kiosk → controller | Core NATS publish, 45s cadence. No persistence — last-write-wins is the entire signal. |
| `{prefix}.{kiosk_code}.command.<name>` | controller → kiosk | Core NATS request/reply, ≤5 s reply timeout. Today: `inventory.adjust`, `inventory.snapshot`. |

The `KIOSK_EVENTS` stream's `FilterSubjects` deliberately excludes both
— heartbeats and commands should never be replayed from a durable
stream.
