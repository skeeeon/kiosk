export interface KioskBranding {
  logo_url?: string
  tagline?: string
  primary_color?: string
  // Set when the operator configured branding.custom_css_path. The SPA
  // injects a <link rel="stylesheet"> for this URL after Tailwind so the
  // file can override CSS variables and utility classes.
  custom_css_url?: string
}

// Identity payload returned by /api/kiosk/identity on both binaries.
// `role` is the discriminator the SPA checks at boot. Kiosk-only fields are
// present when role === 'kiosk' and absent (or zero-valued) on the controller.
export interface KioskIdentity {
  role: 'kiosk' | 'controller'
  branding: KioskBranding
  // Present on kiosks; absent on the controller.
  kiosk_code?: string
  location_code?: string
  max_qty?: number
  // Present on kiosks; true when this kiosk is opted into central control by
  // the kiosk-controller (catalog read-only, admin mutation affordances hidden).
  managed?: boolean
  // RFID gating. When rfid_enabled is true the checkout view renders the
  // "RFID scan" button (counter_scan mode only). enclosure_diff doesn't
  // expose a button — its reads are NATS-command-driven.
  rfid_enabled?: boolean
  rfid_mode?: 'counter_scan' | 'enclosure_diff'
  // Timeclock gating. timeclock_enabled drives the splash "Time clock"
  // button (kiosk) / the Timeclock report tab (both binaries); the interlock
  // flags shape copy only — the server enforces them.
  timeclock_enabled?: boolean
  timeclock_require_clock_in?: boolean
  timeclock_block_clock_out?: boolean
  // Dedicated punch station: the checkout splash is replaced by a
  // persistent punch panel and badge scans route straight to it.
  timeclock_only?: boolean
  // Public, per-user-authenticated self-service terminal (cmd/timeclock).
  // The SPA shows a worker login screen and, once authed, the self-punch
  // panel wired to /api/self/timeclock/* — no badge scan, no checkout.
  timeclock_virtual?: boolean
}

// --- Timeclock ---

// PunchStatus is GET /api/kiosk/timeclock/status — merged clocked-in state
// for one user plus the open checkouts a blocked clock-out would list.
export interface PunchStatus {
  user_id: string
  user_code: string
  user_name: string
  user_role: 'worker' | 'foreman'
  clocked_in: boolean
  since?: string
  // 'local' = this kiosk's punch ledger; 'fleet' = the controller-distributed
  // replica (clocked in/out at another kiosk); '' = never punched.
  origin: 'local' | 'fleet' | ''
  open_checkouts: OpenCheckoutDetail[] | null
  block_clock_out: boolean
  // CLOSED interval seconds for the local calendar day, scoped to this kiosk's
  // ledger. The panel adds the live (open) session on top for the displayed
  // "X today" total. Only the kiosk /api/kiosk/timeclock/status endpoint
  // populates it; absent on the self-service status payload.
  today_seconds?: number
}

// PunchResult is the reply from the punch endpoints.
export interface PunchResult {
  punch_id: string
  user_id: string
  user_code: string
  user_name: string
  direction: 'in' | 'out'
  occurred_at: string
  recorded_at: string
  source: 'self' | 'foreman' | 'admin' | 'controller_admin'
  clocked_in: boolean
  replayed?: boolean
}

// The 409 body punch endpoints return for expected conflicts. The SPA
// branches on `error`; open_checkouts hydrates the blocked-clock-out list.
export interface PunchConflict {
  error: 'not_clocked_in' | 'already_clocked_in' | 'open_checkouts'
  message: string
  count?: number
  open_checkouts?: OpenCheckoutDetail[] | null
}

export interface TimeclockCrewMember {
  user_id: string
  user_code: string
  user_name: string
  clocked_in: boolean
  since?: string
}

export interface TimeclockCrewOptions {
  group_code: string
  workers: TimeclockCrewMember[]
}

// One raw punch as returned by the history endpoints / exported to CSV.
export interface TimePunchRow {
  punch_id: string
  occurred_at: string
  recorded_at: string
  user_id: string
  user_code: string
  user_name: string
  direction: 'in' | 'out'
  source: 'self' | 'foreman' | 'admin' | 'controller_admin'
  recorded_by?: string
  reason?: string
  force?: boolean
  kiosk_code: string
  location_code?: string
}

export interface PunchInterval {
  user_id: string
  user_code: string
  user_name: string
  in: string
  out?: string
  open?: boolean
  seconds: number
}

export interface PunchDayTotal {
  user_id: string
  user_code: string
  user_name: string
  date: string
  seconds: number
  open?: boolean
}

export interface TimeclockHistoryResponse {
  punches: TimePunchRow[]
  intervals: PunchInterval[]
  day_totals: PunchDayTotal[]
  uncorrelated: unknown[]
}

export interface ClockedInRow {
  user_id: string
  user_code: string
  user_name: string
  since: string
  origin: string
}

export interface User {
  id: string
  code: string
  name: string
  role: string
  email?: string
  open_count: number
  // Populated by /api/kiosk/scan when the scan resolves a user. One row per
  // open_checkouts unit (qty=N non-serialized checkouts produce N rows).
  // Empty/omitted when open_count is 0.
  open_checkouts?: OpenCheckoutDetail[]
}

export interface OpenCheckoutDetail {
  id: string
  item_id: string
  item_code: string
  item_name: string
  item_instance_id?: string
  // The scannable code on the physical unit (what the resolver matches).
  // The foreman-return dialog uses this when adding a serialized line so
  // the server lands on the correct instance.
  item_instance_code?: string
  instance_serial?: string
  qty: number
  checked_out_at: string
}

// Payload returned by GET /api/kiosk/cart/foreman-return/options. Workers
// are filtered to members of the foreman's group who have at least one
// open checkout (empty entries are not included).
export interface ForemanReturnWorker {
  user_id: string
  user_code: string
  user_name: string
  open_checkouts: OpenCheckoutDetail[]
}

export interface ForemanReturnOptions {
  group_code: string
  workers: ForemanReturnWorker[]
}

export interface Item {
  id: string
  code: string
  name: string
  type: 'tool' | 'consumable'
  unit?: string
  tracking_mode: 'quantity' | 'serialized'
  category?: string
  active: boolean
  quantity_on_hand: number
  open_count: number
  holder?: string
}

export interface ItemInstance {
  id: string
  item_id: string
  code: string
  serial?: string
  rfid_epc?: string
  // Lifecycle state. Replaces the old `active` boolean: in_service is
  // checkout-eligible, maintenance is owned-but-parked (counts toward on-hand,
  // not available), retired absorbs the old active=false + the removed hard
  // delete. "Out" is NOT here — it's derived from open_checkouts.
  status: 'in_service' | 'maintenance' | 'retired'
  notes?: string
}

export interface InstanceMatch {
  instance: ItemInstance
  item: Item
}

export type ScanResult =
  | { type: 'user'; record: User }
  | { type: 'item'; record: Item }
  | { type: 'item_instance'; record: InstanceMatch }
  | { type: 'unknown'; value?: string }

export type CartAction = 'checkout' | 'return' | 'consume'

export interface CartLine {
  id: string
  item_id: string
  item_code: string
  item_name: string
  item_type: 'tool' | 'consumable'
  tracking_mode: 'quantity' | 'serialized'
  action: CartAction
  qty: number
  serial?: string
  item_instance_id?: string
  item_instance_code?: string
  original_checkout_user_id?: string
  original_checkout_user_name?: string
  // Per-line "this serialized unit needs maintenance on return" flag. Any
  // worker can set it on a serialized return line; commit routes the unit into
  // maintenance after closing the open checkout. Server-side field is
  // request_maintenance (omitempty).
  request_maintenance?: boolean
  warnings?: string[]
}

export interface Cart {
  id: string
  user_id: string
  user_code: string
  user_name: string
  // Denormalized snapshot from the active user record. The SPA reads this
  // to gate foreman-only affordances (e.g. the "Return on behalf of…"
  // button). Server re-reads role from the DB at commit time, so a stale
  // value here is at worst a UI hint that fails late.
  user_role: 'worker' | 'foreman'
  started_at: string
  expires_at: string
  lines: CartLine[]
}

export interface CommitResult {
  transaction_id: string
  lines_count: number
  checked_out: number
  returned: number
  consumed: number
}

// RFIDScanResult is the response from POST /api/kiosk/cart/rfid-scan.
// observed_epcs is the full deduplicated EPC array the reader saw —
// useful for an "n tags observed" hint regardless of how many landed
// in the cart. unresolved_epcs is the subset that didn't match any
// active item_instance (the SPA renders these as a passive "unknown
// tags" badge). added_lines are the per-EPC cart lines that landed
// through the same merge semantics as cart/add; counts may be fewer
// than observed_epcs - unresolved_epcs because of duplicates or
// inactive instances (those are skip-and-logged server-side).
export interface RFIDScanResult {
  cart: Cart
  added_lines: CartLine[]
  observed_epcs: string[]
  unresolved_epcs: string[]
}

// ReadTriggerResult is the response from POST /api/kiosk/cart/read-trigger
// (enclosure_diff mode). Mirrors RFIDScanResult but with an additional
// skipped_cross_user_count: a returned tag whose open_checkout belongs
// to a different worker is skipped (and counted) since the
// commit-time foreman+same-group gate would have rejected it anyway —
// surfacing the count to the operator instead of silently dropping.
export interface ReadTriggerResult {
  cart: Cart
  added_lines: CartLine[]
  observed_epcs: string[]
  unresolved_epcs: string[]
  skipped_cross_user_count: number
}

// Admin-side records mirror what we persist in PocketBase. These are looser
// than the kiosk DTOs because admin CRUD must handle partial records during
// create/edit and PB returns extra fields (created, updated, collectionId).

export interface ItemRecord {
  id: string
  code: string
  name: string
  type: 'tool' | 'consumable'
  unit: string
  tracking_mode: 'quantity' | 'serialized'
  category: string
  active: boolean
  notes: string
  quantity_on_hand: number
  reorder_threshold: number
  // Serialized-only: when true, a return routes the unit into maintenance
  // automatically (in addition to the per-line "needs maintenance" toggle any
  // worker can set). Ignored for quantity-tracked items and consumables.
  requires_maintenance_on_return?: boolean
  created?: string
  updated?: string
}

export interface WorkerRecord {
  id: string
  email: string
  phone: string
  code: string
  name: string
  role: 'worker' | 'foreman'
  // FK id pointing at a groups record. Empty string for ungrouped workers.
  group?: string
  active: boolean
  created?: string
  updated?: string
  expand?: {
    group?: GroupRecord
  }
}

export interface RecipientsSpec {
  worker_email: boolean
  all_admins: boolean
  extras: string[]
}

export interface ScheduledReportRecord {
  id: string
  report_key: 'open_checkouts' | 'daily_activity' | 'maintenance' | 'timeclock' | 'timeclock_self'
  cadence: 'daily' | 'weekly' | 'monthly'
  hour: number
  weekday: number
  day_of_month: number
  enabled: boolean
  recipients: RecipientsSpec
  subject_override: string
  // kiosk_code scopes the schedule to one kiosk in the fleet. Empty =
  // fleet-wide (controller) or "this kiosk" (standalone). Set only on
  // the controller; standalone kiosks never write a value.
  kiosk_code?: string
  last_run_at?: string
  last_status?: 'sent' | 'failed' | 'skipped' | ''
  last_error?: string
  created?: string
  updated?: string
}

export interface AdminRecord {
  id: string
  email: string
  name: string
  active: boolean
  created?: string
  updated?: string
}

export interface GroupRecord {
  id: string
  code: string
  name: string
  contact_email: string
  contact_phone: string
  notes: string
  active: boolean
  created?: string
  updated?: string
}

export interface KioskRecord {
  id: string
  kiosk_code: string
  location_code: string
  last_transaction_at?: string
  status: 'unknown' | 'active' | 'disabled'
  notes: string
  created?: string
  updated?: string
}

// HeartbeatsResponse is the controller's GET /api/controller/kiosks/heartbeats
// payload. SPA polls it to derive online/stale/offline status per kiosk.
// controller_started_at is used to suppress "offline" during the warmup
// window after a controller restart.
export interface HeartbeatsResponse {
  controller_started_at: string
  kiosks: Record<string, string>
}

// InventorySnapshotItem mirrors the kiosk-side snapshot reply. Returned by
// GET /api/controller/kiosks/:code/inventory.
export interface InventorySnapshotItem {
  item_code: string
  item_name: string
  quantity_on_hand: number
  reorder_threshold: number
  tracking_mode: 'quantity' | 'serialized'
  active: boolean
  // Derived by the controller from its projected ledger + catalog (the kiosk's
  // snapshot doesn't carry these) so the panel can show out/available exactly
  // like the local kiosk Items view. `out` is units currently checked out;
  // `type` distinguishes tools (which have "out") from consumables.
  out: number
  type: 'tool' | 'consumable'
  // Serialized-only: count of units parked in maintenance. Carried straight
  // from the kiosk's snapshot (the controller passes unknown fields through).
  // Subtracted from available alongside `out`; 0 for quantity/consumable items.
  maintenance?: number
}

export interface InventorySnapshotResponse {
  items: InventorySnapshotItem[]
}

// InventoryAdjustResponse mirrors the kiosk's reply to a controller-driven
// adjust. The controller endpoint passes the kiosk's reply through unchanged.
export interface InventoryAdjustResponse {
  adjustment_id: string
  item_id: string
  item_code: string
  delta: number
  new_quantity: number
  prev_quantity: number
}

// KioskOfflineError is the SSE-style 503 body the controller returns when
// the kiosk's heartbeat is stale or the NATS reply doesn't arrive. The SPA
// distinguishes this from generic 5xx so the inventory panel can render a
// "kiosk offline" banner instead of a red error box.
export interface KioskOfflineError {
  error: 'kiosk_offline'
  kiosk_code: string
  command_id?: string
}

// KioskMetrics is the operational + activity snapshot returned by the kiosk's
// GET /api/kiosk/metrics and, fleet-wide, by the controller's proxy at
// GET /api/controller/kiosks/:code/metrics (same metrics.Compute on the kiosk).
export interface KioskMetricsOperational {
  uptime_seconds: number
  nats_connected: boolean
  rfid_enabled: boolean
  rfid_mode?: string
  rfid_connected: boolean
  active_carts: number
}

export interface KioskMetricsLedger {
  items_out: number
  users_with_items_out: number
  low_stock_skus: number
  transactions_today: number
  transactions_week: number
}

export interface KioskMetrics {
  kiosk_code: string
  generated_at: string
  operational: KioskMetricsOperational
  ledger: KioskMetricsLedger
}

// kiosk_items: controller-side membership row tying one item to one kiosk.
// A row exists iff that kiosk stocks that item; cascade-deletes from either
// side. Per-kiosk catalog projection is driven by these rows.
export interface KioskItemRecord {
  id: string
  kiosk: string
  item: string
  created?: string
  updated?: string
}

export interface StockAdjustmentRecord {
  id: string
  item: string
  delta: number
  new_quantity: number
  reason: string
  admin: string
  // source discriminates locally-clicked adjustments from controller-driven
  // ones. controller_admin_id and command_id only populate for source='controller'.
  source?: 'local' | 'controller'
  controller_admin_id?: string
  command_id?: string
  created: string
  expand?: {
    admin?: { id: string; name: string; email: string }
  }
}

export interface StockAdjustmentResult {
  adjustment_id: string
  item_id: string
  delta: number
  new_quantity: number
  prev_quantity: number
}

// CatalogIntegrityReport mirrors the controller's
// GET /api/kiosk/catalog/integrity response. One bucket per KV store
// (items, users, groups). Keys are sorted on the server so consecutive
// runs produce stable diffs.
//
// Slice fields are marked nullable so the SPA stays robust when talking
// to a controller running pre-groups code (which returns null arrays when
// no drift exists, and omits the groups bucket entirely).
export interface CatalogIntegrityBucket {
  bucket: string
  expected_keys: number
  actual_keys: number
  missing_in_kv: string[] | null
  extra_in_kv: string[] | null
}

export interface CatalogIntegrityReport {
  items: CatalogIntegrityBucket
  users: CatalogIntegrityBucket
  groups?: CatalogIntegrityBucket
}

// CatalogReconcileBucket / Report mirror the response from
// POST /api/kiosk/catalog/reconcile. Errors are per-key strings so the UI
// can render them; absence means no errors.
export interface CatalogReconcileBucket {
  bucket: string
  published: number
  deleted: number
  publish_errors?: string[]
  delete_errors?: string[]
}

export interface CatalogReconcileReport {
  items: CatalogReconcileBucket
  users: CatalogReconcileBucket
  groups?: CatalogReconcileBucket
}

// LedgerRepublishResult mirrors POST /api/kiosk/ledger/republish.
// From/to echo back the request scope; counts report what was walked.
export interface LedgerRepublishResult {
  from?: string
  to?: string
  transactions_published: number
  lines_published: number
  skipped: number
}
