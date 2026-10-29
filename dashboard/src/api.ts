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
  // "unreadable" when the gateway could not read its stored policy for this
  // answer: confirmed, provisional and deadline are then unknown, not absent.
  stored_authority?: string
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
// called and the dashboard drops everything it shows. Several calls in flight
// can all meet the end of one session; it ends once, so the notice made from
// what the dashboard knew at that moment is not replaced by a later, emptier one.
export function adminClient(csrf: string, signedOut: () => void): Admin {
  let ended = false
  async function call<T>(method: string, path: string, init: RequestInit, signal?: AbortSignal): Promise<Reply<T>> {
    const reply = await request<T>(method, path, init, signal)
    if (reply.kind === 'refused' && reply.status === 401) {
      const session = await request<SessionInfo>('GET', '/admin/session')
      if (session.kind !== 'ok' && !ended) {
        ended = true
        signedOut()
      }
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

// Message is how the Dashboard words a server code: a plain title saying
// what happened, and what to do next. The code itself is shown beside it
// under More details, exactly as sent.
export interface Message {
  title: string
  action: string
}

// messages word the server's error and refusal codes. A code is looked up as
// sent, in capitals and in lower case, because blocked-account and key
// changes send the controller's reasons in lower case while history records
// them in capitals.
const messages: Record<string, Message> = {
  bad_request: { title: 'The request was not accepted', action: 'A value is missing or not allowed. Check what you entered, then try again.' },
  gateway_unavailable: {
    title: 'The gateway could not finish this',
    action: 'For a change, this does not show whether it was applied. Check its outcome in History before sending anything else.',
  },
  candidate_invalid: {
    title: 'This draft change cannot be read as a complete policy',
    action: 'Its impact cannot be previewed. Save a new draft from the current policy.',
  },
  unauthenticated: { title: 'Not signed in', action: 'Your session has ended, or the name or password is wrong. Sign in again.' },
  forbidden: { title: 'Your account cannot do this', action: 'Your administrator account does not have the permission for this action.' },
  not_found: { title: 'Not found', action: 'It does not exist, or it was removed. Refresh to see the current list.' },
  http_404: { title: 'This gateway does not offer that', action: 'The gateway has no such address. It may run a version without this feature.' },
  stream_limit: { title: 'Too many live views are open', action: 'At most 8 can be open at once. Close one, for example in another tab, then reload.' },
  keys_invalid: {
    title: 'The signing keys could not be loaded',
    action: 'The gateway kept the keys it had. Check the key files in the operator settings, then reload the keys again.',
  },
  precondition_required: { title: 'The current version is needed', action: 'Reload the page, so the change is based on the active version, then try again.' },
  unsupported_media_type: { title: 'The request must be JSON', action: 'Send the document as JSON.' },
  payload_too_large: { title: 'The request is too large', action: 'Make the document smaller, then try again.' },
  too_many_attempts: { title: 'Too many failed sign-in attempts', action: 'Sign-in is locked for a while. Wait, then try again.' },
  unavailable: { title: 'The gateway cannot do this now', action: 'Try again shortly. If it goes on, open the Overview to see why.' },
  busy: { title: 'The gateway is busy', action: 'It is checking other passwords. Try again in a moment.' },
  session_limit: { title: 'Too many sessions are open', action: 'Sign out of a session you no longer use, then try again.' },
  no_confirmed_policy: { title: 'No policy is active yet', action: 'Put the first active policy from Changes first.' },
  no_provisional: { title: 'No change is on trial', action: 'There is nothing to keep or undo. Refresh to see the current state.' },
  STALE: { title: 'The policy changed while you were editing', action: 'Reload to see the latest version, then make your change again.' },
  PRECONDITION_FAILED: {
    title: 'This change is out of date',
    action: 'Someone updated the policy after you opened it. Reload, check your edits still apply, then try again.',
  },
  PROVISIONAL_ACTIVE: { title: 'A change is already on trial', action: 'Keep or undo the change on trial first (Changes page), then try again.' },
  INVALID_STATE: { title: 'The gateway cannot do this in its current state', action: 'Open the Overview to see what the gateway is doing now.' },
  CANDIDATE_UNAVAILABLE: {
    title: 'This draft can no longer be used',
    action: 'It was withdrawn, already activated, or set aside after a restore. Save a new draft from the current policy.',
  },
  CANDIDATE_STALE: {
    title: 'This draft is based on an older version',
    action: 'The active version changed after it was saved. Save it again from the current policy.',
  },
  CANDIDATE_INTEGRITY: { title: 'This draft change is damaged', action: 'Its stored copy no longer matches its checksum. Save a new draft.' },
  NOT_FOUND: { title: 'Not found', action: 'It does not exist, or it was removed. Refresh to see the current list.' },
  TRANSITION_ID_REUSED: {
    title: 'This request key was already used',
    action: 'It belongs to a different request. Start the action again; the Dashboard then sends a new key.',
  },
  NO_PROVISIONAL: { title: 'No change is on trial', action: 'There is nothing to keep or undo. Refresh to see the current state.' },
  PROVISIONAL_MISMATCH: {
    title: 'A different change is on trial now',
    action: 'Refresh to see which change is on trial before keeping or undoing it.',
  },
  ALREADY_RESOLVED: { title: 'This has already ended', action: 'It was kept, undone or revoked already. Refresh to see the current state.' },
  DEADLINE_PASSED: {
    title: 'The trial deadline has passed',
    action: 'The gateway undoes the change by itself. Refresh to see the active version.',
  },
  TARGET_INVALID: {
    title: 'The earlier version could not be restored',
    action: 'The gateway stopped accepting requests to stay safe. Open the Overview to see how to recover.',
  },
  INVALID_DEADLINE: { title: 'This trial length is not allowed', action: 'Choose 1 to 86,400 seconds (24 hours).' },
  INVALID_EXPIRY: { title: 'This rule length is not allowed', action: 'Choose 1 to 86,400 seconds (24 hours).' },
  OVERRIDE_CONFLICT: {
    title: 'A temporary rule already covers this',
    action: 'Revoke the other temporary rule or wait for it to end, or choose another endpoint or role.',
  },
  OVERRIDE_LIMIT: { title: 'Too many temporary rules', action: 'The most that can be active at once is reached. Revoke one first.' },
  UNSUPPORTED_CONTROL: { title: 'A temporary rule cannot change this', action: 'Make a permanent change instead: save a draft change and put it on trial.' },
  UNSUPPORTED_ACTION: { title: 'This kind of temporary rule is not available here', action: 'Choose another action for this endpoint.' },
  INVALID_VALUE: { title: 'A value is not allowed', action: 'Check the value named under More details, then try again.' },
  UNKNOWN_REFERENCE: {
    title: 'It names something the policy does not have',
    action: 'Check that every role, endpoint, API and service address it refers to exists in the policy.',
  },
  NOT_DENY_ALL: {
    title: 'The first policy must block everything',
    action: 'Start from the policy that denies everything; add permissions after it is active.',
  },
  OUTCOME_AUDIT_PENDING: {
    title: 'Applied, but the security log could not record it',
    action: 'The change is active. The gateway stops accepting requests until it restarts and records it.',
  },
  UNKNOWN_FIELD: { title: 'The document has a field the gateway does not know', action: 'Remove or correct the field named under More details.' },
  DUPLICATE_MEMBER: { title: 'The document repeats a name', action: 'Each name may appear once in an object. Remove the repeat.' },
  DUPLICATE_ID: { title: 'An ID is used twice', action: 'Give each API, endpoint, role and service address its own ID.' },
  SCHEMA_INVALID: {
    title: 'The document is not shaped like a policy',
    action: 'Correct the field named under More details, or start again from the current policy.',
  },
  SCHEMA_VERSION_UNSUPPORTED: { title: 'This policy format version is not supported', action: 'Use schema version 4.' },
  UNSUPPORTED_FEATURE: { title: 'The document uses something the gateway does not support', action: 'Remove the feature named under More details.' },
  LIMIT_EXCEEDED: { title: 'The document is too large', action: 'It exceeds a size or count limit. Make it smaller.' },
  INVALID_UPSTREAM: {
    title: 'A service address is not allowed',
    action: 'Use a local address with a port, such as 127.0.0.1:9502, and give each service its own address.',
  },
  INVALID_METHOD: { title: 'An endpoint method is not allowed', action: 'Use GET, HEAD, POST, PUT, PATCH, DELETE or OPTIONS.' },
  INVALID_TARGET: { title: 'An endpoint path is not valid', action: 'Use an exact path, such as /orders, or a prefix ending in /*.' },
  DUPLICATE_OPERATION: { title: 'Two endpoints have the same method and path', action: 'Remove one, or change its method or path.' },
  UNAVAILABLE: { title: 'The gateway is unavailable', action: 'It changed nothing. Try again shortly.' },
  AUDIT_UNAVAILABLE: {
    title: 'The security log cannot be written',
    action: 'Nothing was changed. The gateway accepts no change until its security log works again.',
  },
  STORE_ERROR: {
    title: 'The gateway could not use its database',
    action: 'It did not complete this request. If you sent the identical request before, check in History whether that one was applied.',
  },
  PUBLICATION_FAILED: {
    title: 'Applied, but not activated',
    action: 'The change was stored but could not be activated, so the gateway stopped accepting requests. Restart the gateway.',
  },
  CREDENTIAL_CHANGED: { title: 'Your password changed during this request', action: 'Sign in again.' },
  ENROLMENT_CONSUMED: { title: 'This enrolment secret was already used', action: 'Ask the operator for a new one.' },
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
