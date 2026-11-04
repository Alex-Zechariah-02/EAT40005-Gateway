// Development only: the Orders stand-in, with the same answers and errors
// as the real service for the requests the app sends.
import type { Address, Order, User } from '../lib/types'
import * as seed from './seed'
import { all, err, findOrder, freeDelivery, merchantOf, now, on, type Handler, type Json } from './routes'

const idempotency = new Map<string, { body: string; answer: [number, unknown] }>()
const sees = (user: User, order: Order) =>
  user.roles.includes('SUPPORT') || user.roles.includes('OPERATIONS') || order.customer === user.id || (user.roles.includes('MERCHANT') && order.merchant === user.merchant) || order.rider === user.id
const move = (order: Order, status: Order['status']) => {
  order.status = status
  order.timeline.push({ status, at: now() })
  return [200, { order }] as [number, unknown]
}


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

// addOrders adds the Orders routes; the caller fixes their order in the table.
export function addOrders() {
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
}
