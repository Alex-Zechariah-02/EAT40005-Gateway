import { useSyncExternalStore } from 'react'
import { onIdentityChange } from './session'
import type { Item, Merchant } from './types'

// The basket for this tab: one shop at a time, as on GrabFood and
// foodpanda. It is a convenience held in this tab's sessionStorage; prices
// and stock are checked again by the Orders service when the order is placed.

export interface Line {
  item: string
  name: string
  price_sen: number
  quantity: number
}

export interface Basket {
  merchant: { id: string; name: string }
  lines: Line[]
}

const storageKey = 'fetch.basket'
export const maxQuantity = 50
export const maxLines = 20

let basket: Basket | null = read()
const listeners = new Set<() => void>()

function read(): Basket | null {
  try {
    const raw = sessionStorage.getItem(storageKey)
    if (!raw) return null
    const value = JSON.parse(raw) as Basket | null
    if (
      !value?.merchant ||
      typeof value.merchant.id !== 'string' || !value.merchant.id ||
      typeof value.merchant.name !== 'string' ||
      !Array.isArray(value.lines) || !value.lines.length || value.lines.length > maxLines ||
      !value.lines.every((line) => line &&
        typeof line.item === 'string' && line.item.length > 0 && typeof line.name === 'string' &&
        Number.isSafeInteger(line.price_sen) && line.price_sen >= 0 &&
        Number.isInteger(line.quantity) && line.quantity >= 1 && line.quantity <= maxQuantity) ||
      new Set(value.lines.map((line) => line.item)).size !== value.lines.length
    ) return null
    return value
  } catch {
    return null
  }
}

function save(next: Basket | null) {
  basket = next && next.lines.length ? next : null
  try {
    if (basket) sessionStorage.setItem(storageKey, JSON.stringify(basket))
    else sessionStorage.removeItem(storageKey)
  } catch {
    // Storage refused: the basket lasts for this page only.
  }
  for (const listener of listeners) listener()
}

// A basket belongs to one person: a change of person empties it.
onIdentityChange(() => save(null))

export type AddResult = 'added' | 'other-shop' | 'full'

export function addToBasket(merchant: Pick<Merchant, 'id' | 'name'>, item: Item, quantity = 1, replace = false): AddResult {
  if (basket && basket.merchant.id !== merchant.id && !replace) return 'other-shop'
  const start: Basket = basket && basket.merchant.id === merchant.id ? basket : { merchant: { id: merchant.id, name: merchant.name }, lines: [] }
  const existing = start.lines.find((line) => line.item === item.id)
  if (!existing && start.lines.length >= maxLines) return 'full'
  const lines = existing
    ? start.lines.map((line) => (line.item === item.id ? { ...line, quantity: Math.min(maxQuantity, line.quantity + quantity) } : line))
    : [...start.lines, { item: item.id, name: item.name, price_sen: item.price_sen, quantity: Math.min(maxQuantity, quantity) }]
  save({ ...start, lines })
  return 'added'
}

export function setQuantity(itemId: string, quantity: number) {
  if (!basket) return
  const lines = basket.lines
    .map((line) => (line.item === itemId ? { ...line, quantity: Math.min(maxQuantity, quantity) } : line))
    .filter((line) => line.quantity > 0)
  save({ ...basket, lines })
}

export function emptyBasket() {
  save(null)
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function useBasket(): Basket | null {
  return useSyncExternalStore(subscribe, () => basket)
}

export function basketCount(b: Basket | null): number {
  return b ? b.lines.reduce((sum, line) => sum + line.quantity, 0) : 0
}

export function basketSubtotal(b: Basket | null): number {
  return b ? b.lines.reduce((sum, line) => sum + line.quantity * line.price_sen, 0) : 0
}
