# Overview

A high-level tour of what this platform is, the mental model behind it,
and how its three programs fit together. Read this first; the other docs
under [`docs/`](README.md) go deep on each piece.

## What this is

An **asset custody and location platform** for physical inventory that
moves between hands and places. A worker walks up to a touchscreen,
identifies themselves with a badge scan, scans the tools or consumables
they're taking (or returning), confirms, and walks away. Behind that
30-second interaction is an append-only ledger that always knows **who
has what** — and, when location hardware is present, a separate advisory
signal for **where each thing was last seen**.

It was built for **construction and tool-rental operations** — a rental
yard or general contractor stocks tool cribs and consumable storerooms at
job sites; subcontractors and crews check out drills, lasers, and
fasteners; the office needs to know what's out, to whom, and whether it's
coming back. But nothing in the core is construction-specific. Anywhere a
defined set of physical things is shared across people and locations — a
hospital equipment pool, an IT device locker, a film-gear cage, a research
instrument room — the same model applies: **track custody first, layer
location on top, and reconcile the two.**

The whole system is a small number of self-contained Go binaries. The
core kiosk is one ~40 MB executable that embeds its web server, database,
and touchscreen UI. `scp` it to a mini-PC and run it — no separate API
server, no separate database, no cloud dependency for normal operation.

## The core idea: custody, location, and the gap between them

The platform knows two orthogonal things about each tracked asset:

1. **Custody — *who is responsible for it.*** Authoritative. It comes from
   an append-only ledger of checkout/return events, exactly like a bank
   ledger: entries are never edited or deleted, only appended. "What's out
   right now" is always derivable from that history. This is the part that
   is *true* — it's the system of record.

2. **Location — *where it was last seen.*** Advisory. It comes from RFID
   or BLE readers ("gateways") placed around a site that passively observe
   tagged units and report a coarse zone (or GPS coordinates for a roaming
   reader). It's lossy and best-effort. A missed read just means a stale
   "last seen" — it **never** blocks a checkout or alters the ledger.

The product value is the **reconciliation of the two**:

- *"Checked out to Bob, but last seen still in Cabinet A"* — maybe never
  actually taken.
- *"In someone's custody, but not seen anywhere in three days"* — maybe
  lost.
- *"No custody record, but seen leaving the yard"* — unaccounted movement;
  a theft signal.

Custody tells you the paperwork. Location tells you the ground truth. **The
gap between them is the alert** — and that gap is the thing this platform
exists to surface. Custody is mandatory and always on; location is an
optional layer you add when you install gateways.

## The mental model

A handful of nouns carry the whole design. Most deployments only ever touch
the first few; the rest exist so a single tool crib can grow into a
multi-building, multi-cabinet site without changing the model.

| Concept | What it is |
|---|---|
| **Node** | A self-contained unit: one binary, one database, one shared inventory. The unit of trust and the only authority over its own custody ledger. Historically "a kiosk." A node can be one screen on a wall or a whole building's worth of cabinets and screens. |
| **Terminal** | An interaction point — a screen a worker stands at. One node can drive several (e.g. an inside and outside screen, or two doors of a building). Purely descriptive attribution on each transaction; never a security boundary. |
| **Reader** | A physical RFID/BLE reader wired to a node, used to read tags at the point of checkout (bulk scanning, or smart-cabinet reconciliation). |
| **Enclosure** | An access-controlled smart cabinet or locker — an inventory partition inside a node. A serialized unit can "live in" an enclosure, and a cabinet read reconciles what's still inside against what left. |
| **Gateway / Zone** | A **location** sensor placed around the site, operated *outside* the platform. It observes tags and reports sightings; a static gateway maps to a coarse **zone**, a roaming one to GPS. The platform only consumes these — it never operates a location reader itself. |
| **Site** | A grouping label over several nodes. Owns nothing; it's organizational. |

And the inventory itself:

| Concept | What it is |
|---|---|
| **Item (SKU)** | A catalog entry — "1/2-inch impact driver," "box of #8 screws." Carries quantity-on-hand and a reorder threshold. |
| **Instance (unit)** | One physical, individually-tracked thing under a serialized SKU — *this specific* impact driver, with its own barcode, printed serial, and RFID tag. Three drivers under one SKU stay distinct, so returns close out the exact unit that was scanned. |
| **Tracking mode** | How a SKU is counted: **consumable** (decrements on use — screws, blades), **quantity-tracked tool** (a fungible pool of N — a bin of tape measures), or **serialized** (each unit is an Instance with its own lifecycle). |

Two kinds of people use the system, with deliberately different trust models:

- **Workers** identify by badge scan and never log in. On the standard
  kiosk the box itself, on a trusted local network, is the trust boundary.
- **Admins** log in with email and password to manage the catalog,
  workers, imports, reports, and notifications.

## The three programs

The platform ships as three binaries that share the same internal building
blocks but expose deliberately different surfaces.

```
                         ┌───────────────────────────────┐
                         │     kiosk-controller          │   (optional, central)
                         │  • aggregates every node's    │
                         │    events into one ledger     │
                         │  • pushes catalog down to      │
                         │    each node it manages        │
                         │  • live online status, fleet   │
                         │    reports, central email       │
                         └───────────────────────────────┘
                              ▲            ▲            ▲
                    NATS      │            │            │   NATS
              (events up,     │            │            │
           catalog/commands   │            │            │
                  down)       │            │            │
              ┌───────────────┘            │            └───────────────┐
              │                            │                            │
   ┌──────────────────┐         ┌──────────────────┐         ┌────────────────────┐
   │   kiosk-app       │         │   kiosk-app       │         │  kiosk-timeclock   │
   │  (Building A)     │         │  (Building B)     │         │  (phone punch page)│
   │                   │         │                   │         │                    │
   │ touchscreen +     │         │ touchscreen +     │         │ authenticated      │
   │ badge + scanner   │         │ badge + scanner   │         │ self-service       │
   │ checkout/return   │         │ checkout/return   │         │ clock in/out       │
   └──────────────────┘         └──────────────────┘         └────────────────────┘
```

### 1. The kiosk node — `kiosk-app`

The heart of the system and the only program most deployments need. It
serves the touchscreen checkout flow and the admin web UI from a single
process, and owns its own database. It handles the full custody loop:
checkout, return, consume, serialized-unit tracking and maintenance, stock
levels, the optional timeclock, and optional RFID reading. Every state
change flows through one append-only ledger.

A node is **edge-first and autonomous** — it needs no internet and no
central server to do its job. It keeps running, and the ledger stays
correct, even if the network or the controller is down.

### 2. The central controller — `kiosk-controller`

Optional. For deployments with more than one node, the controller is a
central hub that:

- **Aggregates** every node's transaction events into its own consolidated
  ledger, so the office gets one fleet-wide view of what's out everywhere.
- **Distributes the catalog** down to each node — and does so *per node*:
  membership decides which SKUs each location stocks, so a node only ever
  receives its own slice.
- **Tracks live online status** via heartbeats, and can issue admin
  commands to a node remotely (adjust stock, force-close a checkout, change
  a unit's status).
- **Centralizes email** — receipts, low-stock alerts, and scheduled
  digests go out from one place against one mail server.

Crucially, the controller **owns nothing authoritative**. Each node remains
the source of truth for its own custody; the controller is a pure
aggregator and distributor. A node never waits on it. This keeps the
network edge-resilient: a controller outage degrades reporting and central
email, never the ability to check a tool in or out.

### 3. The virtual timeclock terminal — `kiosk-timeclock`

Optional. A publicly-reachable, **per-worker-authenticated** punch page so
crews can clock in and out **from their phones** — no badge scanner, no
physical station. Its trust model is *inverted* from a kiosk: instead of
trusting the box, each worker signs in (company SSO or password), and the
server reads the punched identity from the authenticated session, never
from the request. A worker can only ever punch their own clock.

It's the same timeclock engine as the kiosk under the hood — same ledger,
same fleet-wide state — but it exposes *only* the authenticated punch
surface. None of the anonymous checkout endpoints exist in this binary, so
a public deployment can't accidentally expose them.

## How a deployment grows

A guiding principle is **"N=1 is invisible"**: a single kiosk needs zero
extra ceremony, and every multi-node, multi-cabinet, or central-management
capability is opt-in by topology. You never pay for complexity you don't
use.

1. **One kiosk, standalone.** A mini-PC, a touchscreen, a USB barcode
   scanner. One screen, one inventory, a local database. No network
   required. This is the whole product for a single tool crib.

2. **A node grows peripherals.** Add more screens (terminals), more RFID
   readers, or smart cabinets (enclosures) on the same node — still one
   binary, one database, one shared inventory. A single building becomes a
   site without becoming a distributed system.

3. **A fleet federates under a controller.** Point several nodes at one
   controller over NATS. Now the catalog is managed centrally and pushed
   down, every node's activity rolls up into one ledger and one set of
   reports, and central email and remote admin commands come online — while
   each node stays independently operable.

The same three steps describe the operating **modes** every binary
supports: *standalone* (no network), *standalone + events* (emits its
activity onto a message bus for outside consumers but manages itself), and
*controller-managed* (full federation).

## What it does — a capability tour

Each of these has a dedicated deep-dive doc; this is the one-paragraph
version of each.

- **Checkout / return / consume in one flow.** Scanning an item picks the
  natural action automatically — a consumable is consumed, a tool already
  out *to you* is returned, anything else is checked out — and the worker
  can override before confirming. See [Ledger](ledger.md) and
  [API](api.md).

- **Serialized units with a maintenance lifecycle.** Each physical unit has
  a status: *in service*, *in maintenance*, or *retired*. A returned tool
  can be parked in maintenance (per-SKU policy, or a per-return toggle any
  worker can flip) where it still counts as on-hand but isn't available to
  check out until an admin clears it. Units are never deleted — they retire,
  reversibly — so the ledger's history never dangles. See [Schema](schema.md).

- **Stock levels with an audit trail.** Quantity-tracked items carry an
  on-hand count and a reorder threshold; admin adjustments write an audit
  row in the same database transaction as the change. A low-stock report
  surfaces what needs reordering. (Serialized counts are *derived* from the
  live unit count, so they can't drift.) See [Operations](operations.md).

- **Timeclock with tool interlocks.** Workers clock in and out against a
  second append-only ledger, with the same "never edit, only append"
  discipline. Two optional interlocks tie the clock to the tool ledger:
  *no checkout unless clocked in*, and *no clock-out while holding tools*
  (which lists exactly what to return, and at which building, fleet-wide).
  The raw punch export is the payroll contract — no rounding or overtime
  math lives here. See [Configuration](configuration.md).

- **RFID-assisted checkout.** A networked reader can act as a bulk
  barcode-gun (hit "RFID scan," every observed tag folds into the cart),
  or drive a **smart cabinet**: an access-control event opens a session, a
  trigger reads the cabinet, and the system reconciles what's still inside
  against what left and proposes the resulting cart for the worker to
  confirm. See [RFID](rfid.md).

- **Advisory asset location.** RFID/BLE gateways around the site — run
  *outside* the platform — report sightings onto the message bus; each
  tracked unit gets a coarse "last seen" zone (or GPS). Custody reads
  double as sightings for free. This powers the custody-vs-location
  reconciliation described above, a Locations view, and a reconciliation
  digest. See [Location & Sightings](location-sightings-plan.md).

- **Reporting and notifications.** What's out, aging checkouts, low stock,
  group activity, recent transactions, adjustment audits, and notification
  deliverability — each with a CSV export that respects on-screen filters.
  Transaction receipts, low-stock alerts, and scheduled digests go out by
  email. On a controller these span the whole fleet; standalone, they cover
  the one node. See [Notifications](notifications.md).

## Why it's built this way

A few load-bearing decisions explain most of the design:

- **One binary, no runtime dependencies.** Go + an embedded database +
  the compiled web UI in a single executable. Deployment is copying a file.
  There's nothing to install on the mini-PC and nothing to keep running
  beside it.

- **The ledger is the system of record, and it's append-only.** There is
  exactly one code path that writes custody state, and it only ever
  appends. "What's out right now" is a materialized view that can be
  rebuilt from the ledger at any time, so it can never permanently drift.
  Corrections are new entries with a reason, never edits — the same
  discipline a financial ledger uses.

- **The node is the only authority; the controller only aggregates.**
  There's no central database that nodes depend on to operate. This is what
  makes the system edge-resilient and "federation-ready" without a fragile
  central tier.

- **Location is advisory and can never gate custody.** A wrong or missing
  sighting is, at worst, a stale "last seen" cell. The checkout path never
  reads location. This keeps the authoritative ledger clean and the
  workflow unblockable.

- **Trust models match the deployment.** A kiosk on a trusted local network
  treats the box as the boundary and reads worker identity from a badge.
  The internet-facing timeclock inverts that — every worker authenticates,
  and identity comes from the session, never the request. Each binary
  exposes only the surface its trust model can safely back.

## Where to go next

- New to running it? [Configuration](configuration.md) and
  [Operations](operations.md).
- Want the data model? [Schema](schema.md) and [Ledger](ledger.md).
- Integrating over the wire? [Wire reference](wire.md) and [API](api.md).
- Multi-site? [Central controller](controller.md).
- The optional hardware/location layers? [RFID](rfid.md) and
  [Location & Sightings](location-sightings-plan.md).
- What's shipped vs. planned? [Shipped & roadmap](roadmap.md).

The repo-root [`README.md`](../README.md) is the feature-by-feature
companion to this conceptual overview.
