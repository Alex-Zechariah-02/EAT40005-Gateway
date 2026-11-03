import { useContext, useEffect, useState, type ReactNode } from 'react'
import { Can, ViewMemory } from './hooks'
import { focusParam, to, type View } from '../app/views'

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

// The Security Log search a page hands over through the page's memory.
export const logSearch = 'security-log-search'
export type LogFilter = Partial<Record<'subject' | 'operation' | 'decision', string>>

// Each handed-over search gets a new address, so a repeated search runs
// again; the counter carries no search term.
let searches = 0

// searchLog opens the Security Log searching for an account, endpoint or
// decision. The filter goes through the page's memory, never the address, so
// an account searched for stays out of the browser's history.
export function searchLog(memory: Map<string, unknown> | null, filter: LogFilter) {
  memory?.set(logSearch, filter)
  searches += 1
  window.location.hash = `#/security-log?search=${searches}`
}

// AccountLink opens the Security Log on one account's requests, for an
// administrator who may read it; for anyone else the account is plain text.
export function AccountLink({ account, children }: { account: string; children: ReactNode }) {
  const memory = useContext(ViewMemory)
  const can = useContext(Can)
  if (!can('read_history')) return <>{children}</>
  return (
    <a
      className="xref"
      href="#/security-log"
      onClick={(event) => {
        event.preventDefault()
        searchLog(memory, { subject: account })
      }}
    >
      {children}
    </a>
  )
}
