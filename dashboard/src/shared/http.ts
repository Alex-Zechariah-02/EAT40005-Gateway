import type { MutationRequest, Outcome, Reply, SessionInfo } from './api'

// Calls to the protected Admin API. The dashboard is served by the admin
// listener itself, so every call is same-origin: the browser sends the
// HttpOnly session cookie and the Origin header, and this module adds the
// CSRF token to state-changing calls.

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
