import { useSyncExternalStore } from 'react'

// Light or dark. The default follows the computer's setting; a choice made
// here is remembered in one localStorage key, alongside the Designer's
// box-position key. The resolved theme is set as a class on <html> so the
// stylesheet's tokens follow it.

export type ThemeChoice = 'system' | 'light' | 'dark'

const storageKey = 'gateway.theme'
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

export function useTheme(): ThemeChoice {
  return useSyncExternalStore(
    (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    () => choice,
  )
}
