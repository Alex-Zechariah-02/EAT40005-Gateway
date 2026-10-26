// Development only (npm run dev:mock). A stand-in for the Admin API endpoints
// used in local screen development (recent events, the event stream, traffic,
// impact preview, blocked accounts, keys, administrators).
// Every other /admin path still goes to the real
// gateway. The app never imports this file, so it is never part of a
// production build, and it is never test evidence: browser tests run against
// the real gateway.
import type { Connect, Plugin } from 'vite'

type Stage = 'route' | 'access' | 'token' | 'blocked' | 'permit' | 'rate' | 'validation' | 'record' | 'forward'
interface DevEvent {
  request_id: string
  time: string
  method: string
  path: string
  operation: string | null
  api: string | null
  subject: string | null
  roles: string[]
  client_address: string
  decision: 'allowed' | 'refused' | 'failed'
  status: number
  stage: Stage
  code: string | null
  generation: string
  duration_ms: number
}

// A few of the Fetch baseline operations and the demonstration accounts that
// use them.
const traffic: { method: string; path: string; operation: string; api: string; who: [string, string][] | null }[] = [
  { method: 'GET', path: '/inventory/merchants', operation: 'inventory.merchants', api: 'inventory', who: null },
  { method: 'GET', path: '/inventory/merchants/MER-001', operation: 'inventory.merchant', api: 'inventory', who: null },
  { method: 'GET', path: '/inventory/search', operation: 'inventory.search', api: 'inventory', who: null },
  { method: 'GET', path: '/orders/mine', operation: 'orders.mine', api: 'orders', who: [['cust-001', 'CUSTOMER'], ['cust-003', 'CUSTOMER']] },
  { method: 'POST', path: '/orders', operation: 'orders.create', api: 'orders', who: [['cust-001', 'CUSTOMER'], ['cust-002', 'CUSTOMER']] },
  { method: 'GET', path: '/orders/merchant', operation: 'orders.merchant', api: 'orders', who: [['merchant-001', 'MERCHANT'], ['merchant-005', 'MERCHANT']] },
  { method: 'GET', path: '/orders/jobs', operation: 'orders.jobs', api: 'orders', who: [['rider-01', 'RIDER'], ['rider-02', 'RIDER']] },
  { method: 'GET', path: '/support/queue', operation: 'support.queue', api: 'support', who: [['support-01', 'SUPPORT']] },
  { method: 'GET', path: '/reports/summary', operation: 'reports.summary', api: 'reports', who: [['ops-01', 'OPERATIONS']] },
  { method: 'POST', path: '/accounts/refresh', operation: 'accounts.refresh', api: 'accounts', who: [['cust-001', 'CUSTOMER'], ['rider-01', 'RIDER']] },
]

const refusals: Omit<DevEvent, 'request_id' | 'time' | 'generation' | 'duration_ms'>[] = [
  { method: 'POST', path: '/orders', operation: 'orders.create', api: 'orders', subject: 'cust-002', roles: ['CUSTOMER'], client_address: '127.0.0.1', decision: 'refused', status: 429, stage: 'rate', code: 'rate_limited' },
  { method: 'PATCH', path: '/inventory/items/ITM-012', operation: 'inventory.item.update', api: 'inventory', subject: 'support-01', roles: ['SUPPORT'], client_address: '127.0.0.1', decision: 'refused', status: 403, stage: 'permit', code: 'forbidden' },
  { method: 'POST', path: '/accounts/login', operation: 'accounts.login', api: 'accounts', subject: null, roles: [], client_address: '127.0.0.50', decision: 'refused', status: 429, stage: 'rate', code: 'rate_limited' },
  { method: 'GET', path: '/orders/mine', operation: 'orders.mine', api: 'orders', subject: null, roles: [], client_address: '127.0.0.1', decision: 'refused', status: 401, stage: 'token', code: 'unauthenticated' },
  { method: 'POST', path: '/orders', operation: 'orders.create', api: 'orders', subject: 'cust-001', roles: ['CUSTOMER'], client_address: '127.0.0.1', decision: 'refused', status: 400, stage: 'validation', code: 'bad_request' },
  { method: 'GET', path: '/orders/jobs', operation: 'orders.jobs', api: 'orders', subject: 'cust-003', roles: ['CUSTOMER'], client_address: '127.0.0.1', decision: 'refused', status: 403, stage: 'blocked', code: 'account_blocked' },
  { method: 'GET', path: '/admin/policy', operation: null, api: null, subject: null, roles: [], client_address: '127.0.0.1', decision: 'refused', status: 404, stage: 'route', code: 'not_found' },
]

const events: DevEvent[] = []
let generated = Date.now() - 3_600_000
let counter = 0

function hex(n: number): string {
  let out = ''
  for (let i = 0; i < n; i++) out += Math.floor(Math.random() * 16).toString(16)
  return out
}

function make(at: number): DevEvent {
  counter++
  const base = { request_id: hex(32), time: new Date(at).toISOString(), generation: 'r7+o1-4ebc700ff7ba', duration_ms: Math.round((1 + Math.random() * 9) * 10) / 10 }
  // About one request in twelve is refused, more during a burst each few minutes.
  const burst = Math.floor(at / 1000) % 240 < 20
  if (Math.random() < (burst ? 0.35 : 0.08)) return { ...base, ...refusals[burst ? 0 : counter % refusals.length], duration_ms: 0.4 }
  const pick = traffic[counter % traffic.length]
  const [subject, role] = pick.who ? pick.who[counter % pick.who.length] : [null, null]
  return {
    ...base,
    method: pick.method,
    path: pick.path,
    operation: pick.operation,
    api: pick.api,
    subject,
    roles: role ? [role] : [],
    client_address: '127.0.0.1',
    decision: 'allowed',
    status: pick.method === 'POST' && pick.operation === 'orders.create' ? 201 : 200,
    stage: 'forward',
    code: null,
  }
}

// advance adds events up to now, about six a second, and keeps the newest
// 10,000 as the gateway's window does.
function advance(): DevEvent[] {
  const now = Date.now()
  const fresh: DevEvent[] = []
  while (generated < now) {
    generated += 80 + Math.random() * 250
    const event = make(generated)
    events.push(event)
    fresh.push(event)
  }
  if (events.length > 10_000) events.splice(0, events.length - 10_000)
  return fresh
}

function summary(windowSeconds: number) {
  advance()
  const from = Date.now() - windowSeconds * 1000
  const inWindow = events.filter((e) => Date.parse(e.time) >= from)
  const bucket = windowSeconds / 60
  const per = new Map<number, { allowed: number; refused: number }>()
  for (let n = 0; n < 60; n++) per.set(n, { allowed: 0, refused: 0 })
  const apis = new Map<string, { total: number; allowed: number; refused: number; times: number[] }>()
  const codes = new Map<string, number>()
  for (const e of inWindow) {
    const slot = Math.min(59, Math.floor((Date.parse(e.time) - from) / 1000 / bucket))
    const allowed = e.decision === 'allowed'
    per.get(slot)![allowed ? 'allowed' : 'refused']++
    if (e.api) {
      const a = apis.get(e.api) ?? { total: 0, allowed: 0, refused: 0, times: [] }
      a.total++
      a[allowed ? 'allowed' : 'refused']++
      if (allowed) a.times.push(e.duration_ms)
      apis.set(e.api, a)
    }
    if (e.code) codes.set(e.code, (codes.get(e.code) ?? 0) + 1)
  }
  const quantile = (times: number[], q: number) => {
    const sorted = [...times].sort((a, b) => a - b)
    return sorted.length ? sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))] : 0
  }
  const allowed = inWindow.filter((e) => e.decision === 'allowed').length
  return {
    window_seconds: windowSeconds,
    total: inWindow.length,
    allowed,
    refused: inWindow.length - allowed,
    by_api: [...apis.entries()]
      .sort((a, b) => b[1].total - a[1].total)
      .map(([api, a]) => ({ api, total: a.total, allowed: a.allowed, refused: a.refused, p50_ms: quantile(a.times, 0.5), p95_ms: quantile(a.times, 0.95) })),
    refusal_reasons: [...codes.entries()].sort((a, b) => b[1] - a[1]).map(([code, count]) => ({ code, count })),
    per_second: [...per.entries()].map(([n, v]) => ({ at: new Date(from + n * bucket * 1000).toISOString(), ...v })),
  }
}

const blocked = new Map<string, { subject: string; reason: string; blocked_by: string; blocked_at: string }>([
  ['cust-003', { subject: 'cust-003', reason: 'Suspected card fraud, reported by the bank', blocked_by: 'root', blocked_at: new Date(Date.now() - 1_800_000).toISOString() }],
])
const keys = [
  { kid: 'fetch-2026-10-a', fingerprint: 'sha256:3f9a1c0b7e2d4a6f8b1c3e5d7f9a0b2c4d6e8f0a1b3c5d7e9f0a2b4c6d8e0f1a', loaded_at: new Date(Date.now() - 7_200_000).toISOString() },
]

type Send = (status: number, body: unknown) => void

function handle(method: string, url: URL, body: string, send: Send): boolean {
  const path = url.pathname
  if (method === 'GET' && path === '/admin/traffic') {
    const span = Number(url.searchParams.get('window') ?? 60)
    if (![60, 300, 3600].includes(span)) send(400, { error: 'invalid_query' })
    else send(200, summary(span))
    return true
  }
  if (method === 'GET' && path === '/admin/events') {
    advance()
    const q = url.searchParams
    const limit = Math.min(100, Number(q.get('limit') ?? 20))
    const items = [...events]
      .reverse()
      .filter(
        (e) =>
          (!q.get('ref') || e.request_id.startsWith(q.get('ref')!)) &&
          (!q.get('subject') || e.subject === q.get('subject')) &&
          (!q.get('operation') || e.operation === q.get('operation')) &&
          (!q.get('decision') || e.decision === q.get('decision')),
      )
      .slice(0, limit)
    send(200, { items })
    return true
  }
  const impact = path.match(/^\/admin\/candidates\/(\d+)\/impact$/)
  if (method === 'POST' && impact) {
    advance()
    send(200, {
      window: { from: events[0].time, to: events[events.length - 1].time, requests: events.length },
      flips: [
        { operation: 'orders.jobs', subject: 'rider-01', roles: ['RIDER'], before: 'allowed', after: 'refused', after_code: 'forbidden', count: 142 },
        { operation: 'orders.jobs', subject: 'rider-02', roles: ['RIDER'], before: 'allowed', after: 'refused', after_code: 'forbidden', count: 97 },
      ],
      not_previewed: ['rate', 'body'],
    })
    return true
  }
  if (method === 'GET' && path === '/admin/blocked-accounts') {
    send(200, { items: [...blocked.values()] })
    return true
  }
  const account = path.match(/^\/admin\/blocked-accounts\/([^/]+)$/)
  if (account && method === 'PUT') {
    const subject = decodeURIComponent(account[1])
    const reason = (JSON.parse(body || '{}') as { reason?: string }).reason ?? ''
    if (reason.length < 1 || reason.length > 200) {
      send(400, { error: 'bad_request', field: 'reason' })
      return true
    }
    const entry = { subject, reason, blocked_by: 'root', blocked_at: new Date().toISOString() }
    blocked.set(subject, entry)
    send(200, entry)
    return true
  }
  if (account && method === 'DELETE') {
    const subject = decodeURIComponent(account[1])
    if (!blocked.delete(subject)) send(404, { error: 'not_found' })
    else send(200, { subject, unblocked: true })
    return true
  }
  if (method === 'GET' && path === '/admin/keys') {
    send(200, { items: keys })
    return true
  }
  if (method === 'POST' && path === '/admin/keys/reload') {
    send(200, { items: keys })
    return true
  }
  if (method === 'GET' && path === '/admin/administrators') {
    send(200, { items: [{ name: 'root', permissions: ['read', 'read_history', 'manage_subjects', 'manage_policy'], status: 'active' }] })
    return true
  }
  return false
}

function stream(res: Parameters<Connect.NextHandleFunction>[1]) {
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
  advance()
  const timer = setInterval(() => {
    for (const event of advance()) res.write(`event: request\ndata: ${JSON.stringify(event)}\n\n`)
  }, 400)
  res.on('close', () => clearInterval(timer))
}

const ours = /^\/admin\/(traffic|events(\/stream)?|candidates\/\d+\/impact|blocked-accounts(\/[^/]+)?|keys(\/reload)?|administrators)$/

export function adminStandIn(): Plugin {
  return {
    name: 'admin-stand-in',
    apply: 'serve',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? '/', 'http://localhost')
        // Anything else, body included, goes on to the real gateway untouched.
        if (!ours.test(url.pathname)) return next()
        if (req.method === 'GET' && url.pathname === '/admin/events/stream') return stream(res)
        let body = ''
        req.on('data', (chunk: Buffer) => (body += chunk.toString()))
        req.on('end', () => {
          const answered = handle(req.method ?? 'GET', url, body, (status, payload) => {
            res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' })
            res.end(JSON.stringify(payload))
          })
          if (!answered) next()
        })
      })
    },
  }
}
