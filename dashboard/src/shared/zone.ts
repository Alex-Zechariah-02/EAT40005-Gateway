import { useSyncExternalStore } from 'react'

// The time zone every time on screen is shown in: the browser's own zone
// (Automatic, the default), UTC, or a chosen IANA zone. The choice alone is
// kept in one localStorage key; Automatic removes it. An unreadable or
// invalid stored value counts as Automatic.

export const automatic = 'auto'

const storageKey = 'gateway.timezone'
const listeners = new Set<() => void>()
let choice = readChoice()

// validZone says whether the browser knows an IANA zone by this name.
function validZone(name: string): boolean {
  if (name.length === 0 || name.length > 64) return false
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: name })
    return true
  } catch {
    return false
  }
}

function readChoice(): string {
  try {
    const value = localStorage.getItem(storageKey)
    return value !== null && validZone(value) ? value : automatic
  } catch {
    return automatic
  }
}

// browserZone is the zone Automatic stands for.
export function browserZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
}

// activeZone is the IANA zone times are shown in now.
export function activeZone(): string {
  return choice === automatic ? browserZone() : choice
}

export function setZone(next: string) {
  if (next !== automatic && !validZone(next)) return
  choice = next
  try {
    if (next === automatic) localStorage.removeItem(storageKey)
    else localStorage.setItem(storageKey, next)
  } catch {
    // Storage refused: the choice lasts for this page only.
  }
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

// useZoneChoice is the stored choice: automatic, 'UTC' or a zone name.
export function useZoneChoice(): string {
  return useSyncExternalStore(subscribe, () => choice)
}

// useZone is the active zone; a component using it redraws when it changes.
export function useZone(): string {
  useZoneChoice()
  return activeZone()
}
