// Development only: the stand-in's route table and the helpers its service
// handlers share.
import type { Role, User } from '../lib/types'
import * as seed from './seed'

export type Json = Record<string, unknown>
export interface Ctx { method: string; path: string; query: URLSearchParams; body: Json | null; user: User | null; headers: Headers }
export type Handler = (ctx: Ctx, match: RegExpMatchArray) => [number, unknown]

export const all: Role[] = ['CUSTOMER', 'MERCHANT', 'RIDER', 'SUPPORT', 'OPERATIONS']
export const routes: [string, RegExp, string, Role[] | null, string, Handler][] = []
export const on = (method: string, pattern: RegExp, op: string, roles: Role[] | null, service: string, handler: Handler) => routes.push([method, pattern, op, roles, service, handler])

export const ref = () => Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('')
export const err = (status: number, error: string, message: string, field?: string): [number, unknown] => [status, { error, message, ...(field ? { field } : {}) }]
export const now = () => new Date().toISOString()

function stockState(id: string) {
  const row = seed.stock.get(id)!
  if (row.stock === null) return 'made_to_order'
  const free = row.stock - row.reserved
  return free <= 0 ? 'out' : free <= 15 ? 'low' : 'in_stock'
}
export const itemView = (id: string) => ({ ...seed.items.find((i) => i.id === id)!, stock_state: stockState(id) })
export const merchantOf = (id: string) => seed.merchants.find((m) => m.id === id)

export function freeDelivery(customer: string, merchantId: string) {
  const merchant = merchantOf(merchantId)
  if (!merchant || merchant.section !== 'mart') return false
  return !seed.orders.some((o) => o.customer === customer && merchantOf(o.merchant)?.section === 'mart' && o.status !== 'cancelled')
}

export const findOrder = (id: string) => seed.orders.find((o) => o.id === id)
