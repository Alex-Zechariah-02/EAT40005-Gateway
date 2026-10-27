// Typed access to the protected Admin API. The dashboard is served by the
// admin listener itself, so every call is same-origin: the browser sends the
// HttpOnly session cookie and the Origin header, and this module adds the
// CSRF token to state-changing calls. Nothing here decides what an
// administrator may do; the server checks every call.

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
}

export interface RateRule {
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

// Policy is a schema version 4 document. It names no accounts: roles come
// from the trusted issuer's tokens and blocking is a separate list.
export interface Policy {
  schema_version: number
  profile: string
  upstreams: { id: string; dial: string }[]
  apis: { id: string; upstream: string }[]
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

const timeoutMs = 20_000

export interface Admin {
  get<T>(path: string, signal?: AbortSignal): Promise<Reply<T>>
  send<T>(request: MutationRequest): Promise<Reply<T>>
}

// newKey returns a fresh Idempotency-Key (16 to 64 of A-Z a-z 0-9 _ -).
export function newKey(): string {
  return crypto.randomUUID()
}

// adminClient makes calls with one session's CSRF token. A 401 usually means
// the session has ended, but a password change also answers 401 for a wrong
// current password while the session goes on. So after a 401 the gateway is
// asked whether the session is still live; unless it says so, signedOut is
// called and the dashboard drops everything it shows.
export function adminClient(csrf: string, signedOut: () => void): Admin {
  async function call<T>(method: string, path: string, init: RequestInit, signal?: AbortSignal): Promise<Reply<T>> {
    const reply = await request<T>(method, path, init, signal)
    if (reply.kind === 'refused' && reply.status === 401) {
      const session = await request<SessionInfo>('GET', '/admin/session')
      if (session.kind !== 'ok') signedOut()
    }
    return reply
  }

  return {
    get: (path, signal) => call('GET', path, {}, signal),
    send: (r) => {
      const headers: Record<string, string> = { 'X-CSRF-Token': csrf }
      if (r.ifMatch !== undefined) headers['If-Match'] = r.ifMatch
      if (r.key !== undefined) headers['Idempotency-Key'] = r.key
      const init: RequestInit = { headers }
      if (r.rawBody !== undefined || r.body !== undefined) {
        headers['Content-Type'] = 'application/json'
        init.body = r.rawBody ?? JSON.stringify(r.body)
      }
      return call(r.method, r.path, init)
    },
  }
}

// request performs one call without a session token: sign-in, enrolment and
// session checks use it directly.
export async function request<T>(method: string, path: string, init: RequestInit = {}, signal?: AbortSignal): Promise<Reply<T>> {
  const limit = AbortSignal.timeout(timeoutMs)
  let response: Response
  try {
    response = await fetch(path, {
      ...init,
      method,
      credentials: 'same-origin',
      cache: 'no-store',
      redirect: 'error',
      signal: signal ? AbortSignal.any([signal, limit]) : limit,
    })
  } catch {
    if (signal?.aborted) return { kind: 'aborted' }
    const reason = limit.aborted ? 'no answer within 20 seconds' : 'the server could not be reached'
    return { kind: 'unknown', message: reason }
  }

  const etag = response.headers.get('ETag')
  let body: unknown = undefined
  if (response.status !== 204) {
    try {
      body = await response.json()
    } catch {
      if (signal?.aborted) return { kind: 'aborted' }
      if (!response.ok) {
        return { kind: 'refused', status: response.status, error: `http_${response.status}`, etag }
      }
      // A cut-off success answer says nothing reliable about the outcome.
      return { kind: 'unknown', message: `unreadable answer (HTTP ${response.status})` }
    }
  }
  if (response.ok) {
    const date = Date.parse(response.headers.get('Date') ?? '')
    return { kind: 'ok', status: response.status, body: body as T, etag, date: Number.isNaN(date) ? undefined : date }
  }

  const fields = (body ?? {}) as { error?: string; field?: string; reason?: string; outcome?: string }
  const outcome = fields.outcome !== undefined ? (body as Outcome) : undefined
  return {
    kind: 'refused',
    status: response.status,
    error: fields.error ?? fields.reason ?? `http_${response.status}`,
    field: fields.field,
    outcome,
    etag,
  }
}

// explanations describe the server's error and refusal codes. Unknown codes
// are shown as they are.
const explanations: Record<string, string> = {
  bad_request: 'The request was malformed or a value is not allowed.',
  unauthenticated: 'You are not signed in, or the credentials are wrong.',
  forbidden: 'Your administrator account does not have the permission for this action.',
  not_found: 'It does not exist.',
  http_404: 'The gateway has no such address. It may run a version without this feature.',
  stream_limit: 'Too many live views are open (at most 8). Close one and reload.',
  keys_invalid: 'The keys in the operator settings could not be loaded; the gateway kept the keys it had.',
  precondition_required: 'The request needs the current policy revision.',
  unsupported_media_type: 'The request body must be JSON.',
  payload_too_large: 'The request body is too large.',
  too_many_attempts: 'Too many failed attempts: sign-in is locked for a while.',
  unavailable: 'The gateway cannot serve this now.',
  busy: 'The server is busy checking other passwords. Try again.',
  session_limit: 'Too many sessions are open.',
  no_confirmed_policy: 'No confirmed policy is in force.',
  no_provisional: 'No provisional policy is waiting for confirmation.',
  STALE: 'Someone changed the policy since you loaded it. Refresh and try again.',
  PRECONDITION_FAILED: 'The policy or checksum you based this on is out of date. Refresh and try again.',
  PROVISIONAL_ACTIVE: 'A provisional policy is waiting for confirmation, so this is not allowed now.',
  INVALID_STATE: 'The gateway is not in a state that allows this.',
  CANDIDATE_UNAVAILABLE: 'That candidate cannot be used (withdrawn, established or quarantined).',
  CANDIDATE_STALE: 'The candidate was prepared against an older confirmed policy. Stage it again.',
  CANDIDATE_INTEGRITY: 'The stored candidate no longer matches its checksum.',
  NOT_FOUND: 'It does not exist.',
  TRANSITION_ID_REUSED: 'This request key was already used for a different request.',
  NO_PROVISIONAL: 'No provisional policy is waiting.',
  PROVISIONAL_MISMATCH: 'A different provisional policy is waiting. Refresh.',
  ALREADY_RESOLVED: 'This was already ended.',
  DEADLINE_PASSED: 'The confirmation deadline has passed; the gateway rolls the policy back.',
  TARGET_INVALID: 'The gateway could not restore the earlier policy and stopped admitting requests.',
  INVALID_DEADLINE: 'The confirmation period must be 1 to 86400 seconds.',
  INVALID_EXPIRY: 'The override lifetime must be 1 to 86400 seconds.',
  OVERRIDE_CONFLICT: 'An active override already changes this, or this change touches an overridden rule.',
  OVERRIDE_LIMIT: 'The maximum number of active overrides is reached.',
  UNSUPPORTED_CONTROL: 'Overrides cannot change this control.',
  UNSUPPORTED_ACTION: 'This override type is not supported for this control.',
  INVALID_VALUE: 'A value is not allowed here.',
  UNKNOWN_REFERENCE: 'It names something the policy does not define.',
  NOT_DENY_ALL: 'The first policy must deny everything.',
  OUTCOME_AUDIT_PENDING: 'The change was committed but its audit record is pending; admission is stopped until the gateway restarts.',
  outcome_audit_pending: 'The change was committed but its audit record is pending; admission is stopped until the gateway restarts.',
  UNKNOWN_FIELD: 'The document has a field the gateway does not know.',
  DUPLICATE_MEMBER: 'The document repeats a member name.',
  DUPLICATE_ID: 'An identifier is used twice.',
  SCHEMA_INVALID: 'The document does not have the required shape.',
  SCHEMA_VERSION_UNSUPPORTED: 'The document has an unsupported schema version.',
  UNSUPPORTED_FEATURE: 'The document uses a feature the gateway does not support.',
  LIMIT_EXCEEDED: 'The document exceeds a size or count limit.',
  INVALID_UPSTREAM: 'An upstream must be a literal loopback address with a port, used by only one upstream.',
  INVALID_METHOD: 'The method is not allowed for an operation.',
  INVALID_TARGET: 'The operation path is not a valid exact path or prefix.',
  DUPLICATE_OPERATION: 'Two operations have the same method and path.',
  UNAVAILABLE: 'The gateway is unavailable and changed nothing.',
  AUDIT_UNAVAILABLE: 'The security event log cannot be written, so nothing was changed.',
  STORE_ERROR: 'The gateway could not read or write its database; nothing was changed.',
  PUBLICATION_FAILED: 'The change was stored but could not be put in force; the gateway stopped admitting requests.',
  CREDENTIAL_CHANGED: 'Your credentials changed during the request. Sign in again.',
  ENROLMENT_CONSUMED: 'This enrolment secret was already used.',
}

export function explain(code: string): string {
  return explanations[code] ?? ''
}
