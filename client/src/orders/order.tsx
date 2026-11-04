import { useEffect, useRef, useState, type ReactNode } from 'react'
import { CheckCircle, ChefHat, Lifebuoy, MapPin, Moped, Package, Printer, Receipt, XCircle } from '@phosphor-icons/react'
import { outcomeOfWrite } from '../lib/api'
import { rm, statusNames, time, when, reference } from '../lib/format'
import { describe, type Problem } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link, useLocation } from '../lib/router'
import { call } from '../lib/session'
import type { Order, OrderStatus } from '../lib/types'
import { Button, ButtonLink } from '../ui/button'
import { cx } from '../ui/cx'
import { ConfirmDialog } from '../ui/dialog'
import { AnswerLine, Empty, Loading, ProblemNotice, RefreshProblem, Skeleton } from '../ui/notice'
import { Photo } from '../ui/photo'
import { storeLink } from '../lib/links'

const steps: { status: OrderStatus; label: string; icon: React.ReactNode }[] = [
  { status: 'placed', label: 'Order placed', icon: <Receipt size={18} weight="bold" /> },
  { status: 'accepted', label: 'Being prepared', icon: <ChefHat size={18} weight="bold" /> },
  { status: 'ready', label: 'Ready for pickup', icon: <Package size={18} weight="bold" /> },
  { status: 'picked_up', label: 'On the way', icon: <Moped size={18} weight="bold" /> },
  { status: 'delivered', label: 'Delivered', icon: <CheckCircle size={18} weight="bold" /> },
]

const final = (status: OrderStatus) => status === 'delivered' || status === 'cancelled'

export function OrderPage({ id }: { id: string }) {
  const { params } = useLocation()
  // Follow the order every 5 seconds until it is delivered or cancelled.
  const [settled, setSettled] = useState(false)
  const page = useQuery<{ order: Order }>(`/orders/detail/${encodeURIComponent(id)}`, { poll: settled ? undefined : 5_000 })
  if (!settled && page.data && final(page.data.order.status)) setSettled(true)
  const problem = page.answer && !page.data ? describe(page.answer) : null
  if (problem) {
    return problem.status === 404 ? (
      <Empty className="mt-10" icon={<Receipt size={26} weight="bold" />} title="We couldn’t find this order" body="It may belong to another account, or the link may be wrong." action={<ButtonLink to="/my-orders">See my orders</ButtonLink>} footer={<AnswerLine problem={problem} report className="mt-5 justify-center" />} />
    ) : (
      <ProblemNotice className="mt-10" problem={problem} report action={<button type="button" onClick={page.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
    )
  }
  if (!page.data) {
    return (
      <Loading label="Loading your order">
        <div className="mx-auto mt-8 flex max-w-[860px] flex-col gap-4"><Skeleton className="h-12 w-80 max-w-full" /><Skeleton className="h-40" /><Skeleton className="h-56" /></div>
      </Loading>
    )
  }
  return <OrderView order={page.data.order} justPlaced={params.get('placed') === '1'} onChanged={page.reload} refresh={<RefreshProblem query={page} report className="mt-4" />} />
}

function OrderView({ order, justPlaced, onChanged, refresh }: { order: Order; justPlaced: boolean; onChanged: () => void; refresh: ReactNode }) {
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<Problem | null>(null)
  const reached = new Map(order.timeline.map((t) => [t.status, t.at]))
  const cancelled = order.status === 'cancelled'
  // A status that changes while the page is open is announced politely; the
  // first status read is not, and focus never moves.
  const seen = useRef(order.status)
  const [update, setUpdate] = useState('')
  useEffect(() => {
    if (seen.current === order.status) return
    seen.current = order.status
    const at = reached.get(order.status)
    setUpdate(`Update: ${order.status === 'cancelled' ? 'Order cancelled' : statusNames[order.status]}${at ? ` at ${time(at)}` : ''}.`)
  }, [order.status])

  const cancel = async () => {
    setConfirming(false)
    setBusy(true)
    setProblem(null)
    const answer = await call<{ order: Order }>({ method: 'POST', path: `/orders/cancel/${encodeURIComponent(order.id)}` })
    setBusy(false)
    if (answer.kind === 'cancelled') return
    if (outcomeOfWrite(answer) !== 'done') setProblem(describe(answer, { object: 'cancelling this order', done: 'cancelled', check: 'Refresh this page' }))
    onChanged()
  }

  return (
    <div className="mx-auto mt-8 max-w-[860px]">
      {justPlaced && !cancelled && (
        <div role="status" className="mb-6 flex items-center gap-3 rounded-2xl bg-ok-soft p-4 text-ok print:hidden">
          <CheckCircle size={22} weight="fill" aria-hidden="true" />
          <p className="font-bold">Order placed. {order.merchant_name} has it now.</p>
        </div>
      )}
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <p className="text-sm font-semibold text-ink-3"><Link to="/my-orders" className="hover:text-ink">My orders</Link> · Order {reference(order.id)}</p>
          <h1 className="mt-1 text-[34px] leading-none font-extrabold tracking-[-0.035em]">{cancelled ? 'Order cancelled' : statusNames[order.status]}</h1>
          <p className="mt-2 text-ink-2">From <Link to={storeLink(order.merchant_name)} className="font-semibold text-brand hover:underline">{order.merchant_name}</Link> · placed {when(order.created_at)}</p>
        </div>
      </div>
      {refresh}
      <p role="status" className={cx('mt-4 rounded-2xl bg-brand-soft p-4 font-bold text-brand-strong print:hidden', !update && 'sr-only')}>
        {update}
      </p>
      <p className="hidden print:mt-2 print:block print:text-sm">Receipt · Order {reference(order.id)} · placed {when(order.created_at)}</p>

      {!cancelled ? (
        <ol className="mt-8 grid grid-cols-1 gap-3 sm:grid-cols-5 print:hidden" aria-label="Order progress">
          {steps.map((step, index) => {
            const at = reached.get(step.status)
            const current = order.status === step.status
            return (
              <li key={step.status} aria-current={current ? 'step' : undefined} className={cx('relative rounded-2xl p-4', at ? 'bg-surface shadow-card' : 'bg-sunken')}>
                <span className={cx('flex size-9 items-center justify-center rounded-full', current ? 'bg-brand text-on-brand' : at ? 'bg-brand-soft text-brand-strong' : 'bg-surface text-ink-3 ring-1 ring-line')} aria-hidden="true">{step.icon}</span>
                <p className={cx('mt-3 text-sm font-bold', at ? 'text-ink' : 'text-ink-3')}>{step.label}</p>
                <p className="text-xs text-ink-3 tabular-nums">{at ? time(at) : index === 0 ? '' : 'Not yet'}</p>
                <span className="sr-only">{at ? 'done' : 'not yet'}</span>
              </li>
            )
          })}
        </ol>
      ) : (
        <div className="mt-6 flex items-center gap-3 rounded-2xl bg-surface p-4 shadow-card">
          <XCircle size={22} className="text-bad" aria-hidden="true" />
          <p className="text-sm text-ink-2">Cancelled {reached.get('cancelled') ? `at ${time(reached.get('cancelled')!)}` : ''}. {order.payment === 'wallet' ? `${rm(order.total_sen)} was returned to your Fetch wallet.` : 'Nothing is owed.'}</p>
        </div>
      )}

      {problem && <ProblemNotice className="mt-6" problem={problem} report />}

      <div className="mt-8 grid grid-cols-1 items-start gap-6 lg:grid-cols-[1fr_320px]">
        <section className="rounded-3xl bg-surface p-6 shadow-card" aria-labelledby="items-heading">
          <h2 id="items-heading" className="text-lg font-extrabold tracking-[-0.015em]">Items</h2>
          <ul className="mt-4 flex flex-col gap-3">
            {order.items.map((line) => (
              <li key={line.item} className="flex items-center gap-3">
                <Photo id={line.item} className="size-12 shrink-0 rounded-lg" />
                <p className="min-w-0 flex-1 text-sm"><span className="font-bold tabular-nums">{line.quantity} ×</span> {line.name}</p>
                <p className="text-sm font-semibold tabular-nums">{rm(line.unit_price_sen * line.quantity)}</p>
              </li>
            ))}
          </ul>
          <dl className="mt-5 flex flex-col gap-1.5 border-t border-line pt-4 text-sm tabular-nums">
            <div className="flex justify-between text-ink-2"><dt>Subtotal</dt><dd>{rm(order.subtotal_sen)}</dd></div>
            <div className="flex justify-between text-ink-2"><dt>Delivery</dt><dd>{order.delivery_fee_sen === 0 ? 'Free' : rm(order.delivery_fee_sen)}</dd></div>
            <div className="mt-1 flex justify-between text-base font-extrabold"><dt>Total</dt><dd>{rm(order.total_sen)}</dd></div>
            <div className="mt-1 flex justify-between text-ink-3"><dt>Payment</dt><dd>{order.payment === 'cash' ? 'Cash on delivery' : 'Fetch wallet'}</dd></div>
          </dl>
        </section>
        <div className="flex flex-col gap-4">
          <Button variant="secondary" block onClick={() => window.print()} className="print:hidden">
            <Printer size={18} aria-hidden="true" />
            Print receipt
          </Button>
          <section className="rounded-3xl bg-surface p-6 shadow-card" aria-labelledby="address-heading">
            <h2 id="address-heading" className="flex items-center gap-2 text-lg font-extrabold tracking-[-0.015em]"><MapPin size={18} aria-hidden="true" />Delivering to</h2>
            <address className="mt-3 text-sm text-ink-2 not-italic">
              {order.address.line1}<br />
              {order.address.line2 && <>{order.address.line2}<br /></>}
              {order.address.postcode} {order.address.city}<br />
              {order.address.state}
            </address>
            {order.note && <p className="mt-3 rounded-xl bg-sunken p-3 text-sm text-ink-2">Note: {order.note}</p>}
          </section>
          {order.status === 'placed' && (
            <Button variant="danger" block onClick={() => setConfirming(true)} busy={busy} busyLabel="Cancelling…" className="print:hidden">Cancel order</Button>
          )}
          <ButtonLink to={`/help?order=${encodeURIComponent(order.id)}`} variant="secondary" block className="print:hidden"><Lifebuoy size={18} aria-hidden="true" />Get help with this order</ButtonLink>
        </div>
      </div>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Cancel this order?"
        body={<>The shop hasn’t started it yet. {order.payment === 'wallet' ? `${rm(order.total_sen)} goes back to your Fetch wallet.` : 'You won’t owe anything.'}</>}
        confirmLabel="Cancel order"
        cancelLabel="Keep my order"
        danger
        onConfirm={() => void cancel()}
      />
    </div>
  )
}
