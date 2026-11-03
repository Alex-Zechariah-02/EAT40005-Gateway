import { useContext, useEffect, useState, type ReactNode } from 'react'
import { ViewMemory } from './hooks'
import { focusParam, to, type View } from './views'

// Cross-reference links between pages and the focus they carry.

// XRef links an ID to the page where it lives, opened on that item, so an
// administrator can follow it in one click, as in a cloud console.
export function XRef(props: { view: View; focus?: Record<string, string | number>; label?: string; children: ReactNode }) {
  return (
    <a className="xref" href={to(props.view, props.focus)} aria-label={props.label}>
      {props.children}
    </a>
  )
}

// useFocus is the item the current view was opened on, kept current as links
// within the same view change it.
export function useFocus(name: string): string | null {
  const [value, setValue] = useState(() => focusParam(name))
  useEffect(() => {
    const read = () => setValue(focusParam(name))
    window.addEventListener('hashchange', read)
    return () => window.removeEventListener('hashchange', read)
  }, [name])
  return value
}

// useTarget scrolls the element marked as the focused item into view once it
// is drawn, and lets its highlight fade, so the eye lands on what the link named.
export function useTarget(ready: boolean, key: string | null) {
  useEffect(() => {
    if (!ready || !key) return
    const timer = window.setTimeout(() => {
      const element = document.querySelector('[data-target="true"]')
      element?.scrollIntoView({ block: 'center', behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })
    }, 60)
    return () => window.clearTimeout(timer)
  }, [ready, key])
}

// AccountLink opens Request Lookup on one account's requests. The account is
// handed over through the page's memory, never the address, so like any
// account searched for it stays out of the browser's history.
export function AccountLink({ account, children }: { account: string; children: ReactNode }) {
  const memory = useContext(ViewMemory)
  return (
    <a
      className="xref"
      href="#/request-lookup"
      onClick={() =>
        memory?.set('lookup', {
          form: { ref: '', subject: account, operation: '', decision: '' },
          query: `subject=${encodeURIComponent(account)}`,
        })
      }
    >
      {children}
    </a>
  )
}
