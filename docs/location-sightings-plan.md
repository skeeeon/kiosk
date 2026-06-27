# Location & Sightings — plan

Status: **scoped / not yet started** (2026-06-27). Living doc; update as phases land.
Sibling of [`docs/asset-tracker-plan.md`](asset-tracker-plan.md) (custody foundation),
which is done through Phase 5. This is the deferred **second half** of that vision:
coarse location / last-sighting of assets, and the reconciliation of custody vs location
that is the actual product value.

## North-star

A construction-jobsite asset platform knows two orthogonal things about each serialized
unit:

1. **Custody** — who has it, from the append-only ledger. Authoritative, local-first, already
   built (checkout / return / foreman-on-behalf / fleet clock-out gate).
2. **Location** — *where it was last seen*, from RFID/BLE reads by gateways around the site
   (static gateway → a coarse **zone**; roaming gateway → GPS lat/lon). Advisory, lossy,
   never authoritative.

The product value is the **reconciliation** of the two: "checked out to Bob but last seen
still in Cabinet A" (maybe never actually taken), "in custody but not seen in 3 days" (maybe
lost), "no custody record but seen leaving the yard" (unaccounted movement / theft signal).
Custody tells you the paperwork; location tells you the ground truth; the gap between them is
the alert.

## Principles (load-bearing — inherited, do not violate)

- **Location is advisory, NEVER the ledger.** A sighting can never gate, block, or mutate a
  custody transaction. The commit path does not read location. Worst case for a wrong/missing
  sighting is a stale "last seen" cell — never a failed checkout. (Same stance as the
  fleet-replica clock-out gate being fail-open, and as "rent" carrying no billing math.)
- **Heartbeat-family transport: lossy, last-write-wins, outside the durable stream.** Sightings
  are high-volume and we only ever want *latest*. They ride a new core-NATS family `sighting`
  (sibling of `event` / `command` / `heartbeat`), so they are outside the JetStream
  `*.event.>` stream **by construction** (the family segment is what determines transport —
  see `internal/events/subjects.go`). A dropped sighting self-heals on the next read. Do not
  put sightings on the durable aggregator.
- **The node stays the only custody authority; location is a cross-cutting projection, not a
  new tier.** The controller aggregates site-wide location (it already aggregates everything)
  and mirrors each node's own assets' last-seen back down via a KV replica — the exact
  `punch_state` / `open_checkouts_state` pattern. No new authority/sync tier.
- **N=1 invisible.** A single node with no gateways configured has zero location machinery: no
  `location` readers → no sightings → no `last_observed_*` → the SPA shows no location column.
  Location is opt-in by topology, never a prerequisite.
- **Controller parity.** Every location capability is reachable from the controller (site-wide
  view + reconciliation digest), leaning on existing plumbing (KV replicas, the durable
  aggregator only where a durable join is needed, the scheduler fan-out for digests).
- **Grug.** Reuse the reader infrastructure, the scan resolver (EPC is instance-only), the KV
  replica + watcher pattern, and the scheduler. Add the smallest new surface that delivers the
  reconciliation value. Defer roaming/GPS, BLE, and geofence-enforcement until a real
  deployment needs them.

## Concept model (additions to the asset-tracker model)

| Concept | What it is | Identity | Lives where |
|---|---|---|---|
| **Gateway** | A reader/antenna whose job is *passive observation for location*, not custody. Distinct role from a custody reader (`counter_scan` / `enclosure_diff`). May be node-attached or standalone/roaming. | `gateway_id` | node config (node-attached) or a controller gateway registry (standalone) |
| **Zone** | A coarse logical location label a static gateway is configured with ("Yard", "Building B", "Cabinet A area"). | `zone` (free text) | gateway config |
| **Sighting** | One observation: a gateway saw a tag at a time, with a zone and/or GPS. | — (ephemeral) | the `sighting` NATS family; never stored raw long-term |
| **Last-observed** | The latest sighting per instance (last-write-wins on `observed_at`). The materialized advisory view. | keyed by instance | `item_instances.last_observed_*` (node) + controller `instance_location` table + `last_observed_state` KV (mirror) |

A sighting shape: `{tag_id, gateway_id, zone?, lat?, lon?, observed_at, rssi?}`. `tag_id`
resolves to an `item_instances` row by `rfid_epc` (instance-only — EPCs live on instances,
never SKUs, per the scan resolver). A custody read (counter/enclosure) is *also* a sighting at
that reader's zone — free location data from custody activity (see L1).

**Naming note:** the instance fields are `last_observed_*`, NOT `last_seen_*`. The controller
once had a `kiosks.last_seen` ("last event of any kind", since replaced by
`last_transaction_at` and dropped) — avoid the collision.

## Phasing

Each phase is independently shippable and additive. L1 stands alone (single node + its own
gateways, no controller). L2–L4 layer on the fleet + the value.

### Phase L1 — Node-local sightings *(standalone-capable; the foundation)*

- **Schema (kiosk):** add advisory columns to `item_instances` — `last_observed_at` (date),
  `last_observed_zone` (text), `last_observed_gateway` (text), `last_observed_lat` /
  `last_observed_lon` (number, nullable). Kiosk-local fields, **not** touched by catalog
  resync (same exclusion as `quantity_on_hand` / `reorder_threshold`). Nullable; empty until a
  gateway reports. One migration, idempotent.
- **Config:** a third reader **mode** `location` in the existing `rfid.readers` map, carrying a
  `zone`. A `location` reader runs a periodic passive read loop (interval configurable, shared
  default) and resolves each EPC against `item_instances` locally. Reuses the per-reader LLRP
  client + `ReaderHandle` machinery from asset-tracker Phase 2 — mode is already per-reader.
- **Write path:** a node-local sightings handler resolves EPC → instance and **monotonically**
  upserts the `last_observed_*` columns (only advance on a newer `observed_at`). Pure, no
  ledger, no commit involvement.
- **Free sightings from custody reads:** when a `counter_scan` / `enclosure_diff` read
  completes, also stamp `last_observed_*` for every resolved instance at that reader's zone
  (the tool was demonstrably *there*). One extra call in the existing read paths; no new I/O.
- **SPA:** a "Last seen" column on `ItemInstancesPanel` (zone + relative time), shown only when
  any instance has a non-empty `last_observed_at` — invisible at N=1.
- **N=1:** no `location` reader and no custody-read zones configured → nothing writes
  `last_observed_*` → no column. Unchanged.
- **NOT built here:** no NATS sighting family yet, no controller, no GPS, no cross-node. A
  standalone node sees only what its own gateways observe.

### Phase L2 — Sighting event + controller aggregation *(fleet last-seen)*

- **Transport:** new family `sighting` — `<prefix>.<node_code>.sighting.observed`, built via a
  new `events.SightingSubject` / `SightingFilter` helper (single source of truth, per the
  existing discipline). **Core NATS pub/sub, lossy, NOT on the JetStream stream.** The node
  emits a sighting carrying the **already-resolved** `instance_code` + `kiosk_code` + zone/gps
  + `observed_at` (not just the raw EPC), so the controller aggregates without re-resolving.
- **Controller schema (controller-only migration):** `instance_location` table keyed unique on
  `(kiosk_code, instance_code)`, columns mirroring `last_observed_*`. The site-wide advisory
  location view, upserted **monotonically** on `observed_at`.
- **Controller ingest:** subscribe to `sighting.observed` **plain** (like heartbeats, NOT
  through the durable aggregator — mirror `internal/controller/heartbeats.go`, not
  `consumer.go`). Upsert `instance_location`; then write the unit's last-observed to a
  `last_observed_state` KV bucket keyed by `<kiosk_code>.<instance_code>` (advisory,
  best-effort, never blocks — same posture as `punch_state` / `open_checkouts_state`).
- **Node mirror:** a `last_observed` watcher (sibling of the punch-state / checkout-state
  watchers) hydrates the KV bucket into the node's `last_observed_*` columns, so a node's
  instance seen by *another* node's gateway still shows its true last-seen locally. WatchAll on
  start recovers after restart. Advisory: KV down → local-gateway data only, self-heals.
- **Controller SPA:** `KioskInstancesPanel` gains the same "Last seen" column (the snapshot
  enrichment already passes unknown fields through; thread `last_observed_*` like
  `enclosure_id`). A site-wide "Where is everything" view can come here or in L4.
- **N=1 / standalone:** unchanged — a node with no controller writes its own columns directly
  (L1) and never emits/consumes the family.

### Phase L3 — Standalone & roaming gateways + GPS *(site-wide coverage)*

- **Standalone gateways** (not wired to a node — site-fixed gateways or a roaming truck)
  publish **raw** sightings `{tag_id, gateway_id, zone?, lat?, lon?, observed_at}` to a site
  subject (`<prefix>._site.sighting.raw` or per-gateway). The controller must now resolve
  EPC → instance + owning node itself, which requires a controller-side **EPC index**: thread
  `rfid_epc` onto the `instance.lifecycle` event (or a lightweight controller instance
  catalog) so the controller learns `epc → (instance_code, kiosk_code)`. Resolve, then feed the
  same `instance_location` upsert + KV mirror as L2.
- **Roaming / GPS gateway:** a gateway that moves; each sighting carries `lat`/`lon` and no
  fixed zone. `last_observed_lat`/`lon` already exist (L1). Optional reverse-zone mapping
  (point-in-polygon → named zone) is analytics, not core.
- **Gateway registry (controller):** `gateways` table mapping `gateway_id → {zone, roaming,
  active}` so a static gateway's zone is config-managed centrally and a raw sighting without an
  inline zone can still be labeled.
- **NOT built unless needed:** authenticated gateway enrollment, gateway heartbeats/health.
  Treat absent gateways like an offline reader — silent, self-healing.

### Phase L4 — Reconciliation *(the value) + BLE*

- **Reconciliation view/report:** join custody (`ledger.ReplayOpenRows` — who has what) with
  location (`instance_location` — where last seen) and flag:
  - out to worker W but last-observed in a custody zone (cabinet/counter) → likely not taken;
  - out (in custody) but `last_observed_at` is stale beyond a threshold → possibly lost;
  - **not** in custody but observed moving / off-site → unaccounted movement;
  - GPS outside a configured site polygon → theft signal.
  Pure read/derive — **no enforcement, no geofence hard-stops** (observability only, same
  stance as no-billing-math). Delivered as (a) a scheduled `digest.reconciliation` report via
  the existing scheduler fan-out (`report_key` + template, runs on whichever binary owns the
  schedule — controller fleet-wide, standalone node-local) and (b) an admin SPA view
  (controller: site-wide; node: its own instances).
- **BLE as an alternate sighting source:** a BLE gateway publishes the same sighting shape with
  `tag_id` = a BLE beacon id resolved against a new `item_instances.ble_id` (parallel to
  `rfid_epc`, added to the scan resolver chain). Everything downstream (projection, KV mirror,
  reconciliation) is **source-agnostic** — RFID vs BLE only differs at resolution.

## Deferred (shaped-for, not in this plan)

- **Geofence enforcement / real-time theft alarms.** v1 surfaces reconciliation discrepancies;
  it does not *act* on them. Active alerting (push/SMS on off-site GPS) is a later layer on the
  same data.
- **Sighting history / dwell analytics.** We keep only *latest* (last-observed). A durable
  sighting log for "time in zone" / movement trails is a separate, opt-in, durable-stream
  decision — explicitly **not** the lossy last-write-wins path here.
- **Indoor fine-grained positioning** (multilateration, RSSI trilateration). Out of scope —
  the product is *coarse* zone/last-seen, not RTLS.

## Decisions

| # | Decision | Resolution |
|---|---|---|
| L-D1 | Is location authoritative? | **No.** Advisory, lossy, last-write-wins. Never gates custody; commit never reads it. |
| L-D2 | Transport | New core-NATS family `sighting` (sibling of event/command/heartbeat) — outside the durable JetStream stream by construction. Pub/sub, not durable. |
| L-D3 | Where does last-observed live? | Node: advisory `item_instances.last_observed_*` columns (catalog-resync-excluded). Controller: `instance_location` table (site-wide aggregate). Mirror: `last_observed_state` KV (punch_state pattern). |
| L-D4 | Gateway vs reader | A gateway is a reader **role** — a third `location` mode in the existing `rfid.readers` map for node-attached; a controller `gateways` registry for standalone/roaming. |
| L-D5 | EPC resolution site | Node-attached: resolved node-side (node has the instances). Standalone/roaming: controller-side via an EPC index threaded onto `instance.lifecycle` (L3). |
| L-D6 | Naming | `last_observed_*` (not `last_seen_*` — avoids the dropped `kiosks.last_seen` collision). |
| L-D7 | Reconciliation posture | Observability only — surface discrepancies (view + digest); no geofence enforcement / hard-stops in v1. |
| L-D8 | Custody reads as sightings | Yes — a counter/enclosure read also stamps last-observed at the reader's zone (free location signal). |
| L-D9 | BLE | Same sighting shape; differs only at resolution (`item_instances.ble_id` + resolver chain). Deferred to L4. |

## What this reuses (no new tiers)

- **Reader / `ReaderHandle` map** (asset-tracker Phase 2) → the `location` reader mode.
- **`events/subjects.go` family discipline** → the `sighting` family + helpers.
- **KV replica + watcher pattern** (`punch_state`, `open_checkouts_state`) → `last_observed_state`.
- **Plain-subscribe controller ingest** (`heartbeats.go`, not the durable `consumer.go`).
- **Scan resolver** (EPC instance-only) → sighting resolution; `ble_id` extends the same chain.
- **Scheduler fan-out** (`report_key` + templates) → the reconciliation digest.
- **Snapshot enrichment pass-through** → surfacing `last_observed_*` in the controller panels.
