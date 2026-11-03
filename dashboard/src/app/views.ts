import type { Admin } from '../shared/http'
import type { Permission } from '../shared/api'

// The Dashboard's pages, the props each page gets, and links from one page to another.

export type View =
  | 'overview'
  | 'live-traffic'
  | 'security-log'
  | 'access-checker'
  | 'limits'
  | 'advice'
  | 'apis'
  | 'policy'
  | 'versions'
  | 'changes'
  | 'temporary-rules'
  | 'accounts'
  | 'history'
  | 'team'
  | 'my-account'

export interface Handoff {
  draft?: string
  candidate?: number
}

// ViewProps is what every signed-in view receives. can() only decides which
// actions to offer; the server decides whether they are allowed.
export interface ViewProps {
  admin: Admin
  can: (permission: Permission) => boolean
  open: (view: View, handoff?: Handoff) => void
}

// to is the address of a view opened on one item, such as
// #/policy?select=operation%3Aorders.create, so a link elsewhere can show
// exactly what it names and the back button returns to where it was.
export function to(view: View, focus?: Record<string, string | number>): string {
  const query = new URLSearchParams()
  for (const [name, value] of Object.entries(focus ?? {})) query.set(name, String(value))
  const text = query.toString()
  return text ? `#/${view}?${text}` : `#/${view}`
}

// refAddress is a page's address, the current one by default, with one
// request's drawer open over it (#/live-traffic?ref=8e72...) or, for null,
// with the drawer closed. Back closes a drawer opened from its page.
export function refAddress(ref: string | null, hash = window.location.hash): string {
  const [path, query = ''] = hash.split('?')
  const params = new URLSearchParams(query)
  if (ref) params.set('ref', ref)
  else params.delete('ref')
  const text = params.toString()
  const page = path || '#/overview'
  return text ? `${page}?${text}` : page
}

// focusParam reads one item a view was opened on from the address.
export function focusParam(name: string): string | null {
  return new URLSearchParams(window.location.hash.split('?')[1] ?? '').get(name)
}

// connectHandoff is the page memory's key another page sets to open the
// Policy page with Connect an API open; the Policy page clears it on use.
export const connectHandoff = 'policy.connect'
