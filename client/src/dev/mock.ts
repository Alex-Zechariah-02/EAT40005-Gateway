// Development only (npm run dev:mock). A stand-in for the gateway and the
// services, answering with the same shapes and errors for local screen
// development. It is never part of a
// production build and never test evidence: browser tests run against the
// real gateway.
//
// Fault injection for building the unknown-outcome screens: in the browser
// console, sessionStorage.setItem('fetch.dev.fail', 'orders.create=504') or
// '...=lost' (the write is applied, then the answer is lost).
import type { Address, Order, Role, Ticket, User } from '../lib/types'
import * as seed from './seed'

type Json = Record<string, unknown>
interface Ctx { method: string; path: string; query: URLSearchParams; body: Json | null; user: User | null; headers: Headers }
type Handler = (ctx: Ctx, match: RegExpMatchArray) => [number, unknown]

const all: Role[] = ['CUSTOMER', 'MERCHANT', 'RIDER', 'SUPPORT', 'OPERATIONS']
const routes: [string, RegExp, string, Role[] | null, string, Handler][] = []
const on = (method: string, pattern: RegExp, op: string, roles: Role[] | null, service: string, handler: Handler) => routes.push([method, pattern, op, roles, service, handler])

const ref = () => Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('')
const err = (status: number, error: string, message: string, field?: string): [number, unknown] => [status, { error, message, ...(field ? { field } : {}) }]
const now = () => new Date().toISOString()

function stockState(id: string) {
  const row = seed.stock.get(id)!
  if (row.stock === null) return 'made_to_order'
  const free = row.stock - row.reserved
  return free <= 0 ? 'out' : free <= 15 ? 'low' : 'in_stock'
}
const itemView = (id: string) => ({ ...seed.items.find((i) => i.id === id)!, stock_state: stockState(id) })
const merchantOf = (id: string) => seed.merchants.find((m) => m.id === id)

function freeDelivery(customer: string, merchantId: string) {
  const merchant = merchantOf(merchantId)
  if (!merchant || merchant.section !== 'mart') return false
  return !seed.orders.some((o) => o.customer === customer && merchantOf(o.merchant)?.section === 'mart' && o.status !== 'cancelled')
}

// Inventory
on('GET', /^\/inventory\/categories$/, 'inventory.categories', null, 'inventory', () => [200, { items: seed.categories }])
on('GET', /^\/inventory\/merchants$/, 'inventory.merchants', null, 'inventory', ({ query }) => {
  const section = query.get('section')
  const category = query.get('category')
  return [200, { items: seed.merchants.filter((m) => (!section || m.section === section) && (!category || m.category === category)) }]
})
on('GET', /^\/inventory\/merchants\/([^/]+)$/, 'inventory.merchant', null, 'inventory', (_, [, id]) => {
  const merchant = merchantOf(id)
  if (!merchant) return err(404, 'not_found', 'No such merchant.')
  const groups = [...new Set(seed.items.filter((i) => i.merchant === id).map((i) => i.group))]
  return [200, { merchant, menu: groups.map((group) => ({ group, items: seed.items.filter((i) => i.merchant === id && i.group === group).map((i) => itemView(i.id)) })) }]
})
on('GET', /^\/inventory\/search$/, 'inventory.search', null, 'inventory', ({ query }) => {
  const q = (query.get('q') ?? '').trim().toLowerCase()
  if (q.length < 2 || q.length > 40) return err(400, 'invalid_query', 'Search for 2 to 40 characters.', 'q')
  const section = query.get('section')
  const hits = seed.items
    .filter((i) => i.name.toLowerCase().includes(q) || i.description.toLowerCase().includes(q) || merchantOf(i.merchant)!.name.toLowerCase().includes(q))
    .filter((i) => !section || merchantOf(i.merchant)!.section === section)
  return [200, { items: hits.map((i) => ({ merchant: merchantOf(i.merchant), item: itemView(i.id) })) }]
})
on('GET', /^\/inventory\/items\/([^/]+)$/, 'inventory.item', null, 'inventory', (_, [, id]) => {
  const item = seed.items.find((i) => i.id === id)
  return item ? [200, { item: itemView(id), merchant: merchantOf(item.merchant) }] : err(404, 'not_found', 'No such item.')
})
on('PATCH', /^\/inventory\/items\/([^/]+)$/, 'inventory.item.update', ['MERCHANT'], 'inventory', ({ body, user }, [, id]) => {
  const item = seed.items.find((i) => i.id === id)
  if (!item || item.merchant !== user!.merchant) return err(404, 'not_found', 'No such item.')
  if (!body || (body.price_sen === undefined && body.available === undefined)) return err(422, 'invalid_change', 'Change the price or the availability.')
  if (typeof body.price_sen === 'number') item.price_sen = body.price_sen
  if (typeof body.available === 'boolean') item.available = body.available
  return [200, { item: itemView(id) }]
})
const stockAccess = (user: User, id: string) => {
  const item = seed.items.find((i) => i.id === id)
  if (!item || seed.stock.get(id)!.stock === null) return null
  if (user.roles.includes('MERCHANT') && !user.roles.includes('OPERATIONS') && item.merchant !== user.merchant) return null
  return item
}
on('GET', /^\/inventory\/stock\/([^/]+)$/, 'inventory.stock.read', ['MERCHANT', 'OPERATIONS'], 'inventory', ({ user }, [, id]) => {
  if (!stockAccess(user!, id)) return err(404, 'not_found', 'No such stocked item.')
  const row = seed.stock.get(id)!
  return [200, { item: id, stock: row.stock, reserved: row.reserved }]
})
on('PATCH', /^\/inventory\/stock\/([^/]+)$/, 'inventory.stock.adjust', ['MERCHANT', 'OPERATIONS'], 'inventory', ({ user, body }, [, id]) => {
  if (!stockAccess(user!, id)) return err(404, 'not_found', 'No such stocked item.')
  const delta = Number(body?.delta)
  if (!delta) return err(422, 'invalid_change', 'Change the stock by at least one.')
  const row = seed.stock.get(id)!
  if (row.stock! + delta < 0) return err(409, 'insufficient_stock', `Only ${row.stock} in stock.`)
  row.stock = row.stock! + delta
  return [200, { item: id, stock: row.stock, reserved: row.reserved }]
})

// Orders
const idempotency = new Map<string, { body: string; answer: [number, unknown] }>()
const findOrder = (id: string) => seed.orders.find((o) => o.id === id)
const sees = (user: User, order: Order) =>
  user.roles.includes('SUPPORT') || user.roles.includes('OPERATIONS') || order.customer === user.id || (user.roles.includes('MERCHANT') && order.merchant === user.merchant) || order.rider === user.id
const move = (order: Order, status: Order['status']) => {
  order.status = status
  order.timeline.push({ status, at: now() })
  return [200, { order }] as [number, unknown]
}

on('GET', /^\/orders\/quote$/, 'orders.quote', ['CUSTOMER'], 'orders', ({ user, query }) => {
  const merchant = merchantOf(query.get('merchant') ?? '')
  if (!merchant) return err(400, 'invalid_query', 'Name a merchant.', 'merchant')
  const free = freeDelivery(user!.id, merchant.id)
  return [200, { merchant: merchant.id, delivery_fee_sen: free ? 0 : merchant.delivery_fee_sen, free_delivery: free, reason: free ? 'first_mart_order' : null }]
})
on('GET', /^\/orders\/wallet$/, 'orders.wallet', ['CUSTOMER'], 'orders', ({ user }) => [200, { balance_sen: seed.wallets.get(user!.id) ?? 0 }])
on('GET', /^\/orders\/mine$/, 'orders.mine', ['CUSTOMER'], 'orders', ({ user }) => [200, { items: seed.orders.filter((o) => o.customer === user!.id).toReversed() }])
on('POST', /^\/orders$/, 'orders.create', ['CUSTOMER'], 'orders', ({ user, body, headers }) => {
  const key = headers.get('Idempotency-Key')
  const text = JSON.stringify(body)
  if (key && idempotency.has(key)) {
    const seen = idempotency.get(key)!
    return seen.body === text ? seen.answer : err(409, 'idempotency_conflict', 'This key was already used for a different order.')
  }
  const answer = createOrder(user!, body!)
  if (key) idempotency.set(key, { body: text, answer })
  return answer
})
function createOrder(user: User, body: Json): [number, unknown] {
  const merchant = merchantOf(String(body.merchant))
  const lines = body.items as { item: string; quantity: number }[]
  if (!merchant) return err(422, 'item_unavailable', 'That shop is not on Fetch.', 'merchant')
  if (!merchant.open) return err(422, 'merchant_closed', `${merchant.name} is closed right now.`)
  for (const [i, line] of lines.entries()) {
    const item = seed.items.find((x) => x.id === line.item)
    if (!item) return err(422, 'item_unavailable', 'An item is no longer on the menu.', `items[${i}].item`)
    if (item.merchant !== merchant.id) return err(422, 'mixed_merchants', 'Every item must come from the same shop.', `items[${i}].item`)
    if (!item.available) return err(422, 'item_unavailable', `${item.name} is not available today.`, `items[${i}].item`)
    const row = seed.stock.get(item.id)!
    if (row.stock !== null && row.stock - row.reserved < line.quantity) return err(409, 'out_of_stock', `${item.name} is out of stock.`, `items[${i}].item`)
  }
  const orderLines = lines.map((line) => {
    const item = seed.items.find((x) => x.id === line.item)!
    return { item: item.id, name: item.name, quantity: line.quantity, unit_price_sen: item.price_sen }
  })
  const subtotal = orderLines.reduce((sum, l) => sum + l.quantity * l.unit_price_sen, 0)
  const fee = freeDelivery(user.id, merchant.id) ? 0 : merchant.delivery_fee_sen
  if (body.payment === 'wallet') {
    const balance = seed.wallets.get(user.id) ?? 0
    if (balance < subtotal + fee) return err(409, 'insufficient_wallet', `Your wallet has RM ${(balance / 100).toFixed(2)}.`)
    seed.wallets.set(user.id, balance - subtotal - fee)
  }
  for (const line of lines) {
    const row = seed.stock.get(line.item)!
    if (row.stock !== null) row.reserved += line.quantity
  }
  const order: Order = {
    id: `ORD-${1005 + seed.orders.filter((o) => Number(o.id.slice(4)) >= 1005).length}`,
    customer: user.id, merchant: merchant.id, merchant_name: merchant.name, items: orderLines,
    subtotal_sen: subtotal, delivery_fee_sen: fee, total_sen: subtotal + fee, payment: body.payment as Order['payment'],
    address: body.address as Address, note: String(body.note ?? ''), status: 'placed', rider: null,
    timeline: [{ status: 'placed', at: now() }], created_at: now(),
  }
  seed.orders.push(order)
  return [201, { order }]
}
on('GET', /^\/orders\/detail\/([^/]+)$/, 'orders.detail', all, 'orders', ({ user }, [, id]) => {
  const order = findOrder(id)
  return order && sees(user!, order) ? [200, { order }] : err(404, 'not_found', 'No such order.')
})
on('POST', /^\/orders\/cancel\/([^/]+)$/, 'orders.cancel', ['CUSTOMER'], 'orders', ({ user }, [, id]) => {
  const order = findOrder(id)
  if (!order || order.customer !== user!.id) return err(404, 'not_found', 'No such order.')
  if (order.status !== 'placed') return err(409, 'invalid_transition', 'The shop has already started this order, so it can no longer be cancelled.')
  for (const line of order.items) {
    const row = seed.stock.get(line.item)!
    if (row.stock !== null) row.reserved -= line.quantity
  }
  if (order.payment === 'wallet') seed.wallets.set(user!.id, (seed.wallets.get(user!.id) ?? 0) + order.total_sen)
  return move(order, 'cancelled')
})
on('GET', /^\/orders\/merchant$/, 'orders.merchant', ['MERCHANT'], 'orders', ({ user, query }) => {
  const status = query.get('status')
  return [200, { items: seed.orders.filter((o) => o.merchant === user!.merchant && (!status || o.status === status)).toReversed() }]
})
const merchantMove = (from: Order['status'], to: Order['status']): Handler => ({ user }, [, id]) => {
  const order = findOrder(id)
  if (!order || order.merchant !== user!.merchant) return err(404, 'not_found', 'No such order.')
  if (order.status !== from) return err(409, 'invalid_transition', `This order is ${order.status.replace('_', ' ')}.`)
  if (to === 'ready') {
    for (const line of order.items) {
      const row = seed.stock.get(line.item)!
      if (row.stock !== null) {
        row.stock -= line.quantity
        row.reserved -= line.quantity
      }
    }
  }
  return move(order, to)
}
on('POST', /^\/orders\/accept\/([^/]+)$/, 'orders.accept', ['MERCHANT'], 'orders', merchantMove('placed', 'accepted'))
on('POST', /^\/orders\/ready\/([^/]+)$/, 'orders.ready', ['MERCHANT'], 'orders', merchantMove('accepted', 'ready'))
on('GET', /^\/orders\/jobs$/, 'orders.jobs', ['RIDER'], 'orders', ({ user }) => [200, {
  available: seed.orders.filter((o) => o.status === 'ready' && !o.rider),
  mine: seed.orders.filter((o) => o.rider === user!.id && o.status === 'picked_up'),
}])
on('POST', /^\/orders\/pickup\/([^/]+)$/, 'orders.pickup', ['RIDER'], 'orders', ({ user }, [, id]) => {
  const order = findOrder(id)
  if (!order) return err(404, 'not_found', 'No such order.')
  if (order.rider && order.rider !== user!.id) return err(409, 'already_assigned', 'Another rider picked this order up first.')
  if (order.status !== 'ready') return err(409, 'invalid_transition', 'This order is not ready for pickup.')
  order.rider = user!.id
  return move(order, 'picked_up')
})
on('POST', /^\/orders\/deliver\/([^/]+)$/, 'orders.deliver', ['RIDER'], 'orders', ({ user }, [, id]) => {
  const order = findOrder(id)
  if (!order || order.rider !== user!.id) return err(404, 'not_found', 'No such order.')
  if (order.status !== 'picked_up') return err(409, 'invalid_transition', 'This order is not on its way.')
  return move(order, 'delivered')
})
on('GET', /^\/orders$/, 'orders.all', ['SUPPORT', 'OPERATIONS'], 'orders', ({ query }) => {
  const status = query.get('status')
  return [200, { items: seed.orders.filter((o) => !status || o.status === status).toReversed() }]
})

// Reports (deterministic figures for development)
const days = Array.from({ length: 30 }, (_, i) => new Date(Date.UTC(2026, 9, 1 + i)).toISOString().slice(0, 10))
on('GET', /^\/reports\/merchant$/, 'reports.merchant', ['MERCHANT'], 'reports', ({ user }) => {
  const seedNo = Number(user!.merchant!.slice(4))
  const byDay = days.map((date, i) => {
    const orders = 2 + ((i * 7 + seedNo * 3) % 6)
    return { date, orders, revenue_sen: orders * (1350 + seedNo * 110) }
  })
  return [200, { merchant: user!.merchant, from: days[0], to: days.at(-1), orders: byDay.reduce((s, d) => s + d.orders, 0), revenue_sen: byDay.reduce((s, d) => s + d.revenue_sen, 0), by_day: byDay }]
})
on('GET', /^\/reports\/summary$/, 'reports.summary', ['OPERATIONS'], 'reports', () => [200, { from: '2026-08-01', to: '2026-10-30', orders: 412, revenue_sen: 812345, customers: 3, merchants: 6, average_order_sen: 1972 }])
on('GET', /^\/reports\/sales$/, 'reports.sales', ['OPERATIONS'], 'reports', () => [200, { months: [
  { month: '2026-08', section: 'food', orders: 98, revenue_sen: 168400 }, { month: '2026-08', section: 'mart', orders: 31, revenue_sen: 96250 },
  { month: '2026-09', section: 'food', orders: 112, revenue_sen: 193900 }, { month: '2026-09', section: 'mart', orders: 38, revenue_sen: 121300 },
  { month: '2026-10', section: 'food', orders: 96, revenue_sen: 166500 }, { month: '2026-10', section: 'mart', orders: 37, revenue_sen: 65995 },
] }])
on('GET', /^\/reports\/top-items$/, 'reports.top-items', ['OPERATIONS'], 'reports', () => [200, { items: [
  ['ITM-001', 210], ['ITM-011', 188], ['ITM-004', 166], ['ITM-031', 121], ['ITM-021', 117], ['ITM-501', 64],
].map(([id, quantity]) => {
  const item = seed.items.find((i) => i.id === id)!
  return { item: id, name: item.name, merchant: item.merchant, quantity, revenue_sen: (quantity as number) * item.price_sen }
}) }])

// Support
const visibleTicket = (user: User, ticket: Ticket | undefined) => ticket && (user.roles.includes('SUPPORT') || ticket.customer === user.id)
on('POST', /^\/support\/tickets$/, 'support.create', ['CUSTOMER'], 'support', ({ user, body }) => {
  const order = body?.order ? findOrder(String(body.order)) : null
  if (body?.order && (!order || order.customer !== user!.id)) return err(422, 'invalid_change', 'That order is not one of yours.', 'order')
  const ticket: Ticket = {
    id: `TKT-${1001 + seed.tickets.length}`, customer: user!.id, customer_name: user!.name, order: order?.id ?? null, request_ref: body?.request_ref ? String(body.request_ref) : null,
    subject: String(body!.subject), status: 'open', messages: [{ from: 'customer', author: user!.id, author_name: user!.name, body: String(body!.message), at: now() }], created_at: now(),
  }
  seed.tickets.push(ticket)
  return [201, { ticket }]
})
on('GET', /^\/support\/mine$/, 'support.mine', ['CUSTOMER'], 'support', ({ user }) => [200, { items: seed.tickets.filter((t) => t.customer === user!.id).toReversed() }])
on('GET', /^\/support\/queue$/, 'support.queue', ['SUPPORT'], 'support', ({ query }) => {
  const status = query.get('status')
  return [200, { items: seed.tickets.filter((t) => !status || t.status === status).toReversed() }]
})
on('GET', /^\/support\/ticket\/([^/]+)$/, 'support.ticket', ['CUSTOMER', 'SUPPORT'], 'support', ({ user }, [, id]) => {
  const ticket = seed.tickets.find((t) => t.id === id)
  return visibleTicket(user!, ticket) ? [200, { ticket }] : err(404, 'not_found', 'No such ticket.')
})
on('POST', /^\/support\/reply\/([^/]+)$/, 'support.reply', ['CUSTOMER', 'SUPPORT'], 'support', ({ user, body }, [, id]) => {
  const ticket = seed.tickets.find((t) => t.id === id)
  if (!visibleTicket(user!, ticket)) return err(404, 'not_found', 'No such ticket.')
  if (ticket!.status === 'resolved') return err(409, 'invalid_transition', 'This ticket is resolved. Open a new one if you still need help.')
  const agent = user!.roles.includes('SUPPORT')
  ticket!.messages.push({ from: agent ? 'agent' : 'customer', author: user!.id, author_name: user!.name, body: String(body!.message), at: now() })
  ticket!.status = agent ? 'answered' : 'open'
  return [200, { ticket }]
})
on('POST', /^\/support\/resolve\/([^/]+)$/, 'support.resolve', ['SUPPORT'], 'support', (_, [, id]) => {
  const ticket = seed.tickets.find((t) => t.id === id)
  if (!ticket) return err(404, 'not_found', 'No such ticket.')
  ticket.status = 'resolved'
  return [200, { ticket }]
})

// Fetch Accounts: a development token is "dev.<subject>.<expiry ms>"; it means
// nothing outside this stand-in.
const lifetime = 15 * 60_000
const issue = (user: User): [number, unknown] => {
  const expires = Date.now() + lifetime
  return [200, { token: `dev.${user.id}.${expires}`, expires_at: new Date(expires).toISOString(), user }]
}
on('POST', /^\/accounts\/login$/, 'accounts.login', null, 'accounts', ({ body }) => {
  const user = seed.users.find((u) => u.email === String(body?.email ?? '').trim().toLowerCase())
  return user && body?.password === seed.password ? issue(user) : err(401, 'invalid_credentials', 'That email and password don’t match a Fetch account.')
})
on('POST', /^\/accounts\/refresh$/, 'accounts.refresh', all, 'accounts', ({ user }) => issue(user!))
on('GET', /^\/accounts\/me$/, 'accounts.me', all, 'accounts', ({ user }) => [200, { user }])

function userFor(authorization: string | null): User | null | 'invalid' {
  if (!authorization) return null
  const [, subject, expiry] = authorization.replace(/^Bearer /, '').split('.')
  const user = seed.users.find((u) => u.id === subject)
  return user && Number(expiry) > Date.now() ? user : 'invalid'
}

function respond(status: number, body: unknown, service: string | null): Response {
  const headers = new Headers({ 'Content-Type': 'application/json', 'X-Request-Id': ref() })
  if (service) headers.set('X-Service', service)
  return new Response(JSON.stringify(body), { status, headers })
}

const apiPath = /^\/(inventory|orders|reports|support|accounts)(\/|$)/
const pause = () => new Promise((resolve) => setTimeout(resolve, 180 + Math.random() * 260))

export function installMock() {
  const realFetch = window.fetch.bind(window)
  window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin)
    if (url.origin !== location.origin || !apiPath.test(url.pathname)) return realFetch(input, init)
    await pause()
    init?.signal?.throwIfAborted()
    const method = (init?.method ?? 'GET').toUpperCase()
    const route = routes.find(([m, pattern]) => m === method && pattern.test(url.pathname))
    if (!route) {
      const exists = routes.some(([, pattern]) => pattern.test(url.pathname))
      return exists ? respond(405, { error: 'method_not_allowed', request_id: ref() }, null) : respond(404, { error: 'not_found', request_id: ref() }, null)
    }
    const [, pattern, op, roles, service, handler] = route
    const headers = new Headers(init?.headers)
    const user = userFor(headers.get('Authorization'))
    if (roles) {
      if (!user || user === 'invalid') return respond(401, { error: 'unauthenticated', request_id: ref() }, null)
      if (!user.roles.some((role) => roles.includes(role))) return respond(403, { error: 'forbidden', request_id: ref() }, null)
    }
    const fault = sessionStorage.getItem('fetch.dev.fail')?.split('=')
    let body: Json | null = null
    if (typeof init?.body === 'string') {
      try {
        body = JSON.parse(init.body) as Json
      } catch {
        return respond(400, { error: 'bad_request', request_id: ref() }, null)
      }
    }
    const ctx: Ctx = { method, path: url.pathname, query: url.searchParams, body, user: user === 'invalid' ? null : user, headers }
    if (fault?.[0] === op && fault[1] === '503') return respond(503, { error: 'gateway_unavailable', request_id: ref() }, null)
    const [status, answer] = handler(ctx, url.pathname.match(pattern)!)
    if (fault?.[0] === op && fault[1] === 'lost') throw new TypeError('Failed to fetch')
    if (fault?.[0] === op && fault[1] === '504') return respond(504, { error: 'upstream_timeout', request_id: ref() }, null)
    return respond(status, answer, service)
  }
}
