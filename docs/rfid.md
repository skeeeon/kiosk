# RFID

This doc describes how RFID is integrated into the kiosk, the two
operational modes the binary supports, and the phased rollout plan. It
assumes familiarity with [Schema](schema.md) and the architecture
overview in [CLAUDE.md](../CLAUDE.md).

RFID at this kiosk is **serialized-only by construction**. A tag carries
unique identity by definition; if every unit has a tag, every unit is
distinguishable, which is the definition of serialized. The data model
is one tag = one `item_instances` row, matched on the existing
`rfid_epc` field. Consumables and quantity-tracked tools never
participate in RFID flows.

EPC matching is **case-insensitive in effect**: `item_instances.rfid_epc`
is normalized to lower-case, trimmed hex on every write (model hooks in
[`internal/instances/hooks.go`](../internal/instances/hooks.go), covering
both the admin SPA's PocketBase writes and the controller command bus),
backfilled by [`migrations/1792000000_lowercase_rfid_epc.go`](../migrations/1792000000_lowercase_rfid_epc.go).
The LLRP reader emits lower-case hex and both match paths
(`scan.scanInstanceByRFID`, `expectedInstanceStates`) fold case
defensively, so a tag registered in any case resolves reliably against an
observed read.

There are two distinct RFID input surfaces, deliberately not unified:

- **USB HID badge reader.** A card scan emits keyboard events just like
  a barcode gun, lands at the existing window-level scan composable in
  the SPA, and resolves through `scan.Resolver` against a user `code`
  or an instance `rfid_epc`. Nothing new at this layer — buy the right
  reader, register the card's internal ID into PocketBase. This is the
  cart-start input.
- **Networked Impinj R700 (or another LLRP reader).** Talks to the
  kiosk binary directly over LLRP, owned by the kiosk process. This is
  the inventory input, used for either of the two modes below.

The rest of this doc is about the second surface.

## Modes

Both modes operate on the same in-process LLRP connection and the same
resolver. They differ in who initiates the read and what the read
means.

### `counter_scan` — operator-driven scan window

A countertop reader, antenna pointed at a small work surface. The
operator badges in at the touchscreen, hits an "RFID scan" button on
the cart screen, and the kiosk runs a single 3-5 s inventory cycle
against the reader. Every observed EPC is fed through the existing
`scan.Resolver`; each matched instance becomes a cart line via the
same path that `POST /api/kiosk/cart/add` uses today. The
[`defaultActionFor`](../internal/handlers/cart.go) logic decides
checkout vs return for each line. Worker confirms the cart on the
touchscreen, normal commit path runs.

This mode is essentially "the barcode gun is now an RFID antenna" —
the cart-write side of the system doesn't know or care that the input
arrived in bulk over LLRP rather than one keypress at a time.

### `enclosure_diff` — system-driven diff against expected state

A reader mounted inside an RF-isolated enclosure (smart cabinet, tool
locker). The worker stands outside; the reader inside sees what's
present. The flow is event-driven and NATS-orchestrated:

1. **Cart start.** An access-control event (badge scan at the
   enclosure door, handled by an external system) publishes a
   `cart.start` NATS command at
   `<prefix>.<kiosk_code>.command.cart.start` carrying `user_code` and
   `enclosure_id`. The kiosk's command dispatcher creates or reuses a cart
   keyed `(user_code, enclosure_id)` — re-fires within the active window
   return the same cart_id. The cart is empty at this point.
2. **Worker uses the enclosure.** Takes things out, puts things back.
   No kiosk involvement; the camera / occupancy system handles this
   outside the binary.
3. **Read trigger.** When occupancy ends (worker has stepped out), the
   external system publishes a `read.trigger` NATS command at
   `<prefix>.<kiosk_code>.command.read.trigger` carrying the
   `(user_code, enclosure_id)` of the active cart, or the `cart_id` if
   known. The kiosk runs one LLRP inventory cycle.
4. **In-process diff.** Pure function over (observed EPCs, expected
   set). Expected-present = non-retired serialized instances (in_service +
   maintenance) on this kiosk whose `id` is **not** currently in
   `open_checkouts`. When the node hosts **more than one cabinet**
   (`enclosureCount() > 1`), that set is further scoped to instances whose
   `item_instances.enclosure_id` matches the cabinet being read — so a read
   of cabinet A never treats cabinet B's tools as missing. A single-cabinet
   node (and any unassigned instance) keeps the whole-inventory set, so
   nothing changes there. Assign a unit to a cabinet from the instance admin
   UI (the "Enclosure" field, local or controller) or via CSV / the PB
   superuser UI. A maintenance unit is expected-present (it's
   physically in the enclosure) but **not** checkout-eligible: if it
   leaves the enclosure it is skip-and-counted (recorded in a
   `SkippedIneligible` bucket, surfaced to the operator), never
   synthesized as a checkout — commit would reject a checkout of a
   non-`in_service` unit anyway. The diff yields:
   - **observed and expected-present** → no-op (still there).
   - **observed but expected-absent** (i.e., currently in
     `open_checkouts`) → return line.
   - **expected-present but not observed** → checkout line.
   - **observed but unresolvable EPC** → log, drop. Strays don't
     touch the cart.
5. **Confirm.** The diff has populated the cart. The worker confirms
   on a screen outside the enclosure (same `CheckoutView`, no new
   view); normal commit runs.

The kiosk does not auto-commit. A read result that nobody confirms
times out via the existing session idle timeout, same as any other
abandoned cart.

A manual "trigger another read" button on the outside-enclosure
screen calls the same `read.trigger` handler over HTTP, useful when
the operator wants to re-run after intervening (took an extra thing,
put something back) before confirming.

## Trust boundary

The kiosk binary still binds HTTP to `127.0.0.1` and treats the box
itself as the trust boundary. Mode B's external integrations
(access-control, camera/occupancy) do not reach the kiosk via HTTP at
all — they publish NATS commands, subject to the same per-subject
ACLs that already gate `controller → kiosk` traffic. From the kiosk's
perspective, a `cart.start` from access-control is dispatched
identically to a `cart.start` someone might fire from the controller
binary: same handler, same idempotency, same dispatcher.

The LLRP TCP connection is outbound from the kiosk to the reader on
the LAN. The kiosk initiates; the reader does not need to reach the
kiosk on any new port.

## Code seams

```
internal/rfid/                  domain wrapper, public to other internal packages
  reader.go                     Reader interface + impinjReader impl + EPC type
  diff.go                       enclosure_diff pure function
  diff_test.go                  pure-function unit tests, no I/O
  reader_test.go                wrapper tests; LLRP integration deferred to RFID_SIM=1

internal/cartevents/            tiny SSE-driven pub/sub broker
  broker.go                     Subscribe / Tickle / Close
  broker_test.go

internal/handlers/
  rfid_scan.go                  counter_scan endpoint + PerformRFIDScan core
  rfid_read_trigger.go          enclosure_diff PerformReadTrigger core + ReadTrigger HTTP wrapper
  cart_events.go                SSE endpoint (GET /api/kiosk/cart/events)
  cart.go                       gains CartGet (GET /api/kiosk/cart) for SSE-driven refetch

internal/commands/
  rfid_enclosure.go             cart.start + read.trigger handlers (one file, two methods)
  dispatcher.go                 gains KioskHandlers field for those handlers to reach in

internal/cart/
  store.go                      gains StartByExternal + GetByUserEnclosure + secondary byUserEnclosure index

internal/config/
  config.go                     RFIDConfig block + KIOSK_RFID_* env overrides + cross-field validation

internal/events/
  subjects.go                   ScanRFIDObservedSubject, CartStartCommandSubject, ReadTriggerCommandSubject

cmd/kiosk/main.go               wires rfid.Reader, cart-events broker, enclosure command deps

ui/src/composables/
  useCart.ts                    refresh(), rfidScan(), readTrigger()
  useCartEvents.ts              EventSource wrapper

ui/src/views/CheckoutView.vue   "RFID scan" (counter_scan), "Re-read enclosure" (enclosure_diff),
                                SSE subscription via useCartEvents
ui/src/types.ts                 KioskIdentity.rfid_enabled / .rfid_mode, RFIDScanResult, ReadTriggerResult
```

The `Reader` interface in `internal/rfid` stays small (`Connect`,
`ReadFor(d) ([]EPC, error)`, `Close`) so handler and command tests can
mock it cleanly without spinning up an LLRP simulator. The EdgeX
`llrp.Client` is an implementation detail of `impinjReader` and is not
re-exported from `internal/rfid`.

## Schema

No new collections. No migrations. `item_instances.rfid_epc` already
exists; the diff is computed against existing PB tables. The cart
remains in-memory; the in-memory store grows a secondary lookup key
to support `(user_code, enclosure_id)` cart resolution in `enclosure_diff`
mode, but the persistence story is unchanged.

## Config

The `rfid:` block goes in `kiosk.yaml`. Disabled by default; when off,
the kiosk binary behaves exactly as it does today.

```yaml
rfid:
  enabled: false
  read_window: "3s"         # one inventory cycle, shared across readers
  # readers maps a reader_id to one physical reader. A node can host several —
  # a counter plus one or more enclosure cabinets — each with its own mode.
  # A single-reader kiosk declares exactly one entry (selection is implicit).
  readers:
    front-counter:
      mode: "counter_scan"  # "counter_scan" | "enclosure_diff"
      host: "10.0.0.50"     # reader IP / hostname
      port: 5084            # standard LLRP port
      # antennas (optional): active reader ports + the TX power each should run
      # at. Empty/omitted leaves the reader's own baseline alone. When set, only
      # the listed ports are inventoried, each at the given dBm (resolved to the
      # reader's nearest available power index at Connect time).
      # antennas:
      #   - id: 1
      #     tx_power_dbm: 25.0
      # zone (optional): a coarse location label. When set, a read here also
      # stamps each observed unit's advisory "last seen" at this zone — free
      # location data from custody activity. See docs/location-sightings-plan.md.
      # zone: "Main Crib"
    cabinet-a:
      mode: "enclosure_diff"
      host: "10.0.0.51"
      port: 5084
      enclosure_id: "cabinet-a"  # required when mode=enclosure_diff
```

Validation at startup:

- When `rfid.enabled=true`, `rfid.readers` must have at least one entry.
- Each reader requires `mode` (`counter_scan` | `enclosure_diff`), `host`, and `port`.
- A reader's `enclosure_id` is required when its `mode=enclosure_diff`.
- A reader's `zone` is optional (location/sightings); empty = no location stamping.
- When any reader is `enclosure_diff`, the shared `rfid.read_window` must be ≤ 3.5 s
  (`config.MaxEnclosureReadWindow`) — the read runs synchronously inside the
  controller's ~5 s command-reply window, so a larger window would push the
  reply past it. `counter_scan` is HTTP-driven and not capped. Defaults to
  3 s when unset.
- Antenna entries (when listed) must have `id >= 1`, unique IDs, and
  `tx_power_dbm > 0`. The actual reader-side ceiling (FCC ~33 dBm,
  ETSI ~31.5 dBm at port) is enforced at Connect against the reader's
  power table, not statically — see the Reader lifecycle section.
- Failure to connect to the reader on startup **does not block the
  binary**. The kiosk supervises the LLRP session in a background
  goroutine that retries on failure with exponential backoff, so a
  kiosk that boots before its reader is online will self-heal once
  the reader appears. RFID endpoints return `ErrNotConnected` (503 to
  the SPA) during any gap and recover transparently when the session
  is re-established.

Env-var overrides cover the top-level toggles only — `KIOSK_RFID_ENABLED`
and `KIOSK_RFID_READ_WINDOW`. Per-reader fields live in the `rfid.readers`
map and are YAML-only (there's no flat env path into a map entry).

The identity payload served to the SPA grows `rfid_enabled` and
`rfid_mode` so the frontend gates affordances appropriately.
`counter_scan` shows an "RFID scan" button on `CheckoutView`;
`enclosure_diff` shows a "Re-read enclosure" button as a manual
fallback alongside the primary NATS-driven `read.trigger`. Both
modes share the 3-second countdown styling; the button is hidden
entirely when RFID is disabled.

**Reader selection (multi-reader nodes).** `rfid_mode` in identity is the
*sole* reader's mode — meaningful only when the node has exactly one reader.
A node with more than one reader (e.g. two crib windows) wires each
touchscreen to its reader with a `?reader=<reader_id>` URL param, alongside
the existing `?terminal=` attribution param. The "RFID scan" button then
posts `POST /api/kiosk/cart/rfid-scan?cart_id=…&reader=<id>` and the kiosk
fires exactly that reader (`Handlers.ReaderByID`). With one reader the param
is omitted and selection is implicit — N=1 needs no URL params at all. The
button shows when the sole reader is `counter_scan` *or* a `?reader=` is
present; the server validates the named reader is actually `counter_scan` and
404s a misconfigured screen. (The `enclosure_diff` "Re-read" button needs no
`?reader=` — it resolves the reader from the cart's `enclosure_id` via
`ReaderForEnclosure`. Its visibility is gated on the **active cart** carrying
an `enclosure_id` — i.e. a server-started enclosure_diff cart — rather than on
the node's sole-reader `rfid_mode`, so it works even at a node that mixes
`counter_scan` + `enclosure_diff` readers, where `rfid_mode` is blank.)

## Reader lifecycle

The kiosk maintains one long-running LLRP TCP session per configured
reader, not a connect-per-read model. Each entry in `rfid.readers` is
its own `impinjReader` (`internal/rfid/reader.go`) owning a session
through a supervisor goroutine started by `Connect` and cancelled by
`Close`; the read lock (`readMu`) is therefore per-reader, so two
enclosures on one node can read concurrently.

**Supervisor loop.** On each iteration:

1. Dial the reader (5 s per-attempt timeout).
2. Start the EdgeX `*llrp.Client` against the TCP conn.
3. If the operator configured antennas, send
   `GET_READER_CAPABILITIES` and resolve each requested `tx_power_dbm`
   against the reader's `TransmitPowerLevelTable` to an LLRP power
   index (5 s timeout). The resolver picks the highest entry **at or
   below** the request — never silently exceeding — and clamps up
   with a log line if every entry is above (request below the
   reader's minimum). Hopping vs fixed-frequency region info comes
   from the same response.
4. Publish the live `*llrp.Client` + resolved per-antenna `txConfig`
   under the reader's mutex. `ReadFor` snapshots both and proceeds.
5. Park on the connection's exit channel until either it drops
   (retry) or the supervisor's context is cancelled (Close).

On dial or capabilities failure, the supervisor logs and retries with
exponential backoff: 1 s → 2 s → 4 s → ... → 30 s ceiling. A
successful connect resets backoff to 1 s, so a transient blip during
reconnect doesn't penalize the next attempt.

**Why a supervisor and not just retry-on-demand.** The reader is
long-lived shared state. A kiosk that boots before its reader is
ready — or one whose reader power-cycles mid-shift — must not require
a manual binary restart. Operator-driven retry inside the HTTP
handler would also pay the dial latency on every cart-screen visit.

**Why inline AntennaConfiguration, not `SET_READER_CONFIG`.** Antenna
power could be baked into the reader's NVRAM (web UI / IoT REST) or
pushed once via LLRP `SET_READER_CONFIG` per session. Instead the
kiosk embeds an `AntennaConfiguration` block inside every ROSpec's
`InventoryParameterSpec`. Tradeoff:

- Reader reboots become transparent — the next `ReadFor` re-applies
  the tuning. No supervisor logic to re-push baseline state on
  reconnect.
- `kiosk.yaml` is the source of truth, not the box. Swapping a reader
  (RMA, hardware refresh) doesn't require re-provisioning; the same
  YAML configures it.
- Variable installs (1–4 antennas per room) are a config problem, not
  a code problem. Each antenna gets its own dBm — overhead and
  side-mount antennas in the same cabinet routinely run at different
  power to avoid bleed.

The cost — one extra parameter block per ROSpec on the wire — is
negligible at our read cadence (operator-driven, low frequency).

**When the operator leaves `antennas:` empty**, the ROSpec falls back
to `AntennaIDs: [0]` ("all antennas, reader's own baseline") and emits
no per-antenna override. The supervisor also skips the capabilities
round-trip in that case — there's nothing to resolve.

## NATS subjects

Two new families enter the picture. Both follow the existing
`<prefix>.<kiosk_code>.<family>.<...>` shape with the same family
semantics already documented in CLAUDE.md.

**Commands** (`enclosure_diff` only, request/reply, ≤5 s timeout):

| Subject | Payload | Reply |
|---|---|---|
| `<prefix>.<code>.command.cart.start` | `{user_code, enclosure_id, command_id}` | `{success, error, data: {cart_id, user_code, enclosure_id, reused}}` |
| `<prefix>.<code>.command.read.trigger` | `{cart_id}` *or* `{user_code, enclosure_id}` (+ optional `command_id`) | `{success, error, data: {cart, added_lines, observed_epcs, unresolved_epcs, skipped_cross_user_count}}` |

Both commands MUST reply within the 5 s window even on error —
silence renders "kiosk offline" at the caller. To hold that contract for
`read.trigger` even with a slow or half-open reader, the handler bounds the
LLRP read with a 4.5 s deadline (`commands.ReadTriggerBudget`, below the 5 s
window): past it the read unwinds, releases the reader's serialization lock,
and replies with an error rather than hanging the caller. The `read_window`
≤ 3.5 s cap above keeps a normal read comfortably inside that budget.

**Events** (fire-and-forget, JetStream-bound):

| Subject | When | Payload |
|---|---|---|
| `<prefix>.<code>.event.scan.rfid.observed` | After every completed read in either mode | `{kiosk_code, location_code, cart_id, enclosure_id, mode, observed_epcs, observed_at}` |

The observed-EPCs event is cheap observability — it gives the
controller (or any downstream consumer) a stream of "what tags have
been seen where" without any in-process cost. No projector consumes
it today; it lands in the JetStream stream and is available for
future drift detection or analytics. Subject builders go in
`internal/events/subjects.go` alongside the existing helpers.

## SSE channel

Mode `enclosure_diff` breaks the SPA's "all cart state changes are
SPA-initiated" assumption — NATS-driven cart starts and reads happen
without the SPA's knowledge. A small SSE endpoint covers it:

- `GET /api/kiosk/cart/events?cart_id=...` streams `cart.updated`
  and `cart.gone` tickles. Plain `text/event-stream`; 15-second SSE
  comment heartbeats keep idle proxies from closing the connection.
- `GET /api/kiosk/cart?cart_id=...` is the matching refetch endpoint
  the SPA hits on every tickle.
- Broker is `internal/cartevents`: `Subscribe(cartID) → (chan, unsub)`,
  `Tickle(cartID)`, `Close(cartID)`. Drop-on-full per-subscriber
  buffering so a slow client never backs up a hot write path.
- Tickle fires from every cart write path (CartAdd, CartUpdateLine,
  CartDeleteLine, CartForemanReturn, PerformRFIDScan,
  PerformReadTrigger) — exactly once per HTTP/NATS call, not once
  per cart line. Close fires from CartCommit and CartCancel.
- SPA opens the EventSource when `CheckoutView`'s cart_id becomes
  non-null, refetches via the GET endpoint on every tickle, drops
  the subscription on unmount or on `cart.gone`. Plain
  `EventSource` — no library. The browser auto-reconnects only on
  network-level drops; for a non-2xx response (a transient 503 on
  restart, or a 404 for an unknown cart_id) it sets `readyState` to
  CLOSED and gives up. So the composable (`useCartEvents.ts`) detects
  CLOSED and reconnects itself with capped exponential backoff
  (1.5 s → 30 s), refetching once on recovery to catch writes missed
  while the stream was down. `cart.gone` is the one terminal case where
  it stops for good.
- `counter_scan` also fires the tickle even though the button press
  is local; this keeps the broker uniform and supports any future
  "spectator" client (e.g., a controller-side watch-this-kiosk view).

The cart stays in-memory. Persisting it to PocketBase to leverage
PB realtime would solve the delivery problem but cost the in-memory
invariant — adding concurrency, schema, cleanup, and the temptation
to grow more functionality on top of a "persistent" cart. SSE-with-
tickle keeps the invariant intact.

## LLRP library

The kiosk uses the LLRP protocol implementation from EdgeX Foundry's
`device-rfid-llrp-go` repository. As of v4, the LLRP package lives at
`github.com/edgexfoundry/device-rfid-llrp-go/pkg/llrp` — a stable
public API. We **import it directly**, pinned via `go.mod`:

```
go get github.com/edgexfoundry/device-rfid-llrp-go/pkg/llrp@main
```

Note the `@main` pseudo-version rather than a clean semver tag.
EdgeX's `v4.x` tags exist but are unreachable to Go's module system
because their `go.mod` declares `module
github.com/edgexfoundry/device-rfid-llrp-go` without the `/v4`
major-version suffix Go semver-major requires. The proxy serves only
the older `v1` tags (which predate the move from `internal/llrp` to
`pkg/llrp`). Pinning at `@main` resolves to a `v1.0.1-0.<timestamp>-<sha>`
pseudo-version that Go is happy with. If EdgeX ever ships a release
that fixes the module path, bump to that tagged version.

We pick the EdgeX implementation over the available standalone Go
LLRP libraries because:

- It is actively maintained as part of an LF-backed project.
- It is explicitly tested against Impinj hardware, including the R700.
- Apache-2.0 license is friction-free for our use.
- The package's production code imports only the Go standard library —
  EdgeX's enormous device-service transitive dependency tree
  (mongodb, OpenTelemetry, Zitadel, etc.) belongs to the driver code,
  not the LLRP package, so importing pulls nothing extra into our
  go.mod.

We previously considered vendoring (copying source into our own
`internal/llrp/`) when the LLRP code was still under EdgeX's
`internal/` directory. With it now in `pkg/llrp`, importing is the
simpler, more idiomatic choice. Version pinning in `go.mod` covers
the "what if upstream breaks us" concern that vendoring would have
addressed — we bump on our schedule, not theirs.

We do **not** write our own LLRP. The spec is large, binary, and
unforgiving — reinventing it earns no benefit.

## Phasing

The feature lands in five phases. Each phase is independently
mergeable; nothing below phase N depends on phase >N being committed
first.

### Phase 1 — Foundation

**Scope.** `go get` the EdgeX LLRP package. Write the `internal/rfid`
wrapper: `Reader` interface (`Connect`, `ReadFor`, `Close`),
`impinjReader` implementation around `*llrp.Client` covering Connect
and Close, plus a stub `ReadFor` that returns "not implemented" — the
LLRP message dance for an inventory cycle lands with Phase 2 where
the first caller appears. Add the `RFIDConfig` block to
`internal/config` with env-var overrides and cross-field validation.
Extend `kiosk.yaml.example` accordingly. Wire reader construction +
lifecycle in `cmd/kiosk/main.go` gated on `rfid.enabled`. Extend the
identity payload with `rfid_enabled` and `rfid_mode` so Phase 2's
frontend gating is ready.

**Behavior change.** None. Code is dormant when the feature flag is
off. When on, the kiosk attempts a reader connection on startup and
fails soft (warn + continue, like NATS unreachability), but no
endpoints or commands consume the reader yet.

**Tests.** Config validation (enabled/disabled, mode requirements,
enclosure_id requirement when `mode=enclosure_diff`, env overrides).
Wrapper construction from a `RFIDConfig` value. No LLRP-level
integration tests in this phase — those land with Phase 2's `ReadFor`
implementation against the EdgeX simulator, gated on an opt-in
`RFID_SIM=1` env var that mirrors the pattern PB tests already use
for slower paths.

**Size.** Small.

### Phase 2 — `counter_scan` end-to-end

**Scope.** Implement `impinjReader.ReadFor`: send AddROSpec /
EnableROSpec, collect ROAccessReport messages for the configured
window, decode TagReportData EPCs, send DisableROSpec / DeleteROSpec.
New endpoint `POST /api/kiosk/cart/rfid-scan?cart_id=...` calls
`rfid.ReadFor`, loops EPCs through `scan.Resolver`, adds each matched
instance to the cart via the same write paths `cart/add` uses today.
Returns the new lines. Publishes `event.scan.rfid.observed` after
every read. Frontend gets an "RFID scan" button on `CheckoutView`,
gated on `rfid_enabled && rfid_mode === 'counter_scan'` from the
identity payload. Button shows a brief "Reading… 3s" toast styled
like the existing cart-complete countdown.

**Tests.** Handler tests with a fake `rfid.Reader`. End-to-end test
of the scan → resolve → cart-add path against a real PB app per the
`commit_test.go` pattern. `ReadFor` integration tests against the
EdgeX LLRP simulator, gated on `RFID_SIM=1`.

**Size.** Small-medium.

### Phase 3 — SSE infrastructure

**Scope.** `GET /api/kiosk/cart/events?cart_id=...` SSE endpoint.
Server-side broadcaster threaded through every cart write path. SPA
composable `useCartEvents` wraps `EventSource`; `CheckoutView`
subscribes on mount and refetches on tickle. Phase 2's
`/cart/rfid-scan` fires the tickle for uniformity.

**Tests.** End-to-end SSE test using `httptest`: open subscription,
mutate cart, assert tickle arrives.

**Size.** Small.

### Phase 4 — `enclosure_diff` end-to-end

**Scope.** Two new commands on the kiosk-side `Dispatcher`:

- `cart.start`: payload `{user_code, enclosure_id, command_id}`.
  Idempotency: the cart store grows a secondary index keyed
  `(user_code, enclosure_id)`; a re-fire within the cart's active window
  (governed by `session.idle_timeout`) returns the existing cart_id
  rather than creating a new one. After commit or idle expiry, the
  next `cart.start` for the same key creates a fresh cart.
- `read.trigger`: payload `{cart_id}` or `{user_code, enclosure_id}`.
  Looks up the active cart; **rejects with error if no active cart
  exists for the key.** Runs `ReadFor`, computes the diff, synthesizes
  cart lines, fires the SSE tickle. We deliberately do not start an
  anonymous cart on read trigger — that path would silently mask
  wiring mistakes (access-control event lost, race with cart
  timeout). Failing loud surfaces the problem.

The diff itself is a pure function in `internal/rfid/diff.go`:
exhaustive unit tests cover every observed/expected combination.
Stray observed EPCs that don't resolve are logged at warn level and
dropped, never affecting the cart.

**Cross-user returns: skip + count.** When the diff sees a returned
tag whose `open_checkouts` row belongs to a worker other than the
cart user, we skip the line and increment `SkippedCrossUserCount`
in the response. The commit-time foreman+same-group gate would
reject such a line anyway, so synthesizing it would just produce a
cart the worker can't commit. The count surfaces in the SPA toast
("3 cart lines from 5 observed (1 skipped — held by another
worker)") so the operator sees the read wasn't a black hole. If
real demand for cross-user enclosure returns emerges later, the
right pattern is probably a foreman-only mode toggle, parallel to
the existing `CartForemanReturn` dialog — not silent acceptance.

The outside-enclosure screen is the existing `CheckoutView`. The
manual "Re-read enclosure" button calls the same `read.trigger`
logic over HTTP (`POST /api/kiosk/cart/read-trigger?cart_id=…`) for
parity with the NATS-driven path.

**Tests.** Diff function tests (exhaustive). Command handler tests
with a fake `rfid.Reader` and a real PB app. Idempotency test for
`cart.start`. Anonymous-read rejection test for `read.trigger`.

**Size.** Medium.

### Phase 5 — Docs and roadmap

**Scope.** Move RFID from the "Roadmap" section to "Shipped since v1"
in `docs/roadmap.md`. Update CLAUDE.md's "Architecture you can't see
from one file" section with the new command family entries and the
SSE channel. Verify `docs/rfid.md` matches what actually shipped.

**Size.** Tiny.

## Testing strategy

The EdgeX `pkg/llrp` package is itself well-tested upstream; the
integration risk lives in our wrapper and in the diff. Both are
designed to be testable without hardware:

- **`internal/rfid/diff.go`** — pure functions over slices.
  Exhaustive unit tests with no I/O. This is where the real
  correctness risk lives — much more than in the LLRP comms.
- **`internal/rfid.Reader` wrapper** — small enough interface to mock
  in handler tests. Optional EdgeX-simulator integration tests gated
  on `RFID_SIM=1`, opt-in for CI.
- **Handler and command tests** — use real PB apps per the
  `commit_test.go` pattern; mock the `Reader` interface for
  determinism.
- **SSE endpoint** — `httptest` end-to-end.

Frontend has no test suite per CLAUDE.md; SPA correctness rides on
`vue-tsc` and integration verification.

## Out of scope

- Drift detection, cross-fleet instance movement, controller-side qty
  projection. Separate roadmap items.
- Camera / access-control integrations. These are external systems
  that publish to the NATS command subjects defined here. We define
  the command shape; we don't wire the camera side.
- Antenna placement, cabling, regulatory region selection, EPC
  filter prefixes. Handled via physical design and the reader's own
  provisioning surfaces (web UI / IoT REST). TX power per antenna is
  in-binary — see Reader lifecycle.
- LLRP-over-MQTT or any alternate reader transport. We picked direct
  LLRP and we're committing.
- Persisting carts to PocketBase. The in-memory invariant stays.
- Auto-commit of `enclosure_diff` carts. A worker always confirms.
