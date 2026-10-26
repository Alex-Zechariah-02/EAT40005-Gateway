import { useSyncExternalStore } from 'react'

// Light or dark. The default follows the computer's setting; a choice made in
// the app is remembered in one localStorage key. The resolved theme is set as
// a class on <html> so the CSS tokens and the dark: variant agree.

export type ThemeChoice = 'system' | 'light' | 'dark'

const storageKey = 'fetch.theme'
const media = window.matchMedia('(prefers-color-scheme: dark)')
const listeners = new Set<() => void>()
let choice: ThemeChoice = readChoice()

function readChoice(): ThemeChoice {
  try {
    const value = localStorage.getItem(storageKey)
    return value === 'light' || value === 'dark' ? value : 'system'
  } catch {
    return 'system'
  }
}

function apply() {
  const dark = choice === 'dark' || (choice === 'system' && media.matches)
  document.documentElement.classList.toggle('dark', dark)
  document.documentElement.classList.toggle('light', !dark)
  for (const listener of listeners) listener()
}

media.addEventListener('change', apply)
apply()

export function setTheme(next: ThemeChoice) {
  choice = next
  try {
    if (next === 'system') localStorage.removeItem(storageKey)
    else localStorage.setItem(storageKey, next)
  } catch {
    // Storage refused: the choice lasts for this page only.
  }
  apply()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function useTheme(): { choice: ThemeChoice; dark: boolean } {
  const current = useSyncExternalStore(subscribe, () => choice)
  return { choice: current, dark: document.documentElement.classList.contains('dark') }
}
