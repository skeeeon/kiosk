# Northwind demo — plan

Status: **phases 1–3 landed; 4–6 not started.** Living doc; update as phases land.

The fixture and both appliers are in `internal/demoseed`, the two `demo-seed`
subcommands are registered on their binaries, and `demo/standalone.yaml` makes the
standalone demo runnable today. What is still ahead: the mock RFID reader, the
rule-router driver and the per-kiosk configs, and the platform Thing types.

The repo has no demo story. This plans one: a runnable **Northwind Traders**
estate — three kiosks, a virtual timeclock terminal and a central controller —
that stands up from two subcommands and then runs itself, and that composes with
the [stone-access](https://github.com/stone-age-io) and Stone Age platform demos
already describing the same company.

Everything here is **demo tooling**. Nothing in it ships to a customer install,
and the passwords are deliberately weak and well-known.

## North-star

Open a browser on a cold machine and, twenty minutes later, be showing a fleet: a
tool crib whose RFID cabinet notices a wrench leaving, a dock kiosk handing out
gloves from a deliberately different catalogue, a crib at a second site, workers
clocking in at one building and out at another, and a controller that can see and
command all of it — with a live activity feed nobody had to fake.

The estate is the same company as the other two demos: same sites, same people.
Elena badges through the freezer door at 05:10 in stone-access and checks out a
pallet jack at the crib at 05:14 here. That costs almost nothing if it is decided
now, and is expensive to retrofit later.

## Where this came from

The two sibling repos have converged on one shape, from opposite directions:

- **Static graph → an in-binary `demo-seed`.** `internal/demoseed`, running
  in-process against `core.App`, idempotent by natural key, covered by
  `go test ./...`. Both siblings adopted this *after* retiring an external script.
  access-control's README is blunt about why `seed.ps1` lost: it drifts from the
  schema silently and needs a live superuser session.
- **Live motion → rule-router**, publishing a **stimulus** rather than an outcome.
  access-control's rules write to the *reader* subject, so running
  `access-controller` processes decide each presentation themselves — "nothing here
  fabricates a decision." Its predecessor injected finished `evt.tap` events and was
  demoted for exactly that reason: it can only replay a decision someone typed into
  a YAML file.

This repo has neither half — only `seed-catalog` (CSV) and no `demo/` directory at
all.

---

## Principles

These are the load-bearing decisions. Everything below is downstream of them.

### Seed flows down. Truth flows up.

Catalogue goes controller → JetStream KV → kiosks. Ledger goes kiosks → event
stream → controller.

The controller's `transactions` and `transaction_lines` are therefore **never
seeded directly**, for the same reason rule-router must not fabricate events. If
the controller's database is wiped mid-preparation, it is recovered by
republishing from the kiosks, not by re-running a script — and that is worth saying
out loud during a demo, because it is true by construction rather than by
discipline.

### The rules supply stimulus, never outcomes

rule-router calls the kiosk's own anonymous HTTP endpoints. The kiosk resolves the
scan, defaults the action (`handlers.defaultActionFor`), validates
(`commit.Commit`), writes the ledger and publishes the events. The controller
projects them through its ordinary durable consumer.

> **The demo rules MUST NOT publish on `kiosk.*.event.>`.**

That single line is the difference between this demo and the one access-control
retired. A forged `transaction.complete` would fill the controller's ledger while
the kiosk's own database knew nothing — and the controller's kiosk-detail panels
*enrich live snapshots* with ledger-derived out-counts
(`internal/controller/snapshot_enrich.go`), so you would get "out: 7" against a
kiosk whose real `open_checkouts` is empty, with Inventory and Instances
contradicting each other on the same screen.

### State lives where state belongs

rule-router is stateless and its `{@random.*}` draws are independent. A card tap is
fire-and-forget, so a random tap is a plausible tap. A checkout is *half of a pair*
— the return must name the same worker, the same SKU, the same unit, and land
later.

The demo never asks rule-router to hold that correlation. Three mechanisms supply
it, none of them in the rules:

1. `defaultActionFor` decides checkout-vs-return server-side. The rules scan; they
   never name an action.
2. **Pool sizing makes the loop self-balancing** — see "The catalogue is sized for
   the loop to close" below. This is what removes any need for a return sweep, and
   with it any need for the rules to read state at all.
3. The mock RFID reader owns its cabinet's tag list, on disk. rule-router only says
   *read now*.

An earlier draft of this plan had rule-router poll each worker's open checkouts and
fan them back into a return cart — four chained rules, a subject-embedded cart id
and a timing-window throttle to rejoin the fan-out. It was deleted because the
arithmetic in (2) makes it unnecessary. That is worth remembering the next time
something here looks like it needs state: check whether the kiosk already supplies
it.

### Don't simulate the operator

Inventory adjustments, send-to-maintenance, admin-close and the controller's remote
commands stay **presenter-driven**. Simulating them fills the audit log with actions
nobody took, and it steals the best beats of a live demo — the moments where a human
does something and the fleet reacts.

---

## The estate

Sites come from the platform's own seed
(`platform/internal/demoseed/data.go`), which already places Northwind at
`KC-DC1` (Kansas City Distribution Center, with freezer/chilled zones and a comms
cabinet), `KC-OFFICE` and `SGF-XD2` (Springfield Cross-Dock).

| Node | `kiosk.code` | Site | Modality | Port |
|---|---|---|---|---|
| Main tool crib | `KC-DC1-CRIB` | KC-DC1 | counter_scan + **two** `enclosure_diff` cabinets | 8101 |
| Dock consumables | `KC-DC1-DOCK` | KC-DC1 | badge + counter, no RFID | 8102 |
| Cross-dock crib | `SGF-XD2-CRIB` | SGF-XD2 | counter + one cabinet | 8103 |
| Timeclock terminal | `KC-OFFICE-TC` | KC-OFFICE | `cmd/timeclock`, virtual | 8092 |
| Controller | — | KC-DC1-MDF | `cmd/controller` | 8091 |

All on one host. That is a demo convenience, but it is also a hard requirement of
the driver: see "Topology is load-bearing" under Phase 5.

**Three kiosks, chosen to cover every distinction the product makes.**
`KC-DC1-CRIB` is the flagship — the RFID tool crib. `KC-DC1-DOCK` is an ordinary
badge/barcode kiosk stocking consumables, which is what most installs actually are,
and it sits at the *same site* as the crib so the per-kiosk catalogue difference is
visible without changing buildings. `SGF-XD2-CRIB` is the second site: it makes the
fleet real, gives the controller something to aggregate across, and is what makes
the cross-kiosk clock-out gate demonstrable — a worker with a tool out at
Springfield being blocked from clocking out in Kansas City is the whole argument
for the `open_checkouts_state` replica.

A fourth and fifth node (a freezer PPE locker, an IT equipment cabinet at the
office) were dropped from this plan: each costs a config, a data dir, a fixture
slice and a rule set, and neither shows anything the three above do not. Add them
later if a specific demo needs them.

**KC-OFFICE deliberately has no kiosk.** Office staff punch from their phones
against the virtual terminal, which is exactly how that binary is meant to be
deployed.

**People are shared with stone-access.** `nw-elena` (foreman, warehouse),
`nw-marco`, `nw-owen`, `nw-dana`, `nw-raj` — same codes, names and emails that
repo's `demo-seed` writes. Groups: `nw-warehouse`, `nw-dock`, `nw-office`,
`nw-maint`.

### The catalogue is sized for the loop to close

This is the single number that makes the whole driver work, so it is worth the
arithmetic.

A worker holding `k` tools out of a per-kiosk pool of `N` SKUs walks up and scans
one at random. If they already hold it, `defaultActionFor` makes it a **return**;
otherwise a **checkout**. So per scan:

```
E[Δk] = (1 − k/N) − (k/N) = 0   when   k = N/2
```

**The loop self-balances**, and `k = N/2` is a stable equilibrium — below it
checkouts dominate, above it returns do. Roughly ten tool SKUs per kiosk against
five or six workers settles at about five held each, thirty-ish open checkouts per
kiosk: a Tools Out report that looks like a working crib and stays that size
overnight. Want more outstanding? Widen the pool. Want less? Narrow it.

A hundred SKUs would make almost every scan a checkout and `open_checkouts` would
only ever grow — which is the failure mode that tempts you into writing a return
sweep. Size the pool instead.

(Consumables are exempt from the arithmetic: `consume` never touches
`open_checkouts`, so they can be as numerous as you like. Serialized units follow
the same rule as tools, per instance rather than per SKU.)

Categories — `hand-tools`, `power-tools`, `ppe`, `consumables` — exist so the "bulk
add by category" action on `KioskItemsPanel` has something to demonstrate. Note
what that action is *not*: categories are not a stored rule, so items added later
do not auto-fill. The demo should show that once.

**Membership is the differentiator.** `KC-DC1-DOCK` stocks gloves, blades, tape and
batteries and no hand tools; `KC-DC1-CRIB` stocks the tools and none of the dock's
consumables; `SGF-XD2-CRIB` stocks a deliberately thinner subset of the crib's
catalogue. The same SKU at two kiosks shares a `code` and holds its own quantity
and its own units locally. That is the whole point of `kiosk_items`, and it is
invisible unless the fixture deliberately makes the sites differ.

---

## What propagates, and what does not

This governs the shape of the seeder, so it is worth stating precisely.

`internal/catalog/payload.go` carries three payloads across the wire — items,
users, groups — and the package comment is explicit about the exclusions. What does
**not** cross:

- `quantity_on_hand` and `reorder_threshold`, named as kiosk-local; `applyItem`
  will not overwrite them on a resync.
- **`item_instances` entirely.** Serial, `rfid_epc`, `ble_id`, `status`,
  `enclosure_id`. Not in the payload: *"RFID EPCs and per-unit serials live on
  item_instances, not on the SKU."*

So a controller-only seed gets you the shared vocabulary and stops exactly short of
the things this demo is *about*. That is the design working, not a gap — an
instance is a physical unit in a specific cabinet at a specific site, and
broadcasting it fleet-wide would be meaningless.

**Do not add instances to `ItemPayload` to collapse the seeder into one place.**
The round-trip test that enforces excluded-fields-stay-excluded exists to stop
precisely that.

`reorder_threshold` is the one genuinely awkward case: it does not cross the wire
and there is no command that sets it (there is no item-field mutation command at
all). **The local applier sets it directly, and the payload is left alone.**

It is tempting to argue that a reorder threshold is really a per-SKU stocking policy
and belongs in `ItemPayload` — but that is a product design change being proposed
for a demo's convenience, and the exclusion is deliberate and test-guarded. One
direct field write in the local applier costs a line. **This demo changes no
production design.** If a real deployment later wants fleet-wide thresholds, that
is its own argument to make on its own evidence.

### The consequence, stated plainly

Holding that line has a cost, and building Phase 1 surfaced it: **the managed
estate gets no reorder thresholds.** The local applier sets them; the remote
applier cannot, because there is no command and no payload field. So on the
three-kiosk managed demo every `reorder_threshold` stays at zero until someone sets
it, and the low-stock alert — which reads the threshold from the kiosk's own
snapshot, the kiosk being the source of truth for its own stocking policy — has
nothing to fire on.

`items.requires_maintenance_on_return` had the identical shape and the identical
consequence — `commit.Commit` reads it from the kiosk's local row, and it was not in
`ItemPayload` either, so a managed kiosk never learned that the torque wrench needs
recalibrating. But the two were never the same kind of thing. `reorder_threshold` is
named in `internal/catalog/payload.go`'s package comment as deliberately excluded
kiosk-local state. `requires_maintenance_on_return` was named nowhere — added by
migration 1795 and simply never wired into the payload. That was a product bug, not
a design decision, and it has since been **fixed on its own merits**: the flag now
crosses the wire alongside `type` and `tracking_mode`, guarded at all three hops
(payload round-trip, `itemPayloadFrom`, `Watcher.upsertItem`). Existing fleets pick
it up on the next catalogue reconcile.

So the gap is now `reorder_threshold` alone, and the admin SPA is no escape hatch —
its input carries `:disabled="managed"`, which is correct, because a managed kiosk
does not own its catalogue.

The managed runbook's answer: **run `kiosk demo-seed --confirm` at each kiosk before
starting it.** The seeder creates that node's item rows locally and applies its
thresholds, quantities and units; the catalogue watcher then upserts the same rows
by `code` on first start and leaves the quantities and the threshold alone. It costs
one extra line per kiosk.

The remote applier still earns its place — it is the production seam, and watching
the controller provision a fleet over the command bus is a better opening than a
database that was already full. But it cannot carry `reorder_threshold`, and this
plan should say so rather than imply the managed path is complete.

---

## Phase 1 — `internal/demoseed` — **landed**

One fixture table, two appliers. The most important thing about them:

> **The remote applier is the local applier with NATS in the middle.**

The kiosk's `instance.create` and `inventory.adjust` command handlers already call
`instances.PerformCreate` (`internal/instances/mutations.go`) and
`handlers.PerformStockAdjustment` (`internal/handlers/stock_adjust.go`). The local
applier calls **those same two functions directly**. One fixture, one set of
mutation functions, two transports. They cannot drift.

```
internal/demoseed/
  data.go          fixture tables — groups, users, items, per-kiosk local state
  seed.go          SeedCatalog + SeedFleet; findOrCreate; Result
  apply_local.go   ApplyLocal(app, kioskCode)  — no NATS
  apply_remote.go  ApplyRemote(nc, adminID)    — command bus, all nodes
  command.go       the kiosk binary's `demo-seed` subcommand
  seed_test.go     boots a real PB per case; asserts idempotency
```

Two shapes differ from the sketch above, both for the same reason — the fixture
should not be able to state something twice and disagree with itself:

- **Membership is derived, not listed.** `Node.ItemCodes()` is the union of the
  node's `Stock` and `Units` entries, so a node cannot declare it stocks a SKU it
  holds none of. `SeedFleet` writes the `kiosk_items` rows from it.
- **EPCs are derived, not written.** `Unit.EPC()` is a SHA-256 of the unit code
  under a fixed salt, rendered lowercase — deterministic across re-seeds (so a mock
  reader's tag file stays valid), unique by construction, and one less column to
  get wrong by hand.

`ApplyRemote` takes the whole estate rather than one kiosk: it is only ever called
from the controller, which provisions all of them in one pass.

**Idempotency** is found-or-created by natural key throughout — `users.code`,
`items.code`, `groups.code`, `kiosks.kiosk_code`, the `(kiosk, item)` pair. A run
that dies partway heals on the next one, and a second full run must report zero
created. Field fills run only on create, so hand-edits survive a re-seed.

**The test harness** is `setupApp` from `internal/commit/commit_test.go`, copied
verbatim: `pocketbase.NewWithConfig` with a `t.TempDir()`, `app.Bootstrap()`, then
`core.NewMigrationsRunner(app, core.AppMigrations).Up()` — because `migratecmd`'s
`Automigrate` hooks `OnServe`, which never fires in a test. `KIOSK_QUIET_BOOTSTRAP=1`
keeps the bootstrap-admin banner out of test output.

### Things the fixture has to get right

**Creating items publishes nothing.** `publishItemToMembers` loops over
`KiosksForItem`, and a freshly-created item has no memberships — the loop body
never executes. **Creating `kiosk_items` rows is what makes catalogue appear at a
kiosk.** Seeding items alone (which is all `seed-catalog` does) pushes nothing
anywhere. Users and groups do broadcast on create, keyed by `code`.

**`kiosks` needs `kiosk_code` and `status`, and `status` has no schema default** —
set `"unknown"` explicitly, as `touchKiosk` does. Pre-registering all seven rows is
the cleanest of the three paths into that table; the heartbeat and first-transaction
auto-register paths then converge on the same row and do nothing.

**`inventory.adjust` rejects serialized items** (`ErrSerializedNotAdjustable`), and
correctly so: serialized `quantity_on_hand` is a materialized view of the
non-retired instance count. For a serialized SKU, creating the instances *is* the
whole job — the `item_instances` hooks recompute the count. The fixture must not
carry a quantity for those rows.

**There is no `enclosures` collection and no `locations` collection.**
`enclosure_id` is a free string whose authoritative set is
`config.RFIDConfig.EnclosureIDs()` — the enclosure registry is the YAML, not the
database. The fixture's enclosure ids
must match the demo `kiosk.yaml` files exactly, and that coupling deserves a comment
in both places. (`GET /api/kiosk/locations` likewise reads
`item_instances.last_observed_*`; "location" here is advisory sighting state, never
a catalogue table.)

**Uniqueness that will bite a careless fixture:** `open_checkouts.serial` is
unique-when-non-empty, and `item_instances.rfid_epc` is unique-when-non-empty and
lowercased. Generate EPCs from a deterministic seed so a re-run produces the same
values.

---

## Phase 2 — `kiosk-controller demo-seed` — **landed**

Copy `internal/controller/seed.go`'s `RegisterSeedCommand` shape. The parts that
matter:

- `app.RootCmd.AddCommand(cmd)`, registered from `cmd/controller/main.go` beside
  the existing `RegisterSeedCommand` call.
- Inside `RunE`: `app.Bootstrap()` then
  `core.NewMigrationsRunner(app, core.AppMigrations).Up()` — the one-shot
  subcommand applies migrations itself, with the same comment explaining why.
- `events.Connect` → `events.JetStream` → `NewCatalogPublisher` **before any
  writes**. That ordering is the whole point: the publisher binds PB record hooks,
  so every subsequent `app.Save` fans out to KV synchronously, and the hooks fire
  whether or not the HTTP server is up. Keep the `--no-publish` escape hatch.
- A `--confirm` gate, matching `accessd demo-seed --confirm`. This creates working
  identities; it must be hard to run against the wrong instance.

### Four things `seed-catalog` does not do

**1. Ensure the JetStream stream exists.** This is the ordering hazard and it is
worth understanding rather than memorising.

`ensureStream` is private to the aggregator and runs from `Aggregator.Start` — i.e.
only when the controller *serves*. But the seeder cannot run while the controller
serves (it writes the controller's own PocketBase database; this is why
`seed-catalog` is a standalone subcommand, and why access-control's seeder
advertises working against a stopped instance).

So under the obvious ordering — kiosks up, seed, then start the controller — the
stream does not exist while the seed runs, and every `instance.lifecycle` and
`inventory.adjust` event the remote applier triggers is published to nothing. The
kiosks stay correct and the live snapshots stay correct, but the controller's
`inventory_audit` and `instance_lifecycle_audit` are empty for the entire seeded
estate. **And there is no republish command for those** — `ledger.republish` and
`timeclock.republish` cover transactions and punches; the audits cannot be
backfilled.

The fix is to have the seeder ensure the stream itself, idempotently, the same way
it already brings up its own publisher hooks. Then ordering stops mattering.
`Aggregator.Start` also creates the durable consumer, which a one-shot CLI must not
do — so **export a small `EnsureStream(ctx, js, streamName)` and have both
`Aggregator.Start` and the seeder call it**, matching this repo's stated preference
that the CLI and HTTP paths can't drift. The config must build its subjects through
`events.StreamSubjectFilter()`, which depends on the prefix installed in `main()`.

**2. Get a `*nats.Conn`.** `events.Conn(pub)` — the exported helper, not a type
assertion.

**3. Wait for the catalogue to land before creating instances.**
`instance.create` resolves `item_code` against the kiosk's **local** items table,
and KV → watcher → local upsert is asynchronous. The seeder polls
`inventory.snapshot` per kiosk until the expected SKUs appear, bounded, with a
failure message that names the kiosk and the SKU rather than timing out anonymously.

**4. Flush before closing.** `seed.go`'s `defer pub.Close()` returns immediately.
A command-sending seeder must drain first or lose in-flight requests.

### Sending the commands

`dispatchKioskCommand` and `fetchKioskData` need a `*core.RequestEvent` and a
populated `HeartbeatRegistry`, so they are not reusable here — a one-shot CLI has
no HTTP request to answer and no beats recorded. The template to copy is
`fanoutSnapshots` in `internal/controller/reports_lowstock.go`: about twenty-five
lines of `nc.Request` plus a `kioskCommandEnvelope` decode, converting
`ErrTimeout`/`ErrNoResponders` into a plain `kiosk_offline`. It also shows the
parallel-goroutine shape for fanning out across the fleet, which matters — five
kiosks times forty instances at a five-second timeout each is worth doing per-kiosk
concurrently, with a progress line.

Two details:

- **A command message with no reply inbox is dropped without dispatch.** The
  seeder uses request/reply, never a bare publish.
- `controller_admin_id` is required on the wire but is a plain text column with no
  foreign key on either side (deliberately — admins live in the controller's
  database, so a FK would dangle). Look up a real `admins` record id so the audit UI
  renders a name; a sentinel also works. `command_id` is a fresh
  `uuid.NewString()` per attempt, reused verbatim on a retry.

### A side effect worth keeping

Every unit seeded this way lands with `source=controller` in `instance_audit` and
an `instance.lifecycle` event on the bus. That is not a demo artefact — the
controller really did provision them. "Watch the controller stand up three kiosks'
worth of inventory over the command bus" is a better opening than "here is a
database that was already full."

---

## Phase 3 — `kiosk demo-seed` — **landed**

The standalone path. One binary, one config, no controller, no NATS — which is the
cheapest thing to put in front of a prospect and the first demo most people will
ever see.

There is no subcommand registration on `cmd/kiosk` today; only `migratecmd`. The
structurally identical insertion point is immediately after
`migratecmd.MustRegister`, mirroring where `cmd/controller/main.go` registers its
seed command. `config.EnsureServeBind` already returns args unchanged for any
non-`serve` subcommand, so nothing else needs to change.

Two gotchas specific to this binary:

- **`cmd/kiosk/main.go` does a great deal of wiring before `app.Start()`, and all
  of it runs for a subcommand too** — including the NATS connect and
  `instances.New().Register(app)`. The instance hooks firing is *wanted*: every
  `item_instances` row the applier saves writes its own `instance_audit` row,
  publishes `instance.lifecycle`, and recomputes `items.quantity_on_hand`. Which is
  why the local applier must **not** also hand-write `instance_audit`. The
  `OnServe` registrations (watchers, RFID, command bus) are inert.
- **`kiosk.yaml` must exist** or `main()` fatals before cobra ever dispatches.

---

## Phase 4 — the mock RFID reader

`read.trigger` asks a physical reader, whether it arrives over HTTP or NATS. So the
enclosure_diff flow — the headline story — cannot be demonstrated without hardware
unless there is a reader that invents reads.

Add `mode: "mock"` beside `counter_scan` and `enclosure_diff` in
`rfid.readers.<id>`. `internal/rfid/reader.go` is already the domain interface
(production uses `impinjReader`; tests substitute a fake), so this is a third
implementation rather than a refactor. access-control has the exact precedent: its
demo runs four controllers on driver `mock`, reader `nats`, and says so in the
prerequisites.

**It is a file, and that is the entire design.** The reader config carries a
`tag_file` path. `ReadFor` reads the file and returns the EPCs in it — one per
line, blank lines and `#` comments ignored. Nothing else. No population model, no
randomness, no timers, no clock.

```yaml
rfid:
  readers:
    cabinet-a:
      mode: "mock"
      enclosure_id: "A"
      tag_file: "demo/tags-cabinet-a.txt"
```

`kiosk demo-seed` writes the initial file for each mock reader from the fixture's
own EPCs, so a freshly seeded cabinet reads as full.

**Taking a tag out is deleting a line.** That is the demo:

```bash
# "Marco takes the torque wrench."
sed -i '/E28011606000020C87A5E1B2/d' demo/tags-cabinet-a.txt
```

...then trigger a read, and the kiosk's own diff proposes the checkout.

An earlier draft gave this reader a stochastic dwell model — tags leaving on a
probability, returning after twenty to ninety minutes. That was wrong twice over.
It violated **Don't simulate the operator** two sections above, and it made the
demo worse: a presenter pulling a tag and watching the cabinet notice is a far
better beat than tools wandering off by themselves while nobody is looking. It was
also fifty lines of simulation living inside the production binary. A file is four.

Everything downstream is real regardless. `internal/rfid/diff.go` reconciles the
observed EPCs against expected-present state exactly as it does for a physical
reader; a `maintenance` unit that leaves is still skip-and-counted rather than
synthesized into a checkout; a cross-user return is still skipped.

**Gate it.** Config validation should reject `mode: "mock"` unless an explicit
opt-in (`rfid.allow_mock: true`, or a build tag). A reader that reports inventory
from a text file is not something a production config should be able to acquire by
typo.

---

## Phase 5 — `demo/rules/`

### The chain

The cart is a three-call transaction with a server-issued id, so the driver is a
chain: an HTTP action republishes its response body to NATS
(`publishResponse`), and a NATS-triggered rule performs the next call templated
against that body.

`publishResponse` republishes the **raw 2xx body, unmodified** — there is no
templating on the response — so each hop sees exactly the previous hop's output and
nothing else. That constraint is worth respecting rather than working around: a
chain that stays three hops long and never needs a value from two hops back is a
chain you can read.

**The whole driver is these three rules, per kiosk:**

```yaml
# 1. A worker walks up. Cron picks who and when; the kiosk decides everything else.
- trigger:
    schedule:
      cron: "*/45 * 5-21 * * 1-6"
      timezone: "America/Chicago"
  action:
    http:
      url: "http://127.0.0.1:8101/api/kiosk/cart/start"
      method: POST
      payload: '{"user_code":"{@random.choice(nw-elena,nw-marco,nw-owen)}"}'
      publishResponse:
        subject: "demo.KC-DC1-CRIB.co.started"

# 2. They scan something. Body carries cart.id and cart.user_code.
- trigger:
    nats:
      subject: "demo.KC-DC1-CRIB.co.started"
      mode: core
  action:
    http:
      url: "http://127.0.0.1:8101/api/kiosk/cart/add"
      method: POST
      payload: '{"cart_id":"{cart.id}","item_code":"{@random.choice(TW-3801,ID-2210,PJ-0440)}"}'
      publishResponse:
        subject: "demo.KC-DC1-CRIB.co.lined"

# 3. Accept. The add response carries the cart again.
- trigger:
    nats:
      subject: "demo.KC-DC1-CRIB.co.lined"
      mode: core
  action:
    http:
      url: "http://127.0.0.1:8101/api/kiosk/cart/commit"
      method: POST
      payload: '{"cart_id":"{cart.id}","terminal_id":"counter-1"}'
```

**The rules never name an action.** `defaultActionFor` returns `consume` for a
consumable, `return` for a tool this worker already has out, and `checkout`
otherwise. Whether rule 2 produced a checkout or a return is the kiosk's answer,
not the file's — which is the same property access-control's reason codes have.

**Single-line carts.** One add per cart, which is what most real transactions are
anyway. Multi-line would mean a `forEach` of adds and then some mechanism to fire
exactly one commit after the last of them — and the point of the pool arithmetic is
that nothing here needs that.

**Timeclock is one anonymous call, no chain:**

```yaml
payload: '{"user_code":"nw-marco","direction":"in","job_code":"WO-4471"}'
```

against `POST /api/kiosk/timeclock/punch`. Punches are driven at the **kiosks**,
not at the virtual terminal — see "What is deliberately not seeded, and not simulated".

### Topology is load-bearing

The kiosk binds `127.0.0.1` *because* `/api/kiosk/*` is anonymous: the box is the
trust boundary. So rule-router must sit on the kiosk's host. With all three kiosks
on one machine at different ports, one rule-router process reaches all of them.

Say this loudly in `demo/rules/README.md`: this is a scheduled **unauthenticated
write path** into a kiosk. access-control's driver publishes into an
account-scoped, credentialed NATS subject; this one does not, and must not
accidentally become a documented deployment pattern.

It is also the one place the kiosk demo diverges from its siblings, for a real
architectural reason rather than an oversight — and it means the platform-minted
credential in Phase 6 covers the kiosk's *outbound* events and its command subtree, not
the driver.

### Traps — every one of these fails silently

- **A NATS-triggered rule with an `action.http` needs `features.gateway: true`.**
  The HTTP executor is only attached to the broker when the gateway app is
  constructed; router-only leaves it nil and the action is skipped with a log
  warning and a successful return. The scheduler is unaffected — it builds its own
  executor — so cron→HTTP works while the follow-up hops do nothing. Enabling
  gateway also starts the inbound listener, so bind `http.server.address` to
  `127.0.0.1`.
- **`nats.publish.mode` must be `core`.** No stream covers `demo.>`, and a
  JetStream publish there hangs waiting for an ack that never comes.
  `publishResponse` has only a `subject` field — there is no per-action mode — so
  this is a global config setting, and the NATS triggers need `mode: core` to
  match.
- **`retry.maxAttempts` defaults to 1** — no retry at all unless you ask.
- **Scheduler HTTP actions are capped at ten seconds**, tighter than
  `http.client.timeout`.
- **Overlapping fires are dropped, not queued.** Each rule is a singleton; a fire
  that comes due while the previous is still running is dropped and counted at
  `scheduler_job_runs_total{status="singleton_rescheduled"}`.
- **`{@random.choice(...)}` arguments cannot contain a comma or a space.** They
  are literals split on `,` with no quoting and no escaping.
- **Numeric templates render bare; string templates must be quoted** in the JSON
  payload.
- **Six-field cron.** `*/5 * * * * *` is every five seconds; `*/5 * * * *` is every
  five minutes. Read the field count before assuming.

### Warm-up

Because the ledger starts empty (see "What is deliberately not seeded, and not simulated"), a cold
estate has thin screens.

**There is no second copy of the rules.** rule-router expands `${VAR}` in rule
files, so every cron in the set reads `"${DEMO_CRON}"` and the cadence is chosen at
launch:

```bash
DEMO_CRON="*/3 * * * * *"  rule-router --rules demo/rules   # warm up, ~10 min
DEMO_CRON="*/45 * * * * *" rule-router --rules demo/rules   # ambient
```

A parallel `demo/rules/warmup/` directory would mean keeping two copies of every
rule in step forever, to vary one string.

Ten minutes of warm-up produces a few hundred real transactions, real open
checkouts of realistic ages, and real punches — all through `commit.Commit`, none of it
fabricated.

### File structure

Mirror `access-control/demo/rules/northwind-access.yaml`: a fenced header block
carrying PREREQUISITES (with literal shell commands), the publish-mode rationale,
the cron field-count warning and the `features.gateway` warning; then `# ---`
section banners titled *NOUN PHRASE — the claim being made*; a one-line
name-and-reason comment before every rule; `timezone:` on every trigger even when
identical. Sections group by workflow, not by mechanism: THE MORNING RUSH → THE
CRIB → CONSUMABLES AT THE DOCK → THE CABINET (RFID) → SPRINGFIELD → THE TIMECLOCK.

One file per kiosk under `demo/rules/`, and a sibling `README.md` with a "what you
should see in the first minute" table. Three kiosks × three rules plus punches is a
small file — if it starts growing, that is the signal to re-read the pool
arithmetic rather than to add rules.

---

## Phase 6 — Northwind on the platform

In `platform/internal/demoseed/contract.go`.

**Subject mapping.** The kiosk's grammar is
`<prefix>.<kiosk_code>.<family>.<...>` — note there is **no location segment**,
unlike `acc.{location}.{type}.{thing}`. The kiosk code *is* the Thing code, so the
subject prefix is `kiosk.{thing}`. `kiosk.>` is free in Northwind's subject space.

**Operations** to add:

| Name | Capability | Suffix |
|---|---|---|
| `publish_kiosk_transaction` | publish | `event.transaction.complete` |
| `publish_kiosk_inventory` | publish | `event.inventory.adjust` |
| `publish_kiosk_instance` | publish | `event.instance.lifecycle` |
| `publish_kiosk_punch` | publish | `event.timeclock.punch` |
| `publish_kiosk_heartbeat` | publish | `heartbeat` |
| `reply_kiosk_command` | **reply** | `command.>` |

The `reply` capability is exactly right and worth noting rather than glossing: on
that platform it means precisely "the thing subscribes to its own subject and
publishes its answer to the requester's `_INBOX.>`", which is what the kiosk's
command dispatcher does.

**Open question to resolve when writing it:** `event.item.{action}` has a varying
final token (`checkout` / `return` / `consume` / `admin_close`). Either declare one
operation per action or use a wildcard suffix. The existing fixtures use literal
suffixes throughout, so a wildcard would be a new precedent — probably worth one
operation per action.

**Thing types:** `tool-kiosk` (kind gateway, prefix `kiosk.{thing}`),
`timeclock-terminal` (kind appliance, same prefix), `kiosk-controller` (kind app,
prefix `app.kiosk.{thing}`). Schemas can carry the things an integrator actually
records — reader make and antenna count, enclosure count, screen size, the
barcode-scanner model.

**Roles.** Two precise changes, and the second mirrors a bug the `gateway` role's
own comment already narrates:

- `gateway`: add `kiosk.>` to **both** publish and subscribe. It already carries
  `$JS.API.>`, `$KV.>` and `_INBOX.>`, which is what a kiosk needs to watch the
  catalogue buckets and answer commands.
- `application`: add `kiosk.>` **and `$KV.>`** to publish. It currently has
  `app.>`, `cmd.>`, `helpdesk.>`, `$JS.API.>` — and the controller *writes*
  `catalog_items`, `catalog_users`, `catalog_groups`, `punch_state` and
  `open_checkouts_state`. A KV write is a plain publish to `$KV.{bucket}.{key}`,
  which `$JS.API.>` does not cover. This is the identical failure the access
  controller hit: a box that boots clean, syncs its whole graph, and then cannot
  write state.

`TestEveryThingTypeCanSpeakItsOwnContract` checks both directions for every type
against the role it points at, so it will fail until the lists and the operations
agree. That is the safety net doing its job, not an obstacle.

**Things:** three kiosks and the timeclock terminal at their mapped locations, plus
the controller. Each kiosk then runs on a platform-minted signed credential, the
same as the four access controllers — and the Northwind account carries cold-chain
telemetry on `telemetry.>`, access traffic on `acc.>` and tool custody on
`kiosk.>` at once, which is what a tenant's bus actually looks like.

---

## Runbook

Each step is where it is for a reason; the reasons are above.

```bash
# 0. NATS. The platform's embedded server in operator mode, or a plain nats-server.

# 1. Controller: schema, then the catalogue. The seed ensures the stream and the
#    KV buckets itself, so it does not matter that the controller is not serving.
./kiosk-controller demo-seed --confirm   # applies its own migrations first

# 2. Kiosks: the reorder thresholds, which neither the catalogue wire nor any
#    command carries. Run BEFORE first start; the catalogue watcher upserts the
#    same rows by code and leaves reorder_threshold alone. See "The consequence,
#    stated plainly" above for why this step exists at all. (The maintenance
#    policy no longer needs it — that one rides the catalogue now.)
KIOSK_CONFIG=demo/kc-dc1-crib.yaml  ./kiosk-app demo-seed --confirm
KIOSK_CONFIG=demo/kc-dc1-dock.yaml  ./kiosk-app demo-seed --confirm
KIOSK_CONFIG=demo/sgf-xd2-crib.yaml ./kiosk-app demo-seed --confirm

# 3. Kiosks serve. They come up managed, watch `<code>.>` on catalog_items, and
#    project their own slice of the catalogue locally.
KIOSK_CONFIG=demo/kc-dc1-crib.yaml  ./kiosk-app
KIOSK_CONFIG=demo/kc-dc1-dock.yaml  ./kiosk-app
KIOSK_CONFIG=demo/sgf-xd2-crib.yaml ./kiosk-app

# 4. Controller: provision kiosk-local state over the command bus. Polls
#    inventory.snapshot per kiosk until the catalogue has landed, then fires
#    instance.create / inventory.adjust. Idempotent against step 2 — the
#    quantities already match and the units already exist, so this run reports
#    everything as already present. Run it on a fresh estate and it does the
#    whole job; it is also the demo beat worth showing.
./kiosk-controller demo-seed --remote --confirm

# 5. Controller and timeclock terminal serve. The controller's durable consumer
#    starts at the beginning of the stream, so it picks up every audit event the
#    seed produced in steps 2 and 4 — which is what EnsureStream in step 1 bought.
./kiosk-controller serve
KIOSK_CONFIG=demo/timeclock.yaml ./kiosk-timeclock

# 6. Warm up, then go ambient. Same rules, different cadence.
DEMO_CRON="*/3 * * * * *"  rule-router --config demo/rule-router.yaml --rules demo/rules
# ...ctrl-C after ~10 minutes...
DEMO_CRON="*/45 * * * * *" rule-router --config demo/rule-router.yaml --rules demo/rules
```

**Standalone variant** — one kiosk, no controller, no NATS, no fleet. This part
works today:

```bash
KIOSK_CONFIG=demo/standalone.yaml ./kiosk-app demo-seed --confirm
KIOSK_CONFIG=demo/standalone.yaml ./kiosk-app
# then http://127.0.0.1:8101 — scan nw-marco, then HT-1010.
# admin at /admin/login: admin@northwind.example / northwind-demo
DEMO_CRON="*/20 * * * * *" rule-router --config demo/rule-router.yaml --rules demo/rules/standalone
```

`demo-seed` applies the migrations itself, so there is no separate `migrate up`.

---

## What is deliberately not seeded, and not simulated

**No backdated ledger.** The kiosk ledger is the system of record and
`open_checkouts` is derived from it; hand-writing history means hand-writing
transactions, rebuilding the derived view and republishing it to the controller.
Ten minutes of warm-up gets the same screens through the real commit path, so the
ledger starts at zero and fills itself.

(The machinery exists if this ever becomes painful:
`handlers.PerformIntegrityRebuild` fully derives `open_checkouts` from
`transaction_lines`, and `ledger.republish` walks a date range and re-emits it
idempotently. `timeclock.PerformPunch` accepts an explicit `OccurredAt` for
`admin`-sourced punches, so a timesheet could be backdated through its real funnel.
None of that is needed for v1 of this demo.)

**No simulated operator.** Inventory adjustments, send-to-maintenance,
admin-close, the controller's remote commands.

**No simulated phone punches.** The virtual terminal's `/api/self/*` endpoints are
per-worker authenticated and read identity from `re.Auth`, never the body — a
driver would need a token per worker. More to the point, the presenter logging in
on a phone *is* the demo for that binary. Punch traffic is driven at the kiosks,
where the endpoint is anonymous, and the controller's fleet projection plus the
`punch_state` KV replica is what makes a clock-in at the crib and a clock-out at
the office work.

**No simulated tag movement.** The mock reader reads a file; a human edits the
file.

**No fabricated events**, on any subject, ever.

---

## Phasing

| Phase | Scope | Notes |
|---|---|---|
| 1 | `internal/demoseed` — fixture + both appliers | **Landed** |
| 2 | `kiosk-controller demo-seed` + exported `EnsureStream` | **Landed** |
| 3 | `kiosk demo-seed` + `demo/standalone.yaml` | **Landed** — the standalone demo runs |
| 4 | Mock RFID reader | Unblocks the enclosure_diff story |
| 5 | `demo/rules/` + `demo/*.yaml` configs | Needs 1–3; 4 for the cabinet rules |
| 6 | Platform Northwind thing types, operations, roles | Cross-repo |

Phases 1–3 land together: one fixture is useless without an applier, and the
standalone demo is the cheapest thing to be able to show.

## Before the rules file is written

One claim carries the whole driver and should be proved by a three-rule spike
against one kiosk before the full set is authored: **nested templating** —
`{cart.id}` resolving from `{"cart":{"id":"…"}}` inside an `action.http` payload.
The docs say url, method, headers and payload all go through the same template
engine and that dot-notation reaches nested fields, but this is the hinge the chain
turns on and it is ten minutes to confirm.

The kiosk's half of that claim is now confirmed against a seeded standalone node.
`POST /api/kiosk/cart/start` returns `{"cart":{"id":"…","user_code":"…",…}}` and
`/cart/add` returns the whole cart again under the same key alongside a `line`
object, so `{cart.id}` is the right path at both hops and the chain never needs a
value from two hops back. `/cart/commit` answers
`{"transaction_id":…,"lines_count":…,"checked_out":…,"returned":…,"consumed":…}`,
which is a useful thing to publish for a dashboard even though nothing consumes it.

Two other load-bearing claims were checked the same way, because they are cheaper
to confirm than to debug inside a rules file:

- **`defaultActionFor` really does close the loop.** A second scan of the same SKU
  by the same worker came back `"action":"return"` with no hint from the caller —
  for the quantity-tracked wrench and for the serialized torque wrench alike. This
  is the mechanism the pool arithmetic depends on.
- **The maintenance queue populates itself.** Returning `PT-2020-0001` answered
  with `maintenance_entered: [{… "reason":"auto: SKU requires maintenance on
  return"}]` and left the unit at `status: maintenance`. The fixture sets
  `requires_maintenance_on_return` on the digital torque wrench for exactly this:
  a maintenance bench with something on it, and not one simulated operator.

Everything else the driver relies on is a single documented behaviour with an
obvious failure mode: `publishResponse` republishing a 2xx body, and
`features.gateway` being on.

## See also

- [RFID](rfid.md) — reader modes, the diff, enclosure partitioning.
- [Wire reference](wire.md) — command payload and reply shapes.
- [Controller](controller.md) — catalogue publishing, membership, the command bus.
- [Ledger](ledger.md) — why nothing here writes `transactions` by hand.
