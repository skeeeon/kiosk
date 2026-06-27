# Asset-tracker generalization — plan

Status: **in design / not yet started** (2026-06-27). Living doc; update as phases land.

## North-star

A **site-scoped tool & asset platform for construction jobsites — custody *plus* coarse
location — federated under a central controller.** A rental company or GC runs it;
subcontractors check out/rent tools and consume items at per-building nodes, while
RFID/BLE reads (static gateways, or a roaming GPS-stamped gateway) give the last-known
coarse location of every tracked asset. Custody answers *who's responsible*; location
answers *where is it*; the diff between them is the product.

This plan covers the **foundation refactor** that makes a kiosk grow from a single
enclosure into a site (one node, many readers/enclosures/terminals) and/or be managed by a
controller. Location/sightings ride on top later (see Deferred).

## Principles (load-bearing)

1. **N=1 is invisible.** A single kiosk needs zero new ceremony. One config shape
   (`readers:` map), but with exactly one reader, selection is implicit — no
   `terminal`/`reader` URL param required. No `enclosure_id` → the node is one counter
   pool (today's behavior). No controller → standalone. All multi-* / site / controller
   machinery is opt-in by topology, never a prerequisite.
2. **Controller feature parity, observe-first.** Every new config/behavior is reachable
   from the controller. Lean on existing plumbing: `terminal_id`/`enclosure_id` project to
   the controller's `transactions`; per-instance `enclosure_id` rides the existing
   `instance.*` command family + `KioskInstancesPanel`. Node *config*
   (readers/enclosures/terminals) gets a read-only `config.snapshot` command + a detail-page
   tab for **observability** parity; **remote config editing is deferred** (config is edited
   locally and changes rarely).
3. **Grug.** One config shape (defaults collapse it at N=1). `rfid.Diff` stays a pure
   function. No "enclosure node" half-tier — an enclosure is an *attribute/partition inside*
   a node, not a sync unit. No `doors` table — a "door" is emergent (a terminal + reader(s)
   tagged to the same enclosure).
4. **The node is the only local-first custody authority.** The controller is a pure
   aggregator/hub that owns nothing authoritative. No new sync/authority tier. A kiosk
   grows either by gaining peripherals (topology A) or by federating under a controller
   (topology B); both reuse the same node abstraction.

## Concept model

| Concept | What it is | Identity | Where it lives |
|---|---|---|---|
| **Node** | Custody authority + DB (today's "kiosk"). Local-first. | `kiosk_code` (unchanged) | one PB/SQLite |
| **Terminal** | Interaction + **custody-acceptance** point (a screen). An enclosure has one per door. | `terminal_id` | URL param; node config binds terminal→enclosure/reader/mode |
| **Reader** | Physical LLRP/BLE reader device. | `reader_id` | node config `readers:` map |
| **Enclosure** | Access-controlled smart cabinet = inventory partition. Has 1..N doors (each a terminal); covered by 1..N readers. | `enclosure_id` | node config `enclosures:` map + `item_instances.enclosure_id` |
| **Site** *(future)* | Pure grouping label for nodes/zones. Owns nothing. | `site` | field/label |
| **Zone / Gateway** *(future)* | Location sensor + coarse area. | — | sighting stream |

**Access control + custody acceptance.** Access control is the external trigger for
`enclosure_diff`: it publishes `cart.start {user_code, enclosure_id}` and `read.trigger` on
our command bus. `enclosure_diff` is **propose-then-accept, not auto-commit** — the read
produces a *proposed* cart; the worker reviews and **accepts** it on the door's terminal,
and that accept is the custody handoff. Custody comes from the badged worker identity + the
explicit accept; `terminal_id` is attribution-only, never an auth boundary. A cabinet has a
terminal per door — it is **not** headless. The access boundary, inventory partition,
cart-session key, and diff scope coincide on the **enclosure**; the **terminal**
additionally records which door the worker accepted at. The access controller is an external
publisher — we don't model locks; lock→enclosure / badge→worker resolution lives in an
adapter (or node-side via the scan resolver) at integration time.

## Phasing

Isolated commits on one branch; each keeps `go test ./...` green and leaves single-kiosk
behavior identical. Land Phases 1–2 early (behavior-preserving); Phases 3–4 are the
capability and land together. Not one mega-diff.

### Phase 1 — Decompose `door_id` → `terminal_id` + `enclosure_id`  *(behavior-preserving)*

`door_id` does two unrelated jobs; split along the existing seam.

**Bucket A → `terminal_id` (interaction/acceptance attribution):**
- Migration (kiosk): rename `transactions.door_id` → `terminal_id` (reindex
  `idx_transactions_terminal_id`) **and** add new nullable `enclosure_id` (indexed).
  Historical attribution-only drift on old enclosure_diff rows is acceptable; optional
  backfill if meaningful history exists.
- Migration (controller): mirror both columns on projected `transactions`.
- `events/payloads.go`, `controller/consumer.go`: `DoorID`→`TerminalID`, add `EnclosureID`;
  wire keys `terminal_id` / `enclosure_id`.
- `commit.go`: write **both** — `terminal_id` = accepting terminal, `enclosure_id` =
  `cart.EnclosureID`.
- `exports/csv.go`: emit `terminal_id` + `enclosure_id`.
- `cart.go` + `cart/store.go`: split cart's `DoorID` into `TerminalID` (manual `?terminal=`
  acceptance) + `EnclosureID` (enclosure session).
- UI: `useCart.ts` `doorId`→`terminalId`; `CheckoutView.vue` `?door=`→`?terminal=`.
- Docs: schema.md, README, configuration.md.

**Bucket B → `enclosure_id` (enclosure session + read location context):**
- `cart/store.go`: `byUserDoor`→`byUserEnclosure`, `StartByExternal(…doorID)`→`…enclosureID`,
  `GetByUserDoor`→`GetByUserEnclosure`, `userDoorKey`→`userEnclosureKey`.
- `commands/rfid_enclosure.go`: `DoorID`→`EnclosureID` in both command payloads + lookups.
- `rfid_read_trigger.go`: enclosure_diff observed-event key `door_id`→`enclosure_id`.
- `config.go`: `RFID.DoorID` (yaml `door_id`, env `KIOSK_RFID_DOOR_ID`) → `RFID.EnclosureID`;
  keep "required when mode=enclosure_diff".
- Docs: rfid.md, wire.md, kiosk.yaml.example, configuration.md.

**Decision D5 (revised):** two columns. `terminal_id` (the interacting/accepting screen —
always present) + `enclosure_id` (the cabinet — set for `enclosure_diff`). For
`enclosure_diff` **both** are present and distinct (which-door vs which-cabinet); for
`counter_scan`, `terminal_id` only. The cart-session key stays `(user_code, enclosure_id)`.

Tests: commit / cart / controller / rfid-diff suites. Single-kiosk: both fields default
empty — unchanged.

### Phase 2 — Reader map  *(behavior-preserving at N=1)* — **DONE (core)**

- Config: single `reader:` → `readers:` map keyed by `reader_id`, each carrying
  `mode`/`host`/`port`/`enclosure_id`/`antennas` (mode + enclosure_id moved node-level →
  per-reader, so one node can host `counter_scan` + `enclosure_diff` at once). `read_window`
  stays shared (top-level), capped when any reader is `enclosure_diff`. Per-reader fields are
  YAML-only (no flat env path into a map entry).
- `h.RFID rfid.Reader` (single) → `h.Readers map[string]*handlers.ReaderHandle`
  (`{Reader, Mode, EnclosureID}`); `rfid.New` now takes one `RFIDReaderConfig`; `readMu` is
  per-reader by construction (one `impinjReader` per entry).
- Resolution: `ReaderByID("")` returns the sole reader (N=1 implicit); `ReaderForEnclosure`
  matches a cabinet by `enclosure_id` and falls back to the sole reader. Counter button and
  identity `rfid_mode` use `SoleReaderMode()` (meaningful at one reader; multi-reader SPA
  selection is the terminal work).
- `main.go`: one `rfid.New` per entry, Connect each on serve, Close all on terminate.
  Single-reader configs behave exactly as before.
- **Controller parity (deferred to a follow-on):** read-only `config.snapshot` command +
  detail-page tab for reader/enclosure observability. Not blocking — node config is edited
  locally; this is observability polish.

### Phase 3 — `counter_scan` multi-reader + terminals first-class  *(additive)*

- Terminal identity from URL param; node config binds terminal → enclosure/reader/mode.
  Handler fires `h.Readers[reader_id]`. Zero-config variant: URL carries `reader` directly.
- Enclosure_diff accept now stamps `terminal_id` (the accepting door's screen).
- At N=1, no param needed (implicit).

### Phase 4 — `enclosure_diff` partition  *(additive — the capability)* — **DONE (backend)**

- `item_instances.enclosure_id` (migration 1802000000; nullable, indexed). Explicit
  admin-assigned membership, no auto-flow. Empty = counter/crib stock or a single-cabinet
  node.
- No separate `enclosures:` config — a reader's `enclosure_id` (Phase 2) already links it to
  its cabinet, and `ReaderForEnclosure` matches on it.
- `handlers.expectedInstanceStates(enclosureID)` partitions the expected-present set by
  `enclosure_id` **only when `enclosureCount() > 1`** — a single-cabinet node (and any
  not-yet-assigned instance) keeps the whole-inventory set, so existing deployments need no
  backfill. `rfid.Diff` **untouched**. Covered by `TestPerformReadTrigger_PartitionsByEnclosure`.
- **"Two doors" = two antennas on one reader** (already supported per-reader), so
  multi-reader-per-enclosure **union is deferred** — only needed when a cabinet needs >1
  physical reader (rare). One reader per enclosure is the shipped model.
- **Deferred to the parity step:** the assignment UI — `enclosure_id` through the instance
  create/edit command + `ItemInstancesPanel` / `KioskInstancesPanel`. Until then it's
  assignable via the PB superuser UI / CSV.
- Edge (note, don't solve): a tool returned to the wrong cabinet shows as unresolved in the
  wrong enclosure and missing in its home — same class of issue the single-enclosure model
  already has at the node boundary.

### Phase 5 — Multi-terminal hardening

- Verify the cart store handles N concurrent terminals (already mutex-guarded + keyed by
  `cart_id`, likely fine) and scrub any single-active-user assumptions in SPA/session.
  Confirm the accept-cart UI works at a per-door terminal. Verify, don't redesign.

## Deferred (shaped-for, not in this plan)

- **Location / sightings.** Sighting `{tag_id, zone?, lat?, lon?, time}` (static gateway →
  zone; roaming gateway → GPS). Heartbeat-family transport (lossy, last-write-wins, outside
  the durable ledger stream). Advisory `last_observed_*` projection mirrored to the owning
  node like `punch_state`/`open_checkouts_state`. Reconciliation reports (custody vs
  location diff) = the real value. `event.scan.rfid.observed` + its controller filter are the
  first ingestion seam.
- **Access-control vendor integration.** Wire a real lock/badge system as the publisher of
  `cart.start`/`read.trigger`. Lock→enclosure + badge→worker resolution in an adapter (or
  node-side via the scan resolver).
- **Subcontractor / account modeling.** Bill-to party = the sub, above `groups`. "rent" is
  **attribution only** — no in-app pricing/billing math (same stance as no-payroll-rounding).
- **Remote config editing** from the controller (push reader/enclosure config to a node).

## Decisions

| # | Decision | Resolution |
|---|---|---|
| D1 | Terminal→reader binding | Config-binds-terminal (URL carries terminal identity; node config maps to enclosure/reader/mode). Zero-config URL-carries-reader variant allowed. |
| D2 | Topology | Per-deployment. (A) one node + `enclosure_id` partitions (default for multi-cabinet sites); (B) node-per-enclosure + `site` label. |
| D3 | Rename scope | `door`→`terminal`, `tool`→`asset` (UI copy), `kiosk`→`node` (docs). NOT project / `kiosk_code` / config keys / `KIOSK_*` env / collections. |
| D4 | Subcontractor modeling | Deferred to a future phase. |
| D5 | Transaction attribution | **Two columns**: `terminal_id` (accepting/interacting screen) + `enclosure_id` (cabinet). Both present for `enclosure_diff`; `terminal_id` only for `counter_scan`. |
