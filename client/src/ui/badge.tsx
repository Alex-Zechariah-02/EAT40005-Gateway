import type { ReactNode } from 'react'
import { statusNames } from '../lib/format'
import type { OrderStatus, StockState } from '../lib/types'
import { cx } from './cx'

type Tone = 'neutral' | 'brand' | 'ok' | 'warn' | 'bad' | 'accent'

const tones: Record<Tone, string> = {
  neutral: 'bg-sunken text-ink-2 ring-1 ring-line',
  brand: 'bg-brand-soft text-brand-strong',
  ok: 'bg-ok-soft text-ok',
  warn: 'bg-warn-soft text-warn',
  bad: 'bg-bad-soft text-bad',
  accent: 'bg-accent-soft text-ink',
}

export function Badge({ tone = 'neutral', children, className }: { tone?: Tone; children: ReactNode; className?: string }) {
  return <span className={cx('inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs font-bold whitespace-nowrap', tones[tone], className)}>{children}</span>
}

// Stock states come from the API: "low" means 1 to 15 left. The app never
// shows exact counts or invents urgency.
export function StockBadge({ state }: { state: StockState }) {
  if (state === 'low') return <Badge tone="warn">Low stock</Badge>
  if (state === 'out') return <Badge>Out of stock</Badge>
  return null
}

const orderTones: Record<OrderStatus, Tone> = {
  placed: 'neutral',
  accepted: 'brand',
  ready: 'accent',
  picked_up: 'brand',
  delivered: 'ok',
  cancelled: 'bad',
}

export function OrderBadge({ status }: { status: OrderStatus }) {
  return <Badge tone={orderTones[status]}>{statusNames[status]}</Badge>
}
