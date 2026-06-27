# Configuration

Both binaries read a YAML config file from CWD: `kiosk.yaml` for the kiosk
binary, `controller.yaml` for the controller binary. Override the path with
`KIOSK_CONFIG=/path`.

## `kiosk.yaml`

```yaml
kiosk:
  code: "KC-MAIN-01"           # Stable identity. Stamped on every transaction.
  location_code: "MAIN"        # Site/yard identifier.

server:
  port: 8090
  bind: "127.0.0.1"            # Localhost only — the touchscreen is the only client.

session:
  idle_timeout: "5m"           # In-memory carts expire after this much inactivity.
  cart_grace_period: "30s"     # Success screen duration (frontend constant).

scanning:
  user_qr_prefix: "U:"         # Optional. If set and a scan starts with this,
                               # it's resolved as a user only.
  item_barcode_prefix: ""      # Optional. Same idea for items.

returns:
  allow_cross_user: true       # Allow foremen to return another worker's tool
                               # via the "Return on behalf of…" dialog (same-group only).
  allow_uncorrelated: true     # Accept returns of items not currently checked out.

controller:                    # Optional. Opt-in to central catalog sync.
  enabled: false               # When true, watches KV buckets below and projects updates locally.
  catalog_items_bucket: "catalog_items"   # JetStream KV bucket published by kiosk-controller
  catalog_users_bucket: "catalog_users"

branding:                      # Optional. Customize visual identity.
  logo_path: "./branding/logo.svg"   # Served by the binary at /branding/logo.
  tagline: "Tool & Consumable Checkout"  # Shown under the logo on the idle screen.
  primary_color: ""            # CSS color (e.g. "#059669"); empty = built-in default.
  custom_css_path: ""          # Optional .css file served at /branding/custom.css.
                               # See "Branding → Custom CSS overrides" below.

nats:                          # Optional. Off by default.
  enabled: false               # When true, events also publish to NATS.
  url: "nats://localhost:4222"
  # Use whichever auth your nats-server expects; leave blank for anonymous.
  token: ""
  username: ""
  password: ""
  credentials_file: ""         # JWT .creds file (NGS / JetStream Cloud)
  nkey_seed_file: ""           # ed25519 NKey seed
  tls_ca_file: ""
  tls_cert_file: ""
  tls_key_file: ""
  tls_insecure: false          # skip cert verification — dev only

rfid:                          # Optional. Off by default.
  enabled: false               # When true, the kiosk dials each configured reader at startup.
  read_window: "3s"            # One inventory cycle, shared across readers.
  readers:                     # reader_id -> one physical reader. A node can host
                               # several (a counter + enclosure cabinets); a single-
                               # reader kiosk declares one entry, selection implicit.
    front-counter:
      mode: "counter_scan"     # "counter_scan" | "enclosure_diff" (required)
      host: "10.0.0.50"        # Reader IP/hostname (required)
      port: 5084               # Standard LLRP port
      antennas: []             # Active reader ports + TX power. Empty = use reader baseline.
                               # Each entry: {id: 1, tx_power_dbm: 25.0}. Ids >= 1 + unique.
    cabinet-a:
      mode: "enclosure_diff"
      host: "10.0.0.51"
      port: 5084
      enclosure_id: "cabinet-a" # Required when mode=enclosure_diff (access-controlled cabinet id)

timeclock:                     # Optional. Off by default.
  enabled: false               # Gates the whole feature: endpoints, splash button, events.
  require_clock_in_for_checkout: false   # Reject checkout/consume when not clocked in (returns always allowed).
  block_clock_out_with_open_checkouts: false  # Reject clock-out while holding tools (fleet-wide in managed mode; fail-open).
  timeclock_only: false        # Dedicated punch station: SPA shows only the punch panel.
  virtual: false               # cmd/timeclock binary only — see timeclock.yaml below. Leave false here.
```

### Returns policy

The `returns.*` flags are enforced at commit time. With either flag set to
`false`, the matching transaction fails and rolls back; the cart is left
intact for the worker to fix and retry.

In addition to the kill-switch flags, two role-based rules always apply at
commit time:

- **Cross-user return** (active worker returning a tool checked out to a
  different worker) requires the active worker to be a `foreman` whose
  `group` matches the original checkout user's group. Both groups must be
  non-empty. An ungrouped foreman, or a foreman in a different group, is
  rejected — the admin handles that case.
- **Uncorrelated return** (no matching open checkout) requires the active
  worker to be a `foreman`. Group is irrelevant; this is a janitorial
  action.

These rules apply regardless of the kill-switch flags; setting
`allow_cross_user: false` simply short-circuits the foreman check by
rejecting any cross-user return outright.

Cross-user returns are an **explicit** kiosk action: scanning a tool
another worker has out defaults to `checkout` (for quantity-tracked tools
the natural reading is "give me one too"; for serialized the resolver
still routes to `return` only when the cart user owns the open checkout).
A foreman initiates a cross-user return through the on-screen "Return on
behalf of…" dialog, which lists workers in the foreman's group who have
items out and accepts a serialized scan as a one-step shortcut. The
dialog is the only client surface that populates
`original_checkout_user_id` on a cart line; the regular `/cart/add`
endpoint never does. See [api.md](api.md) for the endpoint signatures.

### NATS

The `nats.*` block enables an optional publisher. The kiosk's primary job is
the local ledger, so NATS is best-effort about **reachability**: an
unreachable server at startup does **not** block the kiosk from booting (the
connection enters a buffering state and dials in the background). All
`nats.go` auth modes are supported — provide whichever your server expects,
or leave them blank for anonymous.

**Structural** config errors are a different matter and are fatal at startup:
an empty/malformed `nats.url`, or a configured-but-unloadable
`credentials_file` or `nkey_seed_file`, aborts the binary rather than
silently connecting unauthenticated. (A bad nkey seed used to log a warning
and connect without nkey auth — it now fails loud.) Leaving the auth fields
blank is valid; pointing them at a broken file is treated as operator error.

### Controller opt-in

The `controller.*` block opts the kiosk into central management by the
kiosk-controller. When enabled, the kiosk's admin SPA hides Add/Edit/Delete
on items and workers and shows a "Catalog managed by controller" banner;
catalog changes flow in over JetStream KV instead. Requires `nats.enabled=true`
pointing at the same broker the controller publishes to. Stock adjustments
remain available at each kiosk regardless. See
[Central controller](controller.md) for the full picture.

### RFID

The `rfid.*` block opts the kiosk into one of two LLRP-driven inventory
flows. Disabled by default; when off the binary behaves exactly as it
does without any reader on the network.

- **`counter_scan`** — operator hits an "RFID scan" button on
  `CheckoutView`; the kiosk runs one inventory cycle and folds every
  matched EPC into the cart via the same path `cart/add` uses.
- **`enclosure_diff`** — NATS-driven. An access-control system
  publishes `cart.start` at the door, then a camera/occupancy system
  publishes `read.trigger` when the worker steps out. The kiosk
  reconciles observed EPCs against expected-present state and
  synthesizes checkout/return lines. Requires `nats.enabled=true`.

Validation at startup:

- `rfid.readers` must have at least one entry when `rfid.enabled=true`.
- Each reader needs `mode` (`counter_scan` | `enclosure_diff`), `host`, and `port`.
- A reader's `enclosure_id` is required when its `mode=enclosure_diff`.
- `rfid.read_window` (shared across readers) must be ≤ 3.5 s when any reader is `enclosure_diff`
  (`config.MaxEnclosureReadWindow`): the read runs synchronously inside the
  controller's ~5 s NATS command-reply window, so a larger window would push
  the reply past it and render the kiosk "offline" to the caller.
  `counter_scan` is HTTP-driven and not capped. Defaults to 3 s when unset.
- Connect failure at startup logs a warning but does not block the
  binary — RFID endpoints return 503 until the connection comes up.
  Mirrors how NATS unreachability is handled.

The identity payload served to the SPA grows `rfid_enabled` and
`rfid_mode` so the frontend gates affordances appropriately. See
[RFID](rfid.md) for the full design.

### Timeclock

The `timeclock.*` block opts the kiosk into an append-only clock-in/clock-out
punch ledger (`time_punches`) with the same discipline as the tool ledger:
punches are never edited or deleted, corrections are new punches with a
reason, and "who's clocked in" is derived from the latest punch (merged with
the fleet replica in managed mode). Off by default; every key is false so an
omitted block leaves a deployment unchanged.

- **`enabled`** gates the whole surface — endpoints, the splash-screen "Time
  clock" button, event publishing, and (in managed mode) the punch-state
  watcher. Off → the feature does not exist (endpoints 404).
- **`require_clock_in_for_checkout`** makes commit reject any cart containing
  checkout/consume lines when the cart user isn't clocked in. **Returns are
  always allowed** — a worker can hand a tool back regardless of punch state —
  and the checkout screen offers a one-tap "clock in now?" instead of a dead
  end.
- **`block_clock_out_with_open_checkouts`** rejects a clock-out while the
  worker holds open checkouts. In **managed mode** the gate is **fleet-wide**:
  it merges this kiosk's local checkouts with the controller's per-user
  open-checkouts replica, so a tool out at kiosk A blocks a clock-out at kiosk
  B (or on the phone terminal), and the worker is shown which building to
  return each tool to. It's eventually consistent and **fail-open** — if the
  replica is stale or the controller is unreachable, the clock-out is allowed
  rather than stranding the worker. **Standalone** (no controller) it's
  local-only. Admins bypass with `force=true` (the "drove home with a tool"
  escape hatch); a worker/foreman can "clock out anyway" to acknowledge their
  open tools and proceed (recorded on the punch).
- **`timeclock_only`** turns the device into a dedicated punch station: the
  SPA replaces the checkout splash with a persistent punch panel and badge
  scans go straight to it — no carts, no checkout. Requires `enabled`.
  (A punch-only station writes no local `open_checkouts`, but in managed mode
  `block_clock_out_with_open_checkouts` still blocks on the fleet replica;
  standalone it's a no-op.)
- **`virtual`** marks the dedicated **virtual timeclock terminal** (the
  `cmd/timeclock` binary), not a normal kiosk. Leave it `false` in
  `kiosk.yaml`. See the next section.

In managed mode (`controller.enabled=true` + NATS) punches flow to the
controller and per-user clocked-in state is distributed back to every kiosk
via the `punch_state` KV bucket, so **clocking in at one kiosk and out at
another just works**. Standalone kiosks keep a local-only ledger. There is no
payroll math anywhere — the raw-punch CSV export is the payroll contract.

### Virtual timeclock terminal (`timeclock.yaml`)

`cmd/timeclock` is a separate binary that serves a publicly-reachable,
per-user-**authenticated** self-service punch page (workers clock in/out from
their phones — no badge scanner). It reads `timeclock.yaml` (override with
`KIOSK_CONFIG`), uses its own data dir `pb_data_timeclock/`, and requires
`timeclock.enabled=true` + `timeclock.virtual=true`. It supports the **same
three operating modes** as `cmd/kiosk`, selected by the `nats`/`controller`
blocks:

| Mode | `nats.enabled` | `controller.enabled` | Workers from | Clocked-in state |
|---|---|---|---|---|
| standalone | false | false | local (admin SPA / superuser / CSV) | local ledger |
| standalone + NATS | true | false | local | local; punches emit events |
| controller-managed | true | true | catalog sync | fleet-wide via `punch_state` |

Managed mode is the one for a *public* terminal fronting a fleet:
provisioning stays central on the controller, and a worker who clocked in at a
physical kiosk can clock out from their phone. Workers authenticate against the
`users` collection via OAuth2 SSO and/or password — a `cmd/timeclock`-only
migration enables auth on that collection. `timeclock.yaml.example` is the
template; deploy-time hardening (TLS, OAuth2 provider secrets, SMTP for
password reset, firewalling the superuser UI) is covered in
[Operations](operations.md#virtual-timeclock-terminal).

## Environment overrides

Every YAML key has a `KIOSK_*` equivalent: prefix `KIOSK_`, replace dots with
underscores, uppercase. Env vars win over the file.

```
KIOSK_KIOSK_CODE=KC-YARD-03
KIOSK_KIOSK_LOCATION_CODE=YARD
KIOSK_SERVER_PORT=8091
KIOSK_RETURNS_ALLOW_CROSS_USER=false
KIOSK_BRANDING_LOGO_PATH=/etc/kiosk/yard-03.svg
KIOSK_BRANDING_TAGLINE=Yard 03 Crib
KIOSK_BRANDING_PRIMARY_COLOR=#1d4ed8
KIOSK_BRANDING_CUSTOM_CSS_PATH=/etc/kiosk/yard-03-theme.css
KIOSK_NATS_ENABLED=true
KIOSK_NATS_URL=nats://central.example.com:4222
KIOSK_NATS_CREDENTIALS_FILE=/etc/kiosk/nats.creds
KIOSK_CONTROLLER_ENABLED=true
KIOSK_CONTROLLER_CATALOG_ITEMS_BUCKET=catalog_items
KIOSK_CONTROLLER_CATALOG_USERS_BUCKET=catalog_users
KIOSK_RFID_ENABLED=true
KIOSK_RFID_READ_WINDOW=3s
# Per-reader RFID fields (mode/host/port/enclosure_id/antennas) live in the
# rfid.readers map and are YAML-only — there is no flat env path into a map
# entry. Only the top-level rfid.enabled / rfid.read_window have env overrides.
KIOSK_TIMECLOCK_ENABLED=true
KIOSK_TIMECLOCK_REQUIRE_CLOCK_IN_FOR_CHECKOUT=true
KIOSK_TIMECLOCK_BLOCK_CLOCK_OUT_WITH_OPEN_CHECKOUTS=true
KIOSK_TIMECLOCK_TIMECLOCK_ONLY=true
KIOSK_TIMECLOCK_VIRTUAL=true       # cmd/timeclock binary only
```

Other env vars:

| Variable | Purpose |
|---|---|
| `KIOSK_CONFIG` | Path to the YAML file. Default: `kiosk.yaml` (kiosk) / `controller.yaml` (controller) / `timeclock.yaml` (virtual timeclock terminal) |
| `KIOSK_QUIET_BOOTSTRAP` | If set, suppresses the bootstrap admin credentials print (used in tests) |
| `KIOSK_ROLE` | Set to `controller` by `cmd/controller` before config validation; relaxes the `kiosk.code` requirement. Not intended to be set by operators. |

Deployment pattern: one `kiosk.yaml` checked into config management with
sensible defaults; per-kiosk env vars set in the host's service definition.

## Branding

`logo_path`, `tagline`, and `primary_color` cover the common case. The
`branding/logo.svg` shipped in the repo is a generic example (Lucide wrench
+ "TOOL CRIB" wordmark). Replace it with your own PNG or SVG — point
`branding.logo_path` at the new file and restart. Leave any branding key
empty or omit the section entirely to get unbranded defaults.

For anything else — a different accent for secondary actions, surface tint,
typography tweaks — point `branding.custom_css_path` at a `.css` file. Both
binaries (kiosk + controller) stream it at `GET /branding/custom.css` and
the SPA injects a `<link rel="stylesheet">` for it **after** Tailwind, so
your rules win the cascade at equal specificity.

A starter file lives at `branding/theme.css.example` — copy it to
`branding/theme.css` (or wherever) and edit. The example demonstrates
both the recommended variables-only path and the fragile utility-class
approach as a commented-out section.

### Custom CSS overrides

Tailwind 4 emits every color utility as a `var(--color-<name>)` reference,
so overriding the variable cascades through every `bg-*`, `text-*`,
`border-*`, `divide-*` utility — and through their `/40`, `/50`, `/70`
alpha variants via `color-mix()` — without naming any utility class
directly. That's the stable surface; classes themselves are not.

The two slots we declare explicitly:

```css
:root {
  /* Primary action color (commit button, "New X" buttons, focus rings) */
  --color-brand-primary: #6d28d9;
  --color-brand-primary-hover: #7c3aed;
}
```

Tailwind's full palette is also addressable the same way. The SPA's
surface tones come from `slate`; chromatic accents come from `emerald`
(active/success), `red` (destructive), `amber` (warning), `sky` (managed
pill), and `indigo` (controller chip). To re-skin the whole surface
palette:

```css
:root {
  --color-slate-950: #100b21;  /* page background */
  --color-slate-900: #1a1530;  /* cards, dialogs */
  --color-slate-800: #2d2348;  /* buttons, table headers */
  /* ...and so on for slate-700, -600, ..., -100 */
}
```

For light-mode skins, **invert** the slate scale so that `slate-950`
(used for the page background) becomes the lightest tone and `slate-100`
(used for primary text) becomes near-black. The SPA's existing utility
usage then produces dark-text-on-light-background without any code
change.

You can also target Tailwind utility classes directly for things the
color variables don't cover (border-radius, spacing, animations). That
works but is **fragile** — class names change as the UI evolves, and we
don't treat them as a public API. If you find yourself overriding the
same class on a lot of selectors, file an issue and we'll consider
promoting it to a variable.

Cache-Control is 5 minutes; refresh the page after editing the file. The
SPA only injects the `<link>` when the server reports the file is
present on the identity payload, so removing `custom_css_path` from
config and restarting the binary cleanly reverts to defaults.
