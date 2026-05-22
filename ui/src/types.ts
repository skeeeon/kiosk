export interface KioskBranding {
  logo_url?: string
  tagline?: string
  primary_color?: string
}

export interface KioskIdentity {
  kiosk_code: string
  location_code: string
  branding: KioskBranding
  max_qty: number
  // True when this kiosk is opted into central control by the
  // kiosk-controller: items + users are pushed down from the controller via
  // JetStream KV, and the admin SPA hides catalog mutation affordances.
  managed: boolean
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
  serial?: string
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
  serial: string
  category: string
  rfid_epc: string
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
  active: boolean
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
