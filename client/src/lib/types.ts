// Shapes of the answers of the Inventory, Orders, Reports, Support and Fetch
// Accounts APIs.

export type Section = 'food' | 'mart'
export type Role = 'CUSTOMER' | 'MERCHANT' | 'RIDER' | 'SUPPORT' | 'OPERATIONS'
export type StockState = 'in_stock' | 'low' | 'out' | 'made_to_order'
export type OrderStatus = 'placed' | 'accepted' | 'ready' | 'picked_up' | 'delivered' | 'cancelled'
export type Payment = 'cash' | 'wallet'

export interface Category { id: string; name: string; section: Section }

export interface Merchant {
  id: string
  name: string
  section: Section
  category: string
  area: string
  delivery_minutes: { min: number; max: number }
  delivery_fee_sen: number
  open: boolean
}

export interface Item {
  id: string
  merchant: string
  name: string
  description: string
  group: string
  price_sen: number
  available: boolean
  stock_state: StockState
}

export interface List<T> { items: T[]; next?: string }
export interface MerchantPage { merchant: Merchant; menu: { group: string; items: Item[] }[] }
export interface ItemPage { item: Item; merchant: Merchant }
export interface SearchHit { merchant: Merchant; item: Item }
export interface Stock { item: string; stock: number; reserved: number }

export interface Address { line1: string; line2: string; postcode: string; city: string; state: string }

export interface Order {
  id: string
  customer: string
  merchant: string
  merchant_name: string
  items: { item: string; name: string; quantity: number; unit_price_sen: number }[]
  subtotal_sen: number
  delivery_fee_sen: number
  total_sen: number
  payment: Payment
  address: Address
  note: string
  status: OrderStatus
  rider: string | null
  timeline: { status: OrderStatus; at: string }[]
  created_at: string
}

export interface Quote { merchant: string; delivery_fee_sen: number; free_delivery: boolean; reason: string | null }
export interface Jobs { available: Order[]; mine: Order[] }

export interface Ticket {
  id: string
  customer: string
  customer_name: string
  order: string | null
  request_ref: string | null
  subject: string
  status: 'open' | 'answered' | 'resolved'
  messages: { from: 'customer' | 'agent'; author: string; author_name: string; body: string; at: string }[]
  created_at: string
}

export interface User {
  id: string
  name: string
  email: string
  roles: Role[]
  merchant: string | null
  home: '/' | '/merchant' | '/rider' | '/support' | '/ops'
}

export interface SignedIn { token: string; expires_at: string; user: User }

export interface MerchantReport {
  merchant: string
  from: string
  to: string
  orders: number
  revenue_sen: number
  by_day: { date: string; orders: number; revenue_sen: number }[]
}
export interface Summary { from: string; to: string; orders: number; revenue_sen: number; customers: number; merchants: number; average_order_sen: number }
export interface Sales { months: { month: string; section: Section; orders: number; revenue_sen: number }[] }
export interface TopItems { items: { item: string; name: string; merchant: string; quantity: number; revenue_sen: number }[] }

// The states the gateway's order schema accepts for an address.
export const states = [
  'Johor', 'Kedah', 'Kelantan', 'Melaka', 'Negeri Sembilan', 'Pahang', 'Perak', 'Perlis', 'Pulau Pinang',
  'Sabah', 'Sarawak', 'Selangor', 'Terengganu', 'Wilayah Persekutuan Kuala Lumpur',
  'Wilayah Persekutuan Labuan', 'Wilayah Persekutuan Putrajaya',
] as const
