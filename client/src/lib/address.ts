import { useSyncExternalStore } from 'react'
import { onIdentityChange } from './session'
import { states, type Address } from './types'

// The delivery address for this tab, chosen in the header or at checkout and
// held in this tab's sessionStorage. It belongs to one person: a change of
// person clears it.

const storageKey = 'fetch.address'
const listeners = new Set<() => void>()
let address: Address | null = read()

function read(): Address | null {
  try {
    const raw = sessionStorage.getItem(storageKey)
    const value = raw ? (JSON.parse(raw) as Address) : null
    if (!value || !(['line1', 'line2', 'postcode', 'city', 'state'] as const).every((field) => typeof value[field] === 'string')) return null
    if (Object.keys(addressErrors(value)).length || !(states as readonly string[]).includes(value.state)) return null
    return value
  } catch {
    return null
  }
}

export function setAddress(next: Address | null) {
  address = next
  try {
    if (next) sessionStorage.setItem(storageKey, JSON.stringify(next))
    else sessionStorage.removeItem(storageKey)
  } catch {
    // Storage refused: the address lasts for this page only.
  }
  for (const listener of listeners) listener()
}

onIdentityChange(() => setAddress(null))

export function useAddress(): Address | null {
  return useSyncExternalStore(
    (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    () => address,
  )
}

// Address checks matching the gateway's order schema, so a person sees the
// problem before sending. The gateway still validates.
export function addressErrors(a: Address): Partial<Record<keyof Address, string>> {
  const errors: Partial<Record<keyof Address, string>> = {}
  if (!a.line1.trim()) errors.line1 = 'Enter the street address.'
  else if (a.line1.length > 100) errors.line1 = 'Use 100 characters or fewer.'
  if (a.line2.length > 100) errors.line2 = 'Use 100 characters or fewer.'
  if (!/^\d{5}$/.test(a.postcode)) errors.postcode = 'Enter the 5-digit postcode.'
  if (!a.city.trim()) errors.city = 'Enter the town or city.'
  else if (a.city.length > 40) errors.city = 'Use 40 characters or fewer.'
  if (!a.state) errors.state = 'Choose the state.'
  return errors
}
