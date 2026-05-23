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
  allow_cross_user: true       # Bob can return Alice's tool (with UI warning).
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

### NATS

The `nats.*` block enables an optional publisher. The kiosk's primary job is
the local ledger, so NATS is best-effort: an unreachable server at startup
does **not** block the kiosk from booting (the connection enters a buffering
state and dials in the background). All `nats.go` auth modes are supported —
provide whichever your server expects, or leave them blank for anonymous.

### Controller opt-in

The `controller.*` block opts the kiosk into central management by the
kiosk-controller. When enabled, the kiosk's admin SPA hides Add/Edit/Delete
on items and workers and shows a "Catalog managed by controller" banner;
catalog changes flow in over JetStream KV instead. Requires `nats.enabled=true`
pointing at the same broker the controller publishes to. Stock adjustments
remain available at each kiosk regardless. See
[Central controller](controller.md) for the full picture.

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
```

Other env vars:

| Variable | Purpose |
|---|---|
| `KIOSK_CONFIG` | Path to the YAML file. Default: `kiosk.yaml` (kiosk) / `controller.yaml` (controller) |
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
