// Inventory derivations shared between the local kiosk Items view
// (AdminItemsView) and the controller's per-kiosk inventory panel
// (KioskInventoryPanel), so the two render identical numbers from the same
// inputs. "out" is supplied by the caller — counted from open_checkouts on the
// kiosk, or derived from the controller's projected ledger and shipped in the
// snapshot — and these functions only do the presentation math the local view
// has always done. Keeping the formula in one place stops the two panels from
// drifting apart.

// availableFor is on-hand minus what's out, clamped at zero, for tools.
// Consumables have nothing "out" (no open_checkouts), so available is just the
// raw on-hand — which is allowed to be negative when over-consumed, matching
// the ledger-is-authoritative stance.
//
// `maintenance` is the count of serialized units parked in maintenance: they
// count toward on-hand (we still own them) but are not available to check out,
// so they're subtracted alongside `out`. Quantity-tracked tools and
// consumables have no instances, so callers pass 0 and the formula collapses to
// the old on-hand − out. For serialized tools this yields in_service − out.
export function availableFor(onHand: number, out: number, type: string, maintenance = 0): number {
  return type === 'tool' ? Math.max(0, onHand - maintenance - out) : onHand
}

// isLowStock gates on a positive threshold (0 = "no threshold set, never low")
// and compares against available, not raw on-hand — a tool with stock all
// checked out is low even though on-hand looks fine.
export function isLowStock(available: number, threshold: number): boolean {
  const t = threshold ?? 0
  return t > 0 && available <= t
}
