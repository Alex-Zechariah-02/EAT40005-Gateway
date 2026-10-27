// Every request goes to the app's own origin, so the gateway checks each one
// before any service sees it. The app never calls a service or the Admin API
// directly, never decides what a caller may do, and never resends a write.

export type Answer<T> =
  | { kind: 'ok'; status: number; data: T; ref: string | null; service: string | null }
  // A definite refusal by the gateway (no X-Service header) or a service.
  | { kind: 'refused'; status: number; code: string; message: string | null; field: string | null; ref: string | null; service: string | null }
  // A 5xx without a definite write outcome from the gateway or service.
  | { kind: 'failed'; status: number; code: string; message: string | null; ref: string | null; service: string | null }
  // Nothing came back: the request may or may not have reached its service.
  | { kind: 'no-answer'; reason: 'timeout' | 'network' }
  // This page stopped waiting (a newer request or a change of person replaced it).
  | { kind: 'cancelled' }

export interface Request {
  method: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'
  path: string
  body?: unknown
  token?: string | null
  idempotencyKey?: string
  signal?: AbortSignal
  // Only the security lab uses these, to send deliberately wrong requests:
  // extra headers (such as forged X-Gateway-* identity headers) and a body
  // sent as written with its own content type.
  headers?: Record<string, string>
  raw?: { body: string; contentType: string }
}

const timeoutMs = 15_000

export async function send<T>(request: Request): Promise<Answer<T>> {
  const headers: Record<string, string> = { Accept: 'application/json', ...request.headers }
  if (request.token) headers.Authorization = `Bearer ${request.token}`
  if (request.body !== undefined) headers['Content-Type'] = 'application/json'
  if (request.raw) headers['Content-Type'] = request.raw.contentType
  if (request.idempotencyKey) headers['Idempotency-Key'] = request.idempotencyKey
  const limit = AbortSignal.timeout(timeoutMs)
  const signal = request.signal ? AbortSignal.any([request.signal, limit]) : limit

  let response: Response
  let text: string
  try {
    response = await fetch(request.path, {
      method: request.method,
      headers,
      body: request.raw ? request.raw.body : request.body === undefined ? undefined : JSON.stringify(request.body),
      // An API caller is identified only by its token, never by cookies.
      credentials: 'omit',
      cache: 'no-store',
      redirect: 'error',
      signal,
    })
    text = await response.text()
  } catch {
    if (request.signal?.aborted) return { kind: 'cancelled' }
    return { kind: 'no-answer', reason: limit.aborted ? 'timeout' : 'network' }
  }

  const service = response.headers.get('X-Service')
  let parsed: unknown = null
  try {
    parsed = text ? JSON.parse(text) : null
  } catch {
    parsed = null
  }
  const fields = (parsed ?? {}) as { error?: unknown; message?: unknown; field?: unknown; request_id?: unknown }
  const ref = response.headers.get('X-Request-Id') ?? (typeof fields.request_id === 'string' ? fields.request_id : null)

  if (response.ok) return { kind: 'ok', status: response.status, data: parsed as T, ref, service }
  const code = typeof fields.error === 'string' ? fields.error : `http_${response.status}`
  const message = typeof fields.message === 'string' ? fields.message : null
  // Only checkout's Inventory failure guarantees no order or charge. A cancel
  // can return the same code after cancelling and refunding the order while
  // its stock release is still pending.
  const checkoutRefused = response.status === 503 && service === 'orders' && code === 'inventory_unavailable'
    && request.method === 'POST' && request.path === '/orders'
  if (response.status < 500 || checkoutRefused) {
    const field = typeof fields.field === 'string' ? fields.field : null
    return { kind: 'refused', status: response.status, code, message, field, ref, service }
  }
  return { kind: 'failed', status: response.status, code, message, ref, service }
}

// outcomeOfWrite says what the person may conclude about a change they asked
// for. Only a success or a refusal is definite. A missing answer, or a
// gateway or service error after the request may have been forwarded, leaves
// the change unknown: it may or may not have been applied.
export function outcomeOfWrite(answer: Answer<unknown>): 'done' | 'refused' | 'unknown' {
  if (answer.kind === 'ok') return 'done'
  if (answer.kind === 'refused') return 'refused'
  return 'unknown'
}

// answeredBy names who produced an answer: a service sets X-Service on every
// answer, so its absence means the gateway answered itself.
export function answeredBy(answer: Answer<unknown>): string | null {
  if (answer.kind === 'refused' || answer.kind === 'failed' || answer.kind === 'ok') {
    return answer.service ? serviceNames[answer.service] ?? answer.service : 'gateway'
  }
  return null
}

const serviceNames: Record<string, string> = {
  inventory: 'Inventory',
  orders: 'Orders',
  reports: 'Reports',
  support: 'Support',
  accounts: 'Fetch Accounts',
}

// newIdempotencyKey makes a key for one checkout attempt: 32 characters of
// [0-9a-f], inside the Orders service's 16 to 64 characters of [A-Za-z0-9_-].
export function newIdempotencyKey(): string {
  return crypto.randomUUID().replaceAll('-', '')
}
