// Shared serialized-instance lifecycle vocabulary, used by both the local
// kiosk panel (ItemInstancesPanel) and the controller's remote panel
// (KioskInstancesPanel) so the two render identical badges and offer the same
// transitions. The backend derives the audit action verb from the
// (prev → target) transition, so callers only ever send the TARGET status —
// this module never names the verbs (to_maintenance / return_to_service /
// retire / unretire); it maps a current status to the targets reachable from
// it.

export type InstanceStatus = 'in_service' | 'maintenance' | 'retired'

export interface InstanceAction {
  // The status to transition to. The server picks the verb.
  target: InstanceStatus
  label: string
  // Visual tone for the action button. 'service' = positive/restore,
  // 'maintenance' = caution, 'retire' = destructive-ish.
  tone: 'service' | 'maintenance' | 'retire'
}

// instanceActions returns the transitions valid from a given status. "Out"
// (checked out) is NOT a status — it's derived from open_checkouts — so it
// never appears here.
export function instanceActions(status: InstanceStatus): InstanceAction[] {
  switch (status) {
    case 'in_service':
      return [
        { target: 'maintenance', label: 'Send to maintenance', tone: 'maintenance' },
        { target: 'retired', label: 'Retire', tone: 'retire' },
      ]
    case 'maintenance':
      return [
        { target: 'in_service', label: 'Return to service', tone: 'service' },
        { target: 'retired', label: 'Retire', tone: 'retire' },
      ]
    case 'retired':
      return [{ target: 'in_service', label: 'Un-retire', tone: 'service' }]
  }
}

export function statusLabel(status: InstanceStatus): string {
  switch (status) {
    case 'in_service':
      return 'In service'
    case 'maintenance':
      return 'Maintenance'
    case 'retired':
      return 'Retired'
  }
}

// statusBadgeClass returns the Tailwind classes for a status pill. Kept here so
// both panels (and any future surface) share one color language.
export function statusBadgeClass(status: InstanceStatus): string {
  switch (status) {
    case 'in_service':
      return 'bg-emerald-900/60 text-emerald-200'
    case 'maintenance':
      return 'bg-amber-900/60 text-amber-200'
    case 'retired':
      return 'bg-slate-800 text-slate-400'
  }
}

// actionButtonClass maps an action tone to button classes.
export function actionButtonClass(tone: InstanceAction['tone']): string {
  switch (tone) {
    case 'service':
      return 'bg-emerald-950/60 hover:bg-emerald-900/60 text-emerald-200 border-emerald-800/70'
    case 'maintenance':
      return 'bg-amber-950/60 hover:bg-amber-900/60 text-amber-200 border-amber-800/70'
    case 'retire':
      return 'bg-red-950/60 hover:bg-red-900/60 text-red-200 border-red-800/70'
  }
}
