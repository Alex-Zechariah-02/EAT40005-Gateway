import { useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent } from 'react'

// Client-side routes. The gateway returns the app's page for each of these
// paths; the app picks the screen.

const listeners = new Set<() => void>()

function emit() {
  for (const listener of listeners) listener()
}

window.addEventListener('popstate', emit)

export function navigate(to: string, options: { replace?: boolean } = {}) {
  if (to === location.pathname + location.search) return
  if (options.replace) history.replaceState(null, '', to)
  else history.pushState(null, '', to)
  window.scrollTo({ top: 0 })
  emit()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

const snapshot = () => location.pathname + location.search

export function useLocation(): { path: string; params: URLSearchParams } {
  const href = useSyncExternalStore(subscribe, snapshot)
  const url = new URL(href, location.origin)
  return { path: url.pathname, params: url.searchParams }
}

// match returns the named segments when path fits pattern ("/store/:id"). A
// segment with a malformed percent escape names nothing, so it matches no route.
export function match(pattern: string, path: string): Record<string, string> | null {
  const want = pattern.split('/')
  const got = path.split('/')
  if (want.length !== got.length) return null
  const found: Record<string, string> = {}
  for (let i = 0; i < want.length; i++) {
    if (want[i].startsWith(':')) {
      if (!got[i]) return null
      try {
        found[want[i].slice(1)] = decodeURIComponent(got[i])
      } catch {
        return null
      }
    } else if (want[i] !== got[i]) {
      return null
    }
  }
  return found
}

export function Link({ to, replace = false, onClick, ...rest }: AnchorHTMLAttributes<HTMLAnchorElement> & { to: string; replace?: boolean }) {
  const follow = (event: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(event)
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
    event.preventDefault()
    navigate(to, { replace })
  }
  return <a href={to} onClick={follow} {...rest} />
}
