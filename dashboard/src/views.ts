import type { Admin, Permission } from './api'

export type View =
  | 'overview'
  | 'traffic'
  | 'lookup'
  | 'security-log'
  | 'access'
  | 'limits'
  | 'advice'
  | 'apis'
  | 'designer'
  | 'policy'
  | 'candidates'
  | 'overrides'
  | 'accounts'
  | 'history'
  | 'team'
  | 'account'

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
// #/designer?select=operation%3Aorders.create, so a link elsewhere can show
// exactly what it names and the back button returns to where it was.
export function to(view: View, focus?: Record<string, string | number>): string {
  const query = new URLSearchParams()
  for (const [name, value] of Object.entries(focus ?? {})) query.set(name, String(value))
  const text = query.toString()
  return text ? `#/${view}?${text}` : `#/${view}`
}

// focusParam reads one item a view was opened on from the address.
export function focusParam(name: string): string | null {
  return new URLSearchParams(window.location.hash.split('?')[1] ?? '').get(name)
}
