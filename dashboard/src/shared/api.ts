import { type Message, messages } from './messages'

// The protected Admin API's types, and the words for its codes and changes.
// Nothing here decides what an administrator may do; the server checks
// every call.

export type Permission = 'read' | 'read_history' | 'manage_subjects' | 'manage_policy'

export interface SessionInfo {
  name: string
  permissions: Permission[]
  csrf_token: string
  expires_at: string
}

// Outcome is the controller's receipt for a mutation, or its refusal.
export interface Outcome {
  transition_id?: string
  outcome?: string
  reason?: string
  field?: string
  revision?: number
  generation?: string
  candidate?: number
  provisional?: number
  rollback_target?: number
  deadline?: string
  disposition?: string
  override?: number
  target?: string
  expires_at?: string
}

export interface Status {
  state: string
  // admission says whether protected requests are admitted now: "ready", or
  // the reason they are not.
  admission: string
  audit_failed: boolean
  jwt_key_sha256: string
  revision?: number
  generation?: string
  profile?: string
  overrides?: number[]
  not_after?: string
  confirmed?: number
  provisional?: number
  deadline?: string
  // "unreadable" when the gateway could not read its stored policy for this
  // answer: confirmed, provisional and deadline are then unknown, not absent.
  stored_authority?: string
}

interface RateRule {
  algorithm: string
  count: number
  window_seconds: number
  // "subject" counts per account; "client_address" per client address, the
  // only key a public operation may use.
  key: string
}

export interface Operation {
  id: string
  api: string
  method: string
  target: string
  access: string
  auth: string
  body?: { max_bytes: number; schema: unknown }
  headers?: unknown
  rate?: RateRule
}

// Forward is an API's path rewrite: the service receives the request path
// without strip_prefix, after add_prefix. Decisions use the request path.
export interface Forward {
  strip_prefix?: string
  add_prefix?: string
}

// forwardRule describes a forward rule in words, or null when the path is
// forwarded unchanged. The gateway alone applies the rule.
export function forwardRule(forward: Forward | undefined): string | null {
  const parts = [forward?.strip_prefix ? `removes ${forward.strip_prefix}` : '', forward?.add_prefix ? `adds ${forward.add_prefix} in front` : '']
  const text = parts.filter(Boolean).join(', then ')
  return text ? text[0].toUpperCase() + text.slice(1) : null
}

// Policy is a schema version 4 document. It names no accounts: roles come
// from the trusted issuer's tokens and blocking is a separate list.
export interface Policy {
  schema_version: number
  profile: string
  upstreams: { id: string; dial: string }[]
  apis: { id: string; upstream: string; forward?: Forward }[]
  operations: Operation[]
  roles: { id: string }[]
  permits: { role: string; operation: string }[]
}

export interface Revision {
  id: number
  parent?: number
  origin: string
  checksum: string
  actor: string
  created_at: string
  document?: Policy
}

export interface Candidate {
  id: number
  parent?: number
  checksum: string
  deny_all: boolean
  status: string
  actor: string
  created_at: string
  active: boolean
  document?: Policy
}

export interface Activation {
  revision: number
  candidate: number
  rollback_target: number
  deadline: string
  activated_at: string
  actor: string
  disposition?: string
  resolved_at?: string
  resolved_by?: string
  resolution?: string
}

export interface ControlChange {
  control: string
  target: string
}

export interface ProvisionalView {
  activation: Activation
  changes: ControlChange[]
  generation?: string
}

export interface Override {
  id: number
  type: string
  control: string
  operation: string
  role?: string
  count?: number
  target: string
  base_revision: number
  created_at: string
  expires_at: string
  actor: string
  creation: string
  status: string
  resolved_at?: string
  resolved_by?: string
  resolution?: string
}

export interface OverridesView {
  revision: number
  confirmed: number
  provisional?: number
  overrides: Override[]
  generation?: string
  generation_overrides?: number[]
}

export interface HistoryEntry {
  seq: number
  transition_id: string
  kind: string
  actor: string
  target: string
  expected_revision?: number
  outcome: string
  reason?: string
  revision?: number
  candidate?: number
  recorded_at: string
  published_at?: string
  published_by?: string
  audited_at?: string
}

export interface Page<T> {
  items: T[]
  next?: string
}

// Reply is what one call produced. "unknown" means the request may or may not
// have reached the server: a mutation in that state must be checked before it
// is described as applied or not applied.
// An ok reply's date is the gateway's clock when it answered (its Date
// header), so figures over a time window can be placed without trusting the
// browser's clock.
export type Reply<T> =
  | { kind: 'ok'; status: number; body: T; etag: string | null; date?: number }
  | { kind: 'refused'; status: number; error: string; field?: string; outcome?: Outcome; etag: string | null }
  | { kind: 'unknown'; message: string }
  | { kind: 'aborted' }

// MutationRequest is a complete state-changing call. Resending the same
// object resends the same Idempotency-Key and body, which the server answers
// with its recorded receipt instead of acting twice.
export interface MutationRequest {
  method: 'POST' | 'PUT' | 'DELETE'
  path: string
  body?: unknown
  // rawBody is sent byte for byte instead of body, so the server's strict
  // decoder judges the administrator's own text.
  rawBody?: string
  ifMatch?: string
  key?: string
}

const unknownCode: Message = {
  title: 'The gateway refused this',
  action: 'This Dashboard has no description for this code; it is shown under More details for diagnosis.',
}

// message words a code, or says that this Dashboard has no description for
// it, so a code is never shown without any explanation.
export function message(code: string): Message {
  return messages[code] ?? messages[code.toUpperCase()] ?? messages[code.toLowerCase()] ?? unknownCode
}

// committedMessage words a change the gateway recorded although its answer
// reports a problem: its title always says it was applied.
export function committedMessage(code: string): Message {
  const known = message(code)
  if (known.title.startsWith('Applied, but')) return known
  return {
    title: 'Applied, but the answer reports a problem',
    action: `The gateway recorded the change, then could not finish its answer (${known.title.toLowerCase()}). Refresh to see the current state.`,
  }
}

// explain is a code's message as one sentence, for hints and lists.
export function explain(code: string): string {
  const m = message(code)
  return `${m.title}. ${m.action}`
}

// committedAnyway says whether a non-success answer still reports a
// committed change: in force but not yet recorded in the audit log, or
// stored but not activated. Such a change is never shown as refused.
export function committedAnyway(reply: { error: string; outcome?: Outcome }): boolean {
  return reply.outcome?.outcome === 'committed' || reply.error.toUpperCase() === 'OUTCOME_AUDIT_PENDING'
}

// describe names a state-changing request in plain words, for the list of
// changes whose outcome is unknown.
export function describe(request: MutationRequest): string {
  const body = (request.body ?? {}) as { revision?: number; candidate?: number }
  const [, , resource, id, action] = request.path.split('/')
  const name = id === undefined ? '' : decodeURIComponent(id)
  if (resource === 'candidates') return request.method === 'DELETE' ? `Withdraw draft change ${name}` : 'Save a draft change'
  if (resource === 'provisional') {
    if (id === 'confirm') return `Keep version ${body.revision}`
    if (id === 'rollback') return `Undo version ${body.revision}`
    return `Put draft change ${body.candidate} on trial`
  }
  if (resource === 'overrides') return request.method === 'DELETE' ? `Revoke temporary rule ${name}` : 'Add a temporary rule'
  if (resource === 'blocked-accounts') return request.method === 'DELETE' ? `Unblock ${name}` : `Block ${name}`
  if (resource === 'keys' && id === 'reload' && action === undefined) return 'Reload the signing keys'
  return `${request.method} ${request.path}`
}
