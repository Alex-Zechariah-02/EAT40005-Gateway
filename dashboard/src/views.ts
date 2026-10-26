import type { Admin, Permission } from './api'

export type View =
  | 'overview'
  | 'traffic'
  | 'lookup'
  | 'apis'
  | 'designer'
  | 'policy'
  | 'candidates'
  | 'provisional'
  | 'overrides'
  | 'accounts'
  | 'subjects'
  | 'history'
  | 'code'
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
