import type { ReactNode } from 'react'
import { Lightning, LockKey, TextAa, UserSwitch } from '@phosphor-icons/react'
import { send, type Answer, type Request } from '../lib/api'
import { hasRole } from '../lib/session'
import type { Role, User } from '../lib/types'

// The security lab's tests: each one deliberate wrong request, and what the
// Fetch baseline policy should answer for the person signed in.

export type By = 'gateway' | 'service'

export interface Expected {
  status: number
  by: By
  // why says, in plain words, which check should answer.
  why: string
}

export interface Attack {
  id: string
  title: string
  // request describes what is sent.
  request: string
  token: boolean
  expected: (user: User | null) => Expected
  run: (token: string | null, signal: AbortSignal, user: User | null) => Promise<Answer<unknown>[]>
}

interface Group {
  id: string
  title: string
  icon: ReactNode
  attacks: Attack[]
}

const order = {
  merchant: 'MER-001',
  items: [{ item: 'ITM-001', quantity: 1 }],
  address: { line1: '12 Jalan Telawi 3', line2: '', postcode: '59100', city: 'Kuala Lumpur', state: 'Wilayah Persekutuan Kuala Lumpur' },
  payment: 'cash',
}

const one = (request: Request) => send<unknown>(request).then((answer) => [answer])

// altered changes one character in the middle of the token's signature, where
// every bit counts, so the signature no longer matches.
function altered(token: string): string {
  const [header, payload, signature = ''] = token.split('.')
  const middle = Math.floor(signature.length / 2)
  return `${header}.${payload}.${signature.slice(0, middle)}${signature[middle] === 'A' ? 'B' : 'A'}${signature.slice(middle + 1)}`
}

// extended moves the token's expiry a day later without re-signing it.
function extended(token: string): string {
  const [header, payload, signature] = token.split('.')
  const claims = JSON.parse(atob(payload.replaceAll('-', '+').replaceAll('_', '/'))) as { exp?: number }
  claims.exp = (claims.exp ?? Math.floor(Date.now() / 1000)) + 86_400
  const encoded = btoa(JSON.stringify(claims)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
  return `${header}.${encoded}.${signature}`
}

const gatewayRefuses = (status: number, why: string): Expected => ({ status, by: 'gateway', why })
const needs = (role: Role, user: User | null, allowed: Expected, why: string): Expected =>
  !user ? gatewayRefuses(401, 'Sign-in check: no token') : hasRole(user, role) ? allowed : gatewayRefuses(403, `Role check: ${why}`)

export const groups: Group[] = [
  {
    id: 'token',
    title: 'Sign-in token',
    icon: <LockKey size={18} weight="bold" />,
    attacks: [
      {
        id: 'no-token',
        title: 'No token',
        request: 'GET /orders/mine with no Authorization header',
        token: false,
        expected: () => gatewayRefuses(401, 'Sign-in check: a token is required'),
        run: (_, signal) => one({ method: 'GET', path: '/orders/mine', signal }),
      },
      {
        id: 'not-a-token',
        title: 'Not a token',
        request: 'GET /orders/mine with “Bearer not-a-token”',
        token: false,
        expected: () => gatewayRefuses(401, 'Sign-in check: the token cannot be read'),
        run: (_, signal) => one({ method: 'GET', path: '/orders/mine', token: 'not-a-token', signal }),
      },
      {
        id: 'tampered',
        title: 'Tampered token',
        request: 'Your token with one signature character changed',
        token: true,
        expected: () => gatewayRefuses(401, 'Sign-in check: the signature does not match'),
        run: (token, signal) => one({ method: 'GET', path: '/accounts/me', token: altered(token!), signal }),
      },
      {
        id: 'extended',
        title: 'Token with a longer expiry',
        request: 'Your token with its expiry moved a day later, not re-signed',
        token: true,
        expected: () => gatewayRefuses(401, 'Sign-in check: the edited token no longer matches its signature'),
        run: (token, signal) => one({ method: 'GET', path: '/accounts/me', token: extended(token!), signal }),
      },
    ],
  },
  {
    id: 'roles',
    title: 'Identity and roles',
    icon: <UserSwitch size={18} weight="bold" />,
    attacks: [
      {
        id: 'forged-headers',
        title: 'Forged identity headers',
        request: 'GET /orders/mine claiming X-Gateway-Subject: cust-002 and X-Gateway-Roles: OPERATIONS',
        token: true,
        expected: (user) => needs('CUSTOMER', user, { status: 200, by: 'service', why: 'Orders answers with your own orders only: the gateway replaced the forged headers' }, 'only customers list their orders'),
        run: (token, signal) =>
          one({
            method: 'GET',
            path: '/orders/mine',
            token,
            headers: { 'X-Gateway-Subject': 'cust-002', 'X-Gateway-Roles': 'OPERATIONS', 'X-Request-Id': 'forged-by-the-browser' },
            signal,
          }),
      },
      {
        id: 'edit-menu',
        title: 'Edit a menu item',
        request: 'PATCH /inventory/items/ITM-012 (Bintang Mamak’s Mee Goreng), an empty change',
        token: true,
        expected: (user) =>
          needs(
            'MERCHANT',
            user,
            user?.merchant === 'MER-002'
              ? { status: 422, by: 'service', why: 'Inventory refuses an empty change, so nothing is altered' }
              : { status: 404, by: 'service', why: 'Inventory checks the item belongs to your shop' },
            'only merchants edit menus',
          ),
        run: (token, signal) => one({ method: 'PATCH', path: '/inventory/items/ITM-012', token, body: {}, signal }),
      },
      {
        id: 'all-orders',
        title: 'Read every order',
        request: 'GET /orders, the platform-wide order list',
        token: true,
        expected: (user) =>
          !user
            ? gatewayRefuses(401, 'Sign-in check: no token')
            : hasRole(user, 'SUPPORT') || hasRole(user, 'OPERATIONS')
              ? { status: 200, by: 'service', why: 'Support and operations may read every order' }
              : gatewayRefuses(403, 'Role check: only support and operations read every order'),
        run: (token, signal) => one({ method: 'GET', path: '/orders?limit=5', token, signal }),
      },
      {
        id: 'stock',
        title: 'Read exact stock',
        request: 'GET /inventory/stock/ITM-502 (Cooking Oil at Fetch Mart Express)',
        token: true,
        expected: (user) =>
          !user
            ? gatewayRefuses(401, 'Sign-in check: no token')
            : hasRole(user, 'OPERATIONS') || hasRole(user, 'MERCHANT')
              ? { status: hasRole(user, 'OPERATIONS') || user.merchant === 'MER-005' ? 200 : 404, by: 'service', why: 'Merchants read only their own stock; operations read all' }
              : gatewayRefuses(403, 'Role check: the baseline gives customers no stock permit'),
        run: (token, signal) => one({ method: 'GET', path: '/inventory/stock/ITM-502', token, signal }),
      },
    ],
  },
  {
    id: 'shape',
    title: 'Request shape',
    icon: <TextAa size={18} weight="bold" />,
    attacks: [
      {
        id: 'malformed',
        title: 'Malformed order',
        request: 'POST /orders with items given as the text “lots” instead of a list',
        token: true,
        expected: (user) => needs('CUSTOMER', user, gatewayRefuses(400, 'Body check: items must be a list of 1 to 20'), 'only customers place orders'),
        run: (token, signal) => one({ method: 'POST', path: '/orders', token, body: { ...order, items: 'lots' }, signal }),
      },
      {
        id: 'oversized',
        title: 'Oversized order',
        request: 'POST /orders with a 5,000-character note (the limit is 4,096 bytes)',
        token: true,
        expected: (user) => needs('CUSTOMER', user, gatewayRefuses(413, 'Body check: larger than the operation allows'), 'only customers place orders'),
        run: (token, signal) => one({ method: 'POST', path: '/orders', token, body: { ...order, note: 'x'.repeat(5000) }, signal }),
      },
      {
        id: 'not-json',
        title: 'Not JSON',
        request: 'POST /orders with a plain-text body',
        token: true,
        expected: (user) => needs('CUSTOMER', user, gatewayRefuses(415, 'Body check: orders must be JSON'), 'only customers place orders'),
        run: (token, signal) => one({ method: 'POST', path: '/orders', token, raw: { body: 'merchant=MER-001&item=ITM-001', contentType: 'text/plain' }, signal }),
      },
      {
        id: 'encoded-slash',
        title: 'Encoded slash in the path',
        request: 'GET /orders%2Fmine, a path with a hidden separator',
        token: false,
        expected: () => gatewayRefuses(400, 'Request form: encoded separators are refused before routing'),
        run: (token, signal) => one({ method: 'GET', path: '/orders%2Fmine', token, signal }),
      },
      {
        id: 'wrong-method',
        title: 'Method not allowed',
        request: 'DELETE /orders',
        token: false,
        expected: () => gatewayRefuses(405, 'Routing: the path exists, the method does not'),
        run: (token, signal) => one({ method: 'DELETE', path: '/orders', token, signal }),
      },
      {
        id: 'admin-path',
        title: 'Admin path on the public site',
        request: 'GET /admin/policy on the Fetch address',
        token: false,
        expected: () => gatewayRefuses(404, 'Routing: the Admin API is on its own listener, not here'),
        run: (token, signal) => one({ method: 'GET', path: '/admin/policy', token, signal }),
      },
    ],
  },
  {
    id: 'rate',
    title: 'Rate limits',
    icon: <Lightning size={18} weight="bold" />,
    attacks: [
      {
        id: 'flood-checkout',
        title: 'Flood checkout as me (12 orders)',
        request: '12 POST /orders in a row with empty bodies, as the signed-in customer (limit: 10 a minute)',
        token: true,
        expected: (user) => needs('CUSTOMER', user, gatewayRefuses(429, 'Rate check: once 10 orders in a minute are reached, counting any sent earlier; until then each is refused for its empty body'), 'only customers place orders'),
        run: async (token, signal) => {
          const answers: Answer<unknown>[] = []
          for (let n = 0; n < 12 && !signal.aborted; n++) answers.push(await send<unknown>({ method: 'POST', path: '/orders', token, body: {}, signal }))
          return answers
        },
      },
    ],
  },
]
