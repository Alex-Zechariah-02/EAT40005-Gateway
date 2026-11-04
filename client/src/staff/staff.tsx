import { useState, type ReactNode } from 'react'
import { outcomeOfWrite, type Answer } from '../lib/api'
import { rm, time, reference } from '../lib/format'
import { describe, type Problem, type Write } from '../lib/messages'
import { Link } from '../lib/router'
import { call } from '../lib/session'
import type { Order } from '../lib/types'
import { OrderBadge } from '../ui/badge'
import { cx } from '../ui/cx'
import { ProblemNotice } from '../ui/notice'

// Shared pieces for the partner workspaces (merchant, rider, support, operations).

export function WorkspaceTitle({ title, subtitle, children }: { title: string; subtitle?: ReactNode; children?: ReactNode }) {
  return (
    <div className="mt-8 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="text-[34px] leading-none font-extrabold tracking-[-0.035em]">{title}</h1>
        {subtitle && <p className="mt-2 text-ink-2">{subtitle}</p>}
      </div>
      {children}
    </div>
  )
}

// Tabs switch between views of a workspace. Each view has its own address,
// so they are links marked with aria-current, not ARIA tabs.
export function Tabs({ tabs, current, href }: { tabs: { id: string; label: string }[]; current: string; href: (id: string) => string }) {
  return (
    <nav aria-label="Views" className="mt-6">
      <ul className="inline-flex max-w-full flex-wrap rounded-xl bg-sunken p-1 ring-1 ring-line">
        {tabs.map((tab) => (
          <li key={tab.id}>
            <Link to={href(tab.id)} replace aria-current={current === tab.id ? 'page' : undefined} className={cx('block rounded-lg px-4 py-2 text-sm font-semibold', current === tab.id ? 'bg-surface text-ink shadow-card' : 'text-ink-2 hover:text-ink')}>
              {tab.label}
            </Link>
          </li>
        ))}
      </ul>
    </nav>
  )
}

// useAction runs one write at a time for a card and describes its outcome.
// It never resends: after an unknown outcome the person checks first.
export function useAction(onDone: () => void) {
  const [busy, setBusy] = useState<string | null>(null)
  const [problem, setProblem] = useState<{ key: string; problem: Problem } | null>(null)
  // done names the last action that succeeded, for a brief "Saved" message.
  const [done, setDone] = useState<string | null>(null)
  const run = async (key: string, path: string, write: Write, body?: unknown) => {
    if (busy) return
    setBusy(key)
    setProblem(null)
    setDone(null)
    const answer: Answer<unknown> = await call({ method: body === undefined ? 'POST' : 'PATCH', path, body })
    setBusy(null)
    if (answer.kind === 'cancelled') return
    if (outcomeOfWrite(answer) === 'done') {
      setDone(key)
      setTimeout(() => setDone((current) => (current === key ? null : current)), 2400)
    } else {
      setProblem({ key, problem: describe(answer, write)! })
    }
    onDone()
  }
  return { busy, problem, done, run }
}

// OrderCard shows one order as the person looking at it needs it: a shop
// sees what to cook, a rider sees where to collect, where to deliver and any
// cash to collect, support sees both ends.
export function OrderCard({ order, action, problem, view, wide = false }: { order: Order; action?: ReactNode; problem?: Problem | null; view: 'shop' | 'rider' | 'support'; wide?: boolean }) {
  const count = order.items.reduce((sum, line) => sum + line.quantity, 0)
  const address = `${order.address.line1}${order.address.line2 ? `, ${order.address.line2}` : ''}, ${order.address.postcode} ${order.address.city}`
  return (
    <article className={cx('flex flex-col gap-3 rounded-2xl bg-surface p-4 shadow-card', wide && 'sm:flex-row sm:items-start')} aria-label={`Order ${reference(order.id)}`}>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-extrabold tracking-[-0.01em]">Order {reference(order.id)}</span>
          <OrderBadge status={order.status} />
          <span className="text-xs text-ink-3 tabular-nums">{time(order.created_at)}</span>
        </div>
        {view === 'rider' ? (
          <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
            <dt className="text-ink-3">Collect from</dt><dd className="font-semibold">{order.merchant_name}</dd>
            <dt className="text-ink-3">Deliver to</dt><dd>{address}</dd>
            <dt className="text-ink-3">Payment</dt>
            <dd className={order.payment === 'cash' ? 'font-bold text-ink' : 'text-ink-2'}>{order.payment === 'cash' ? `Collect ${rm(order.total_sen)} in cash` : 'Already paid by wallet'}</dd>
          </dl>
        ) : (
          <>
            {view === 'support' && <p className="mt-1 text-sm text-ink-2">{order.merchant_name} → {order.customer}</p>}
            <ul className="mt-2 flex flex-col gap-0.5 text-sm">
              {order.items.map((line) => <li key={line.item}><span className="font-bold tabular-nums">{line.quantity} ×</span> {line.name}</li>)}
            </ul>
          </>
        )}
        {order.note && <p className="mt-2 rounded-lg bg-warn-soft px-2.5 py-1.5 text-sm text-ink">Note: {order.note}</p>}
        <p className="mt-2 text-xs text-ink-3">
          {count === 1 ? '1 item' : `${count} items`}{view !== 'rider' && ` · ${rm(order.total_sen)} · ${order.payment === 'cash' ? 'cash on delivery' : 'paid by wallet'}`}
        </p>
        {view === 'support' && <p className="mt-0.5 text-xs text-ink-3">{address}</p>}
      </div>
      {(action || problem) && (
        <div className={cx('flex flex-col gap-2', wide && 'sm:w-64')}>
          {action}
          {problem && <ProblemNotice problem={problem} />}
        </div>
      )}
    </article>
  )
}

export function Column({ title, hint, count, children }: { title: string; hint: string; count: number; children: ReactNode }) {
  return (
    <section aria-label={title} className="flex min-w-0 flex-col gap-3 rounded-3xl bg-sunken p-3">
      <div className="px-2 pt-1">
        <h2 className="flex items-center gap-2 font-extrabold">{title}<span className="rounded-full bg-surface px-2 text-sm text-ink-2 tabular-nums">{count}</span></h2>
        <p className="text-xs text-ink-3">{hint}</p>
      </div>
      {children}
    </section>
  )
}
