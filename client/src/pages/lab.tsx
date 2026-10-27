import { useEffect, useRef, useState, type ReactNode } from 'react'
import { CheckCircle, Flask, Lightning, LockKey, ShieldCheck, TextAa, UserSwitch, WarningCircle } from '@phosphor-icons/react'
import { answeredBy, send, type Answer, type Request } from '../lib/api'
import { Link } from '../lib/router'
import { hasRole, onIdentityChange, roleNames, useSession } from '../lib/session'
import type { List, Order, Role, User } from '../lib/types'
import { Badge } from '../ui/badge'
import { Button, ButtonLink } from '../ui/button'
import { cx } from '../ui/cx'
import { RefTag } from '../ui/notice'
import { WorkspaceTitle } from './staff'

// The security lab: each button sends one deliberately wrong request
// through the gateway, as the person signed in on this tab, and sets what the
// Fetch baseline policy should do beside what the
// gateway actually answered. The lab decides nothing and changes no policy;
// it sends with the tab's token directly, so a deliberate 401 never ends the
// person's session. Changing person cancels pending requests and clears the
// results.

type By = 'gateway' | 'service'

interface Expected {
  status: number
  by: By
  // why says, in plain words, which check should answer.
  why: string
}

interface Attack {
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

const groups: Group[] = [
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

interface Result {
  answers: Answer<unknown>[]
  expected: Expected
  // unsent is true when the request could not be built, so nothing was sent.
  unsent?: boolean
}

function observedBy(answer: Answer<unknown>): By | null {
  const by = answeredBy(answer)
  return by === null ? null : by === 'gateway' ? 'gateway' : 'service'
}

// matches compares the last answer with the expectation: the same status
// from the same party (the gateway, or a service).
function matches(result: Result): boolean {
  const last = result.answers[result.answers.length - 1]
  return !!last && 'status' in last && last.status === result.expected.status && observedBy(last) === result.expected.by
}

export function LabPage() {
  const { session } = useSession()
  const user = session?.user ?? null
  const [results, setResults] = useState<Record<string, Result>>({})
  const [busy, setBusy] = useState<string | null>(null)
  const controller = useRef(new AbortController())

  // A change of person cancels what is pending and clears earlier answers.
  useEffect(
    () =>
      onIdentityChange(() => {
        controller.current.abort()
        controller.current = new AbortController()
        setResults({})
        setBusy(null)
      }),
    [],
  )
  // Leaving the page cancels what is pending. The controller is made here so
  // a remount (React's development double mount) starts with a fresh one.
  useEffect(() => {
    controller.current = new AbortController()
    return () => controller.current.abort()
  }, [])

  async function fire(attack: Attack) {
    if (busy) return
    const signal = controller.current.signal
    const expected = attack.expected(user)
    setBusy(attack.id)
    let result: Result
    try {
      const answers = await attack.run(session?.token ?? null, signal, user)
      result = { answers: answers.filter((a) => a.kind !== 'cancelled'), expected }
    } catch {
      // Building the request failed (for example a token that is not a
      // JWT), so nothing was sent.
      result = { answers: [], expected, unsent: true }
    }
    if (signal.aborted) return
    setBusy(null)
    setResults((all) => ({ ...all, [attack.id]: result }))
  }

  const ran = Object.values(results)
  const matched = ran.filter(matches).length

  return (
    <>
      <WorkspaceTitle
        title="Security lab"
        subtitle="Each button sends one real request through the gateway. Expected is what Fetch’s baseline policy should do; Observed is what the gateway answered. This page decides nothing."
      >
        <div className="flex flex-wrap items-center gap-3 rounded-2xl bg-surface px-4 py-3 shadow-card">
          <span className="flex size-9 items-center justify-center rounded-full bg-brand-soft text-brand" aria-hidden="true">
            <Flask size={18} weight="bold" />
          </span>
          <div className="text-sm">
            <p className="font-bold text-ink">{user ? `Sending as ${user.name}` : 'Sending with no sign-in'}</p>
            <p className="text-ink-3">{user ? `${user.id} · ${user.roles.map((r) => roleNames[r]).join(', ')}` : 'Token attacks need a signed-in person'}</p>
          </div>
          <ButtonLink to="/signin?next=%2Flab" size="sm" variant="secondary">
            {user ? 'Switch person' : 'Sign in'}
          </ButtonLink>
        </div>
      </WorkspaceTitle>

      {ran.length > 0 && (
        <p className="mt-6 text-sm text-ink-2" role="status">
          {matched} of {ran.length} as expected.{' '}
          <button type="button" className="font-semibold text-brand hover:underline" onClick={() => setResults({})}>
            Clear results
          </button>
        </p>
      )}

      <div className="mt-6 flex flex-col gap-10">
        {groups.map((group) => (
          <section key={group.id} aria-labelledby={`lab-${group.id}`}>
            <h2 id={`lab-${group.id}`} className="flex items-center gap-2 text-lg font-extrabold tracking-[-0.015em]">
              <span className="text-brand" aria-hidden="true">
                {group.icon}
              </span>
              {group.title}
            </h2>
            <ul className="mt-3 grid grid-cols-1 gap-3 lg:grid-cols-2">
              {group.attacks.map((attack) => (
                <AttackCard
                  key={attack.id}
                  attack={attack}
                  user={user}
                  result={results[attack.id]}
                  busy={busy === attack.id}
                  disabled={busy !== null || (attack.token && !session)}
                  onFire={() => void fire(attack)}
                />
              ))}
            </ul>
          </section>
        ))}
      </div>

      <p className="mt-10 max-w-[70ch] text-sm text-ink-3">
        Address-based flooding (public search and sign-in) runs from a local terminal with <code className="font-mono text-ink-2">demo attack -from 127.0.0.50</code>, so this
        browser’s address stays usable. Copy any Ref into the Dashboard’s request lookup to see which check decided it.{' '}
        <Link to="/" className="font-semibold text-brand hover:underline">
          Back to Fetch
        </Link>
      </p>
    </>
  )
}

function AttackCard(props: { attack: Attack; user: User | null; result?: Result; busy: boolean; disabled: boolean; onFire: () => void }) {
  const { attack, user, result } = props
  const expected = result?.expected ?? attack.expected(user)
  return (
    <li className="flex min-w-0 flex-col rounded-2xl bg-surface p-5 shadow-card">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h3 className="font-bold text-ink">{attack.title}</h3>
          <p className="mt-1 font-mono text-xs break-words text-ink-3">{attack.request}</p>
        </div>
        <Button size="sm" variant="secondary" onClick={props.onFire} disabled={props.disabled} busy={props.busy} busyLabel="Sending…">
          Send
        </Button>
      </div>
      <dl className="mt-4 grid grid-cols-1 gap-x-3 gap-y-1 text-sm sm:grid-cols-[6.5rem_minmax(0,1fr)] sm:gap-y-2">
        <dt className="text-ink-3">Expected</dt>
        <dd className="text-ink-2">
          <span className="font-semibold text-ink">{expectedWords(expected)}</span> · {expected.why}
        </dd>
        <dt className="text-ink-3">Observed</dt>
        <dd>
          {result ? (
            <Observed result={result} user={attack.id === 'forged-headers' ? user : null} />
          ) : (
            <span className="text-ink-3">{attack.token && !user ? 'Sign in to send this one' : 'Not sent yet'}</span>
          )}
        </dd>
      </dl>
    </li>
  )
}

function expectedWords(expected: Expected): string {
  return `${expected.by === 'gateway' ? 'Refused by the gateway' : 'Answered by the service'} · ${expected.status}`
}

// Observed reports the gateway's answer. owner, when given, is the person
// whose orders an allowed answer must contain and nobody else's.
function Observed({ result, user: owner }: { result: Result; user: User | null }) {
  const last = result.answers[result.answers.length - 1]
  if (result.unsent) return <span className="text-warn">Not sent: this tab’s token could not be read to build the request.</span>
  if (!last) return <span className="text-ink-3">Stopped: the person changed.</span>
  if (last.kind === 'no-answer') return <span className="text-warn">No answer ({last.reason === 'timeout' ? 'it took too long' : 'the connection failed'}). Nothing can be concluded.</span>
  if (last.kind === 'cancelled') return null
  const ok = matches(result)
  const by = observedBy(last)
  return (
    <div className="flex flex-col gap-2">
      <p className="flex flex-wrap items-center gap-2">
        <Badge tone={ok ? 'ok' : 'bad'}>
          {ok ? <CheckCircle size={14} weight="bold" /> : <WarningCircle size={14} weight="bold" />}
          {ok ? 'As expected' : 'Differs'}
        </Badge>
        <span className="font-semibold text-ink">
          {by === 'gateway' ? 'Answered by the gateway' : `Answered by ${answeredBy(last)}`} · {last.status}
          {'code' in last ? ` ${last.code}` : ''}
        </span>
      </p>
      {result.answers.length > 1 && <Tally answers={result.answers} />}
      {last.kind === 'ok' && owner && <OkDetail answer={last} user={owner} />}
      {'message' in last && last.message && <p className="text-ink-2">{last.message}</p>}
      {last.ref && (
        <p>
          <RefTag value={last.ref} />
        </p>
      )}
    </div>
  )
}

// Tally sums a burst: how many answers had each status, in order of first
// appearance.
function Tally({ answers }: { answers: Answer<unknown>[] }) {
  const counts = new Map<string, number>()
  for (const answer of answers) {
    const key = 'status' in answer ? String(answer.status) : 'no answer'
    counts.set(key, (counts.get(key) ?? 0) + 1)
  }
  return (
    <p className="flex flex-wrap gap-1.5" aria-label="Answers in order">
      {answers.map((answer, n) => (
        <span
          key={n}
          className={cx(
            'rounded px-1.5 py-0.5 font-mono text-[11px] font-bold tabular-nums',
            'status' in answer && answer.status === 429 ? 'bg-bad-soft text-bad' : 'bg-sunken text-ink-2 ring-1 ring-line',
          )}
        >
          {'status' in answer ? answer.status : '—'}
        </span>
      ))}
      <span className="sr-only">{[...counts].map(([status, count]) => `${count} × ${status}`).join(', ')}</span>
    </p>
  )
}

// OkDetail shows what an allowed request proves: for the forged headers,
// whether every order returned belongs to the signed-in customer.
function OkDetail({ answer, user }: { answer: Extract<Answer<unknown>, { kind: 'ok' }>; user: User }) {
  const data = answer.data as Partial<List<Order>> | null
  if (!data || !Array.isArray(data.items)) return null
  const items = data.items as Order[]
  if (items.length === 0) return <p className="text-ink-2">No orders came back.</p>
  if (!items.every((o) => typeof o.customer === 'string')) return <p className="text-ink-2">{items.length} records came back.</p>
  const own = items.filter((o) => o.customer === user.id).length
  return (
    <p className={cx('flex items-center gap-1.5', own === items.length ? 'text-ok' : 'text-ink-2')}>
      <ShieldCheck size={16} weight="bold" aria-hidden="true" />
      {own === items.length ? `All ${items.length} orders are ${user.id}’s own.` : `${own} of ${items.length} orders are ${user.id}’s own.`}
    </p>
  )
}
