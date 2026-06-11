// Three-state online indicator derived from the controller's heartbeat map.
// Shared by AdminKiosksView (list badges) and AdminKioskDetailView (header
// badge) so the thresholds can't drift between the two. The freshness
// threshold mirrors the controller side (heartbeatFreshness=90s in
// internal/controller/inventory.go).
//
// "Unknown" suppresses "offline" briefly after a controller restart so the
// SPA doesn't paint a freshly-booted fleet red — the controller's in-memory
// heartbeat map starts empty and takes up to one beat interval to repopulate.

export type OnlineStatus = 'online' | 'stale' | 'offline' | 'unknown'

// Two 45s beat intervals — one missed beat tolerated before "stale".
const FRESH_MS = 90_000
const STALE_MS = 5 * 60_000

export function onlineStatusFor(
  lastBeat: string | null | undefined,
  controllerStartedAt: string | null,
): OnlineStatus {
  if (!lastBeat) {
    if (!controllerStartedAt) return 'unknown'
    const sinceRestart = Date.now() - new Date(controllerStartedAt).getTime()
    return sinceRestart < FRESH_MS ? 'unknown' : 'offline'
  }
  const age = Date.now() - new Date(lastBeat).getTime()
  if (age < FRESH_MS) return 'online'
  if (age < STALE_MS) return 'stale'
  return 'offline'
}

export function onlineBadgeClass(s: OnlineStatus): string {
  switch (s) {
    case 'online':
      return 'bg-emerald-900/60 text-emerald-200'
    case 'stale':
      return 'bg-amber-900/60 text-amber-200'
    case 'offline':
      return 'bg-red-900/60 text-red-200'
    default:
      return 'bg-slate-800 text-slate-400'
  }
}
