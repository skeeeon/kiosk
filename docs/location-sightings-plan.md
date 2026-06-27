# Location & Sightings — plan

Status: **scoped / not yet started** (2026-06-27). Living doc; update as phases land.
Sibling of [`docs/asset-tracker-plan.md`](asset-tracker-plan.md) (custody foundation),
which is done through Phase 5. This is the deferred **second half** of that vision:
coarse location / last-sighting of assets, and the reconciliation of custody vs location
that is the actual product value.

> **Model decision (2026-06-27):** gateways are **external publishers**, not a kiosk
> reader mode. The platform never operates a location reader — it only *consumes* a
> normalized sighting message off NATS. There is exactly **one** ingest interface (the
> `sighting` family), and resolution happens subscriber-side. See
> [The single ingest path](#the-single-ingest-path) — this replaces the earlier draft's
> embedded `location` reader mode.

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
- **Gateways live outside the platform; the platform only consumes.** A gateway (RFID antenna,
  BLE gateway, roaming GPS unit) is configured and operated in its own world — its `gateway_id`
  and `zone` are *its* config, stamped into every message it sends. The platform does not run an
  LLRP/BLE client for location, does not poll a reader, does not store a gateway config. It
  subscribes to a NATS subject. How a sighting reaches NATS — a reader speaking MQTT straight
  into NATS's MQTT interface, or an HTTP→NATS bridge — is the gateway's problem, invisible here.
- **One ingest interface.** Every sighting, from every source, is one **raw** message shape on
  one NATS family. There is no second "node-attached reader" path. This is the single biggest
  simplification over the earlier draft: one wire contract, one resolution step, source-agnostic
  (RFID today, BLE/GPS later differ only at resolution).
- **Heartbeat-family transport: lossy, last-write-wins, outside the durable stream.** Sightings
  are high-volume and we only ever want *latest*. They ride a new core-NATS family `sighting`
  (sibling of `event` / `command` / `heartbeat`), so they are outside the JetStream
  `*.event.>` stream **by construction** (the family segment is what determines transport —
  see `internal/events/subjects.go`). A dropped sighting self-heals on the next read. Publish
  via the raw conn + a `SightingSubject` helper — **never** `events.Publish` (it's not an
  event; the durable stream must not capture it, and the `kiosk.event` slog line would lie).
- **The node stays the only custody authority; location is a cross-cutting projection, not a
  new tier.** The controller aggregates site-wide location (it already aggregates everything)
  and mirrors each node's own assets' last-seen back down via a KV replica — the exact
  `catalog_items` slicing pattern. No new authority/sync tier.
- **N=1 invisible.** A single node with no gateways has zero location machinery: no sighting
  subscription, no `last_observed_*` writes (beyond the free custody-read stamp), no SPA column.
  Location is opt-in by topology, never a prerequisite.
- **Resolution is subscriber-side, never publisher-side.** A gateway emits the raw tag id it
  read — it does not know our instance codes. Whoever subscribes resolves it: a standalone node
  via the existing scan resolver (it holds the instances); the controller via an EPC index. The
  scan resolver **already** does EPC→instance (`scan.Lookups.ItemInstanceByRFID`,
  `internal/scan/scan.go`), so node-side resolution is reuse, not new code.
- **Grug.** Reuse the scan resolver, the KV-replica + watcher pattern, the plain-subscribe
  controller ingest, and the scheduler. Add the smallest new surface that delivers the
  reconciliation value. Defer roaming/GPS labeling, BLE, gateway registries, and geofence
  enforcement until a real deployment needs them.

## Concept model (additions to the asset-tracker model)

| Concept | What it is | Identity | Lives where |
|---|---|---|---|
| **Gateway** | An external publisher whose job is *passive observation for location* — an RFID antenna, BLE gateway, or roaming GPS unit. **Not** a kiosk reader mode. Configured & operated outside the platform; emits sightings to NATS. | `gateway_id` | the gateway's own config (off-platform) |
| **Zone** | A coarse logical location label a static gateway is configured with ("Yard", "Building B", "Cabinet A area"). Stamped inline into each sighting — **not** stored as platform config. | `zone` (free text) | gateway config, carried inline on the sighting |
| **Sighting** | One observation: a gateway saw a tag at a time, with a zone and/or GPS. Always **raw** (carries the tag id, never a resolved instance code). | — (ephemeral) | the `sighting` NATS family; never stored raw long-term |
| **Last-observed** | The latest sighting per instance (last-write-wins on `observed_at`). The materialized advisory view. | keyed by instance | `item_instances.last_observed_*` (node) + controller `instance_location` table + `last_observed_state` KV (per-node mirror) |

A sighting shape (the **only** wire contract):

```
{ tag_id, gateway_id, zone?, lat?, lon?, observed_at, rssi? }
```

`tag_id` is the raw EPC (RFID) or beacon id (BLE), resolved subscriber-side to an
`item_instances` row by `rfid_epc` (instance-only — EPCs live on instances, never SKUs, per the
scan resolver). `zone`/`lat`/`lon` are whatever the gateway was configured with. A custody read
(counter/enclosure) is *also* a sighting at that reader's zone — free location data from custody
activity (see [D8](#decisions) and L1).

**Naming note:** the instance fields are `last_observed_*`, NOT `last_seen_*`. The controller
once had a `kiosks.last_seen` ("last event of any kind", since replaced by
`last_transaction_at` and dropped) — avoid the collision.

## The single ingest path

This is the heart of the design. **One message shape, three subscription topologies.** They
differ only in *who subscribes and who resolves* — never in the wire contract.

| Deployment | Who publishes | Who subscribes | How it resolves `tag_id` | Transport |
|---|---|---|---|---|
| **N=1, no gateways** | nobody | nobody | n/a — custody reads stamp `last_observed_*` directly (D8) | none (no NATS needed) |
| **Standalone + NATS, with gateways** | external gateways → `<prefix>.<code>.sighting.raw` | the node, on **its own** subject only | locally, via the scan resolver (`ItemInstanceByRFID`) — the node holds the instances | core NATS pub/sub |
| **Controller-managed** | external gateways + each node's custody reads → `<prefix>.<code>.sighting.raw` | the **controller**, fleet-wide (`<prefix>.*.sighting.raw`) | via a controller **EPC index** (`epc → instance_code, kiosk_code`) | core NATS pub/sub |

Key consequences:

- **A standalone node never needs the EPC index** — it resolves its own gateways' sightings
  locally and writes its own columns. No controller, no KV, no index.
- **In managed mode, kiosks subscribe to nothing on the ingest side.** Only the controller
  absorbs the sighting stream. Kiosks learn location *back* via the mirror-down KV (below), and
  each watches **only its own slice**. No kiosk ever processes another site's sightings.
- **Custody reads are sightings too, uniformly.** When an RFID custody read
  (`counter_scan` / `enclosure_diff`) resolves instances, the node (a) stamps its own
  `last_observed_*` locally at the reader's configured `zone` (works with zero NATS), and (b) in
  managed mode publishes the same raw sighting onto the family so the controller sees it. Same
  shape, same path — a custody reader with a `zone` set is just another sighting source. A
  barcode-only counter (no EPC) produces no sighting; that's fine.
- **Why always raw (never publisher-resolved):** keeping one shape means one resolution code
  path and a source-agnostic wire. The controller's EPC-index lookup is an O(1) map hit, so the
  "waste" of re-resolving a custody read the node already knew is negligible — and it keeps
  external gateways (which *can't* resolve) and custody reads on identical rails. A new
  instance whose EPC the index hasn't learned yet is a rare miss that self-heals on the next
  read (sightings are lossy by design).

## Scale & fan-out

Worth making explicit because a "gateways publish to NATS" design quietly fails at scale if the
subjects fan out wrong. Target: ~100 sites × ~200 instances each × several gateways per site.

**Two channels, opposite shapes:**

1. **Ingest (gateway → controller) is fan-IN to one subscriber.** Gateways publish to
   `<prefix>.<site>.sighting.raw`; in managed mode **only the controller** subscribes
   (`<prefix>.*.sighting.raw`, plain core NATS — mirror `internal/controller/heartbeats.go`,
   **not** the durable `consumer.go`). Kiosks subscribe to nothing here. This is consistent with
   the controller already being the sole aggregator of every family.

2. **Mirror-down (controller → owning node) is fan-OUT, but sliced.** The controller writes each
   unit's last-observed to the `last_observed_state` KV bucket keyed `<kiosk_code>.<instance_code>`;
   each node hydrates **only its own slice** via `Watch(<kiosk_code>.>)` — the **`catalog_items`**
   pattern (server-side prefix filtering), **not** the `punch_state`/`open_checkouts_state`
   `WatchAll` pattern (those are org-wide because they're keyed by `user_code`). A node only ever
   needs its own instances' locations (wherever seen), so the slice is both sufficient and
   complete. **This is the one correction over the earlier draft, which called the mirror a
   "sibling of the punch-state watcher" — it must be a sibling of the catalog-items watcher.**

**Why the volume is fine:**

- **Wire is a non-issue.** Worst case (dumb gateways re-publishing every read cycle) is on the
  order of thousands of sightings/sec fleet-wide — NATS core doesn't notice.
- **Writes track *movement*, not reads** — the load-bearing optimization. The controller keeps an
  in-memory `map[tag_id] → last-observed` (~20k entries fleet-wide, trivial). A sighting whose
  `(zone | gps-bucket)` matches what the map already holds is **dropped before any DB or KV
  write**. So a tool sitting in Cabinet A generates traffic but zero writes. DB/KV churn is
  proportional to real-world movement (a trickle on a jobsite), not to read cadence.
- **Storage is bounded.** `instance_location` is **latest-only — one row per instance** (~20k
  fleet-wide), not a sighting log. The in-memory dedup map is bounded by distinct tags. We keep
  no history (see Deferred).
- **Each node's watcher fires only for its own instances that actually moved** — ~200 keys
  watched, updated only on real zone changes.

**Deferred levers (shaped-for, NOT built — YAGNI):** if a much larger fleet ever outran a single
controller subscription, the per-site subject lets the ingest go behind a **NATS queue group** or
shard by site — an ops change, no schema change. A chatty gateway can be coalesced at the
MQTT/HTTP→NATS edge. Neither is worth building now; the subject choice merely leaves the door open.

## Phasing

Each phase is independently shippable and additive. L1 stands alone with zero transport. L2 adds
the gateway interface (standalone-capable). L3 adds the fleet. L4 is the value.

### Phase L1 — Node-local last-observed *(zero transport; the foundation)*

- **Schema (kiosk):** add advisory columns to `item_instances` — `last_observed_at` (date),
  `last_observed_zone` (text), `last_observed_gateway` (text), `last_observed_lat` /
  `last_observed_lon` (number, nullable). Kiosk-local fields, **not** touched by catalog resync
  (same exclusion as `quantity_on_hand` / `reorder_threshold`). Nullable; empty until something
  reports. One migration, idempotent.
- **Free sightings from custody reads:** give a custody reader an optional `zone` in its existing
  `rfid.readers` config entry. When a `counter_scan` / `enclosure_diff` read resolves instances,
  also **monotonically** stamp `last_observed_*` for each at that reader's `zone` (only advance on
  a newer `observed_at`). One extra call in the existing read paths; no new I/O, no NATS, no
  gateway. Pure, no ledger, no commit involvement.
- **SPA:** a "Last seen" column on `ItemInstancesPanel` (zone + relative time), shown only when
  any instance has a non-empty `last_observed_at` — invisible at N=1.
- **N=1 / no readers with a zone:** nothing writes `last_observed_*` → no column. Unchanged.
- **NOT built here:** no `sighting` NATS family, no external gateways, no controller, no GPS,
  no cross-node. A node sees only what its own custody readers observe.

### Phase L2 — Sighting ingest: the gateway wire contract *(standalone + NATS)*

- **Transport:** new family `sighting` — `<prefix>.<node_code>.sighting.raw`, built via a new
  `events.SightingSubject` / `SightingFilter` helper (single source of truth, per the existing
  discipline). **Core NATS pub/sub, lossy, NOT on the JetStream stream**; published via the raw
  conn, never `events.Publish`.
- **Wire shape:** the raw sighting `{ tag_id, gateway_id, zone?, lat?, lon?, observed_at, rssi? }`
  — the only contract. GPS `lat`/`lon` are in the shape from day one, so a roaming gateway is just
  a gateway that fills them and omits `zone`; no extra phase needed for "GPS support" at the wire.
- **Node ingest (standalone):** the node subscribes to **its own** `<prefix>.<code>.sighting.raw`,
  resolves each `tag_id` locally via the scan resolver (`ItemInstanceByRFID` — already exists),
  and **monotonically** upserts `last_observed_*`. Reuses L1's upsert.
- **Gateways plug in here.** Any external publisher (RFID-over-MQTT direct to NATS, or via the
  HTTP→NATS bridge) configured with the site's `code`, a `gateway_id`, and a `zone` starts feeding
  location. The platform adds no gateway config.
- **N=1 / no controller:** a standalone node with gateways runs in standalone + NATS mode (an
  already-supported mode) and resolves locally. A node with no gateways stays exactly as L1 (no
  subscription). KV/controller untouched.

### Phase L3 — Controller aggregation + fleet mirror *(site-wide last-seen + scale)*

- **Controller EPC index (controller-only migration):** a small `instance_epc_index` table keyed
  unique on `tag_id`/`rfid_epc`, carrying `instance_code` + `kiosk_code`. Populated by threading
  `rfid_epc` onto the existing `instance.lifecycle` event (which the controller already projects)
  and upserting in that projection. Bounded (one row per instance), persistent across restart.
  This is the irreducible cost of "any gateway can see any tag" — the controller must learn which
  node owns an EPC. A standalone node needs none of this (L2 resolves locally).
- **Controller schema:** `instance_location` table keyed unique on `(kiosk_code, instance_code)`,
  columns mirroring `last_observed_*`. The site-wide advisory view, upserted **monotonically** on
  `observed_at`. Latest-only — never a log.
- **Controller ingest:** subscribe to `<prefix>.*.sighting.raw` **plain** (like heartbeats, NOT
  the durable aggregator — mirror `heartbeats.go`). Resolve `tag_id` via the EPC index; drop via
  the in-memory dedup map if `(zone|gps-bucket)` is unchanged (the Scale section); else upsert
  `instance_location` and write the unit's last-observed to a `last_observed_state` KV bucket
  keyed `<kiosk_code>.<instance_code>` (advisory, best-effort, never blocks the ingest — same
  posture as `punch_state` / `open_checkouts_state`).
- **Node publishes custody-read sightings (managed mode):** in managed mode the node also
  publishes its custody-read sightings (L1) onto the family so the controller sees custody-derived
  location. Same raw shape; the controller resolves + mirrors it back (idempotent monotonic
  upsert against what the node already wrote locally — harmless).
- **Node mirror:** a `last_observed` watcher that hydrates **only its own slice** —
  `Watch(<kiosk_code>.>)`, the **`catalog_items`** pattern (NOT `WatchAll`) — into the node's
  `last_observed_*` columns, so a node's instance seen by *another* node's gateway still shows its
  true last-seen locally. WatchAll-of-its-own-prefix on start recovers after restart. Advisory: KV
  down → local-gateway data only, self-heals.
- **Controller SPA:** `KioskInstancesPanel` gains the same "Last seen" column (the snapshot
  enrichment already passes unknown fields through; thread `last_observed_*` like `enclosure_id`).

### Phase L4 — Reconciliation *(the value) + BLE*

- **Reconciliation view/report:** join custody (`ledger.ReplayOpenRows` — who has what) with
  location (`instance_location` — where last seen) and flag:
  - out to worker W but last-observed in a custody zone (cabinet/counter) → likely not taken;
  - out (in custody) but `last_observed_at` is stale beyond a threshold → possibly lost;
  - **not** in custody but observed moving / off-site → unaccounted movement;
  - GPS outside a configured site polygon → theft signal.
  Pure read/derive — **no enforcement, no geofence hard-stops** (observability only, same stance
  as no-billing-math). Delivered as (a) a scheduled `digest.reconciliation` report via the
  existing scheduler fan-out (`report_key` + template, runs on whichever binary owns the schedule
  — controller fleet-wide, standalone node-local) and (b) an admin SPA view (controller:
  site-wide; node: its own instances).
- **BLE as an alternate sighting source:** a BLE gateway publishes the **same** sighting shape
  with `tag_id` = a BLE beacon id, resolved against a new `item_instances.ble_id` (parallel to
  `rfid_epc`, added to the scan resolver chain). Everything downstream (ingest, dedup, projection,
  KV mirror, reconciliation) is **source-agnostic** — RFID vs BLE differs only at resolution.
  **Shipped:** the `ble_id` column + the node-side resolver chain (`ItemInstanceByBLE`), so a
  **standalone** node with a BLE gateway resolves BLE sightings locally today (the node subscriber
  tries EPC then BLE). **Deferred** (see below): the controller-side EPC index only resolves
  `rfid_epc`, so **fleet** BLE aggregation (a BLE gateway feeding the controller) isn't wired yet —
  it needs `ble_id` threaded onto `instance.lifecycle` + a second resolution key in
  `instance_epc_index`. Small, well-understood follow-on; not built until a managed BLE deployment
  needs it.

## Deferred (shaped-for, not in this plan)

- **Controller-side fleet BLE resolution.** The node-side BLE resolver ships (standalone BLE
  works); the controller's `instance_epc_index` resolves `rfid_epc` only. Threading `ble_id` onto
  `instance.lifecycle` + a second resolution key would let a BLE gateway feed the controller.
- **Gateway registry (controller `gateways` table).** v1 has each gateway stamp its `zone`/`id`
  inline (gateway config lives off-platform). A central registry — for gateways too dumb to
  self-label, or to manage a static gateway's zone centrally — is a later add-on; the ingest
  consumes an inline zone today and would fall back to a registry lookup only if absent.
- **Reverse-zone mapping for GPS** (point-in-polygon → named zone). Analytics, not core; `lat`/`lon`
  are stored raw and good enough for the off-site reconciliation flag.
- **Geofence enforcement / real-time theft alarms.** v1 surfaces reconciliation discrepancies; it
  does not *act* on them. Active alerting (push/SMS on off-site GPS) is a later layer on the same
  data.
- **Sighting history / dwell analytics.** We keep only *latest* (last-observed). A durable sighting
  log for "time in zone" / movement trails is a separate, opt-in, durable-stream decision —
  explicitly **not** the lossy last-write-wins path here.
- **Indoor fine-grained positioning** (multilateration, RSSI trilateration). Out of scope — the
  product is *coarse* zone/last-seen, not RTLS.
- **Ingest sharding / queue groups.** Single controller subscription is ample at target scale;
  the per-site subject leaves room to shard later with no schema change (see Scale).

## Decisions

| # | Decision | Resolution |
|---|---|---|
| L-D1 | Is location authoritative? | **No.** Advisory, lossy, last-write-wins. Never gates custody; commit never reads it. |
| L-D2 | Transport | New core-NATS family `sighting` (sibling of event/command/heartbeat) — outside the durable JetStream stream by construction. Pub/sub, not durable. Publish via raw conn, never `events.Publish`. |
| L-D3 | Are gateways a kiosk reader mode? | **No.** Gateways are external publishers, configured & operated off-platform; the platform only consumes the `sighting` family. No `location` reader mode, no LLRP/BLE client for location in the binary. |
| L-D4 | How many ingest paths? | **One.** A single raw message shape on one family, from every source (external gateway *and* custody-read byproduct). |
| L-D5 | Where is `tag_id` resolved? | **Subscriber-side, always.** Standalone node → local scan resolver (`ItemInstanceByRFID`). Controller → `instance_epc_index` (fed by `rfid_epc` threaded onto `instance.lifecycle`). Publishers never resolve. |
| L-D6 | Where does last-observed live? | Node: advisory `item_instances.last_observed_*` columns (catalog-resync-excluded). Controller: `instance_location` table (site-wide, latest-only). Mirror: `last_observed_state` KV. |
| L-D7 | Mirror-down fan-out | KV keyed `<kiosk_code>.<instance_code>`; each node `Watch(<kiosk_code>.>)` — the **`catalog_items`** slicing pattern, **not** `WatchAll`. No node sees another site's data. |
| L-D8 | Custody reads as sightings | Yes — a custody reader with a configured `zone` stamps last-observed locally (zero transport) and, in managed mode, publishes the same raw sighting for the controller. |
| L-D9 | Naming | `last_observed_*` (not `last_seen_*` — avoids the dropped `kiosks.last_seen` collision). |
| L-D10 | Reconciliation posture | Observability only — surface discrepancies (view + digest); no geofence enforcement / hard-stops in v1. |
| L-D11 | BLE | Same sighting shape; differs only at resolution (`item_instances.ble_id` + resolver chain + EPC index). Deferred to L4. |
| L-D12 | GPS / roaming | No dedicated phase — `lat`/`lon` are in the wire shape from L2; a roaming gateway just fills them. Reverse-zone mapping deferred. |

## What this reuses (no new tiers)

- **Scan resolver** (`ItemInstanceByRFID`, EPC instance-only) → node-side sighting resolution;
  `ble_id` extends the same chain. The node already resolves EPC→instance without operating any
  reader — this is the fact that makes the external-gateway model free.
- **`events/subjects.go` family discipline** → the `sighting` family + helpers.
- **Plain-subscribe controller ingest** (`heartbeats.go`, not the durable `consumer.go`).
- **KV replica + watcher pattern** → `last_observed_state`, sliced like **`catalog_items`**
  (`Watch(<code>.>)`), not WatchAll.
- **`instance.lifecycle` event** (already projected) → carries `rfid_epc` to build the controller
  EPC index.
- **Custody read paths** (`counter_scan` / `enclosure_diff`) → free sightings at the reader's zone.
- **Scheduler fan-out** (`report_key` + templates) → the reconciliation digest.
- **Snapshot enrichment pass-through** → surfacing `last_observed_*` in the controller panels.
