# Docs

Topic-by-topic documentation for the kiosk binary, the controller
binary, and the operational surface around them. The repo-root
[`README.md`](../README.md) is the orienting overview; everything more
detailed lives here.

## Setup & operation

- [Configuration](configuration.md) — `kiosk.yaml` / `controller.yaml`,
  environment overrides, branding and custom CSS.
- [Development](development.md) — prerequisites, backend/frontend dev
  loops, DB reset, running the Go test suite.
- [Operations](operations.md) — deploying, backups, ledger integrity,
  resync to controller, NATS failure modes, stock adjustments,
  serialized instances, password reset.
- [Troubleshooting](troubleshooting.md) — common errors and what they mean.

## Reference

- [API reference](api.md) — custom `/api/kiosk/*` and
  `/api/controller/*` endpoints, PocketBase collection rules.
- [Wire reference](wire.md) — every NATS subject (events, commands,
  heartbeats) with payload and reply shapes. Read this if you're
  integrating an external system with the kiosk over NATS.
- [Schema](schema.md) — collections, controller-only fields,
  `open_checkouts` cardinality rules, CSV import format.
- [Ledger](ledger.md) — append-only design, the single write path,
  `open_checkouts` as a derived view, the controller's projected
  ledger, drift recovery.

## Subsystems

- [Central controller (kiosk-controller)](controller.md) — multi-kiosk
  deployments: catalog distribution, transaction aggregation,
  heartbeat/online status, remote inventory commands, NATS provisioning.
- [Notifications](notifications.md) — receipts, low-stock alerts,
  scheduled digests; standalone vs managed mode dispatch.
- [RFID](rfid.md) — Impinj R700 / LLRP integration with two
  operational modes (`counter_scan` and `enclosure_diff`), the SSE
  cart-events channel, and the `cart.start` / `read.trigger` NATS
  command pair. USB HID badge readers are also covered.

## Project status

- [Shipped & roadmap](roadmap.md) — what's already in the binary and
  what's deliberately deferred.
