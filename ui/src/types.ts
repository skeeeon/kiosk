export interface KioskBranding {
  logo_url?: string
  tagline?: string
  primary_color?: string
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
}

export interface User {
  id: string
  code: string
  name: string
  role: string
  email?: string
  open_count: number
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
  active: boolean
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
  warnings?: string[]
}

export interface Cart {
  id: string
  user_id: string
  user_code: string
  user_name: string
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
  created?: string
  updated?: string
}

export interface WorkerRecord {
  id: string
  email: string
  code: string
  name: string
  role: 'worker' | 'foreman'
  group?: string
  active: boolean
  created?: string
  updated?: string
}

export interface KioskRecord {
  id: string
  kiosk_code: string
  location_code: string
  last_seen?: string
  status: 'unknown' | 'active' | 'disabled'
  notes: string
  created?: string
  updated?: string
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
// (items, users). Keys are sorted on the server so consecutive runs
// produce stable diffs.
export interface CatalogIntegrityBucket {
  bucket: string
  expected_keys: number
  actual_keys: number
  missing_in_kv: string[]
  extra_in_kv: string[]
}

export interface CatalogIntegrityReport {
  items: CatalogIntegrityBucket
  users: CatalogIntegrityBucket
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
