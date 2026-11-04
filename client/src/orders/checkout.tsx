import { useState, type FormEvent } from 'react'
import { Money, PencilSimple, ShoppingBagOpen, Wallet } from '@phosphor-icons/react'
import { addressErrors, setAddress, useAddress } from '../lib/address'
import { newIdempotencyKey, outcomeOfWrite, type Answer } from '../lib/api'
import { basketSubtotal, emptyBasket, useBasket, type Basket } from '../lib/basket'
import { rm } from '../lib/format'
import { describe, type Problem } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link, navigate } from '../lib/router'
import { call } from '../lib/session'
import { states, type Address, type Order, type Payment } from '../lib/types'
import { Button, ButtonLink } from '../ui/button'
import { cx } from '../ui/cx'
import { SelectField, TextArea, TextField } from '../ui/field'
import { Empty, ProblemNotice } from '../ui/notice'
import { Photo } from '../ui/photo'
import { useQuote } from '../shop/parts'

const blank: Address = { line1: '', line2: '', postcode: '', city: 'Kuala Lumpur', state: 'Wilayah Persekutuan Kuala Lumpur' }
// defaultAddress fills the form in one step with a complete address the
// order schema accepts.
const defaultAddress: Address = { line1: '1 Jalan Ampang', line2: 'Level 10', postcode: '50450', city: 'Kuala Lumpur', state: 'Wilayah Persekutuan Kuala Lumpur' }
const noteLimit = 200

// One attempt at placing an order: its body and idempotency key are frozen
// once sent, so "Send the same order again" after an unknown outcome is safe
// (the Orders service applies one key once).
interface Attempt {
  key: string
  body: { merchant: string; items: { item: string; quantity: number }[]; address: Address; note: string; payment: Payment }
}

export function CheckoutPage() {
  const basket = useBasket()
  if (!basket) {
    return (
      <Empty className="mt-10" icon={<ShoppingBagOpen size={26} weight="bold" />} title="Nothing to check out yet" body="Your basket is empty. Add something from one shop first." action={<ButtonLink to="/">Browse shops</ButtonLink>} />
    )
  }
  return <Checkout basket={basket} />
}

function Checkout({ basket }: { basket: Basket }) {
  const saved = useAddress()
  const [address, setDraft] = useState<Address>(saved ?? blank)
  const [note, setNote] = useState('')
  const [payment, setPayment] = useState<Payment>('cash')
  const [tried, setTried] = useState(false)
  const [busy, setBusy] = useState(false)
  const [attempt, setAttempt] = useState<Attempt | null>(null)
  const [problem, setProblem] = useState<Problem | null>(null)
  const [unknown, setUnknown] = useState(false)
  const [badLine, setBadLine] = useState<string | null>(null)

  const { quote, answer: quoteAnswer } = useQuote(basket.merchant.id)
  const wallet = useQuery<{ balance_sen: number }>('/orders/wallet')
  const subtotal = basketSubtotal(basket)
  const fee = quote?.delivery_fee_sen ?? null
  const total = subtotal + (fee ?? 0)
  const balance = wallet.data?.balance_sen ?? null
  const walletShort = balance !== null && fee !== null && balance < total

  const errors = tried ? addressErrors(address) : {}
  const locked = busy || unknown
  const change = (field: keyof Address, value: string) => setDraft((d) => ({ ...d, [field]: value }))

  const send = async (current: Attempt) => {
    setBusy(true)
    setProblem(null)
    setBadLine(null)
    const answer: Answer<{ order: Order }> = await call({ method: 'POST', path: '/orders', body: current.body, idempotencyKey: current.key })
    setBusy(false)
    if (answer.kind === 'cancelled') return
    const outcome = outcomeOfWrite(answer)
    if (answer.kind === 'ok' && outcome === 'done') {
      setAddress(current.body.address)
      emptyBasket()
      navigate(`/my-orders/${encodeURIComponent(answer.data.order.id)}?placed=1`, { replace: true })
      return
    }
    setUnknown(outcome === 'unknown')
    setProblem(describe(answer, { object: 'your order', done: 'placed', check: 'Check My orders' }))
    if (answer.kind === 'refused' && answer.field) {
      const index = /^items\[(\d+)\]/.exec(answer.field)?.[1]
      if (index !== undefined) setBadLine(current.body.items[Number(index)]?.item ?? null)
    }
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    setTried(true)
    if (locked || Object.keys(addressErrors(address)).length) return
    const next: Attempt = {
      key: newIdempotencyKey(),
      body: {
        merchant: basket.merchant.id,
        items: basket.lines.map((line) => ({ item: line.item, quantity: line.quantity })),
        address: { ...address, line1: address.line1.trim(), line2: address.line2.trim(), city: address.city.trim() },
        note: note.trim(),
        payment,
      },
    }
    setAttempt(next)
    void send(next)
  }

  const startOver = () => {
    setUnknown(false)
    setProblem(null)
    setAttempt(null)
  }

  return (
    <form onSubmit={submit} noValidate className="mt-8 grid grid-cols-1 items-start gap-8 lg:grid-cols-[1fr_400px]" aria-labelledby="checkout-heading">
      <div className="flex min-w-0 flex-col gap-6">
        <div>
          <h1 id="checkout-heading" className="text-[36px] leading-none font-extrabold tracking-[-0.035em]">Checkout</h1>
          <p className="mt-2 text-ink-2">Your order from <span className="font-semibold text-ink">{basket.merchant.name}</span></p>
        </div>

        <fieldset disabled={locked} className="rounded-3xl bg-surface p-6 shadow-card">
          <legend className="sr-only">Delivery address</legend>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="text-lg font-extrabold tracking-[-0.015em]" aria-hidden="true">Deliver to</h2>
            <Button type="button" variant="secondary" size="sm" onClick={() => setDraft(defaultAddress)}>Use default address</Button>
          </div>
          <div className="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
            <TextField className="sm:col-span-2" label="Street address" autoComplete="address-line1" value={address.line1} onChange={(e) => change('line1', e.target.value)} error={errors.line1} maxLength={100} />
            <TextField className="sm:col-span-2" label="Unit, floor or building" optional autoComplete="address-line2" value={address.line2} onChange={(e) => change('line2', e.target.value)} error={errors.line2} maxLength={100} />
            <TextField label="Postcode" inputMode="numeric" autoComplete="postal-code" value={address.postcode} onChange={(e) => change('postcode', e.target.value.replace(/\D/g, '').slice(0, 5))} error={errors.postcode} hint="5 digits" className="sm:max-w-40" />
            <TextField label="Town or city" autoComplete="address-level2" value={address.city} onChange={(e) => change('city', e.target.value)} error={errors.city} maxLength={40} />
            <SelectField className="sm:col-span-2" label="State" autoComplete="address-level1" value={address.state} onChange={(e) => change('state', e.target.value)} error={errors.state}>
              {states.map((state) => <option key={state} value={state}>{state}</option>)}
            </SelectField>
          </div>
        </fieldset>

        <fieldset disabled={locked} className="rounded-3xl bg-surface p-6 shadow-card">
          <legend className="sr-only">Note for the shop</legend>
          <TextArea
            label="Note for the shop"
            optional
            value={note}
            onChange={(e) => setNote(e.target.value.slice(0, noteLimit))}
            hint={`${note.length} of ${noteLimit} characters. For example: less spicy, no ice.`}
            rows={3}
          />
        </fieldset>

        <fieldset disabled={locked} className="rounded-3xl bg-surface p-6 shadow-card">
          <legend className="sr-only">Payment</legend>
          <h2 className="text-lg font-extrabold tracking-[-0.015em]" aria-hidden="true">Payment</h2>
          <div className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
            <PaymentOption value="cash" current={payment} onChange={setPayment} icon={<Money size={22} />} title="Cash on delivery" detail="Pay the rider when your order arrives." />
            <PaymentOption
              value="wallet"
              current={payment}
              onChange={setPayment}
              icon={<Wallet size={22} />}
              title="Fetch wallet"
              detail={balance === null ? (wallet.answer && !wallet.data ? 'Balance unavailable right now.' : 'Reading your balance…') : walletShort ? `Balance ${rm(balance)}: not enough for this order.` : `Balance ${rm(balance)}. Charged when you place the order.`}
              disabled={walletShort || balance === null}
            />
          </div>
          <p className="mt-3 text-xs text-ink-3">Fetch never asks for card details.</p>
        </fieldset>
      </div>

      <aside aria-label="Order summary" className="rounded-[22px] bg-surface p-6 shadow-raised lg:sticky lg:top-24">
        <div className="flex items-baseline justify-between">
          <h2 className="text-lg font-extrabold tracking-[-0.02em]">Order summary</h2>
          {!locked && <Link to="/cart" className="flex items-center gap-1 text-sm font-semibold text-brand hover:underline"><PencilSimple size={14} aria-hidden="true" />Edit basket</Link>}
        </div>
        <ul className="mt-4 flex flex-col gap-3">
          {basket.lines.map((line) => (
            <li key={line.item} className={cx('flex items-center gap-3 rounded-xl', badLine === line.item && 'bg-bad-soft p-2 ring-1 ring-bad')}>
              <Photo id={line.item} className="size-11 shrink-0 rounded-lg" />
              <p className="min-w-0 flex-1 text-sm"><span className="font-bold tabular-nums">{line.quantity} ×</span> {line.name}</p>
              <p className="text-sm font-semibold tabular-nums">{rm(line.price_sen * line.quantity)}</p>
            </li>
          ))}
        </ul>
        <dl className="mt-5 flex flex-col gap-2 border-t border-line pt-4 text-sm tabular-nums">
          <div className="flex justify-between text-ink-2"><dt>Subtotal</dt><dd>{rm(subtotal)}</dd></div>
          <div className="flex flex-wrap justify-between text-ink-2">
            <dt>Delivery</dt>
            <dd>{fee === null ? (quoteAnswer && !quote ? 'Unavailable' : '…') : quote?.free_delivery ? <span className="font-semibold text-brand">Free</span> : rm(fee)}</dd>
            {quote?.free_delivery && <dd className="w-full text-xs text-ink-3">Free because this is your first Fetch Mart order.</dd>}
          </div>
          <div className="mt-2 flex justify-between text-base font-extrabold text-ink"><dt>Total</dt><dd>{rm(total)}</dd></div>
        </dl>

        {problem && (
          <ProblemNotice
            className="mt-5"
            problem={problem}
            report
            action={
              unknown && attempt ? (
                <>
                  <ButtonLink to="/my-orders" size="sm" variant="secondary">Check My orders</ButtonLink>
                  <Button size="sm" onClick={() => void send(attempt)} busy={busy} busyLabel="Sending…">Send the same order again</Button>
                </>
              ) : undefined
            }
          />
        )}
        {unknown ? (
          <p className="mt-4 text-xs text-ink-3">
            Sending the same order again is safe: Fetch places it at most once.{' '}
            <button type="button" onClick={startOver} className="font-semibold text-brand underline-offset-2 hover:underline">Change the order instead</button>{' '}
            (if the first one went through, this would place a second order).
          </p>
        ) : (
          <Button type="submit" size="lg" block className="mt-5" busy={busy} busyLabel="Placing your order…" disabled={fee === null}>
            Place order · {rm(total)}
          </Button>
        )}
        <p className="mt-3 text-center text-xs text-ink-3">{payment === 'cash' ? 'You’ll pay the rider in cash.' : 'Your Fetch wallet is charged now.'}</p>
      </aside>
    </form>
  )
}

function PaymentOption({ value, current, onChange, icon, title, detail, disabled = false }: { value: Payment; current: Payment; onChange: (p: Payment) => void; icon: React.ReactNode; title: string; detail: string; disabled?: boolean }) {
  const id = `payment-${value}`
  return (
    <label htmlFor={id} className={cx('flex cursor-pointer gap-3 rounded-2xl p-4 ring-1 transition-shadow', current === value ? 'bg-brand-soft ring-2 ring-brand' : 'ring-line hover:ring-line-strong', disabled && 'cursor-not-allowed opacity-60')}>
      <input id={id} type="radio" name="payment" value={value} checked={current === value} disabled={disabled} onChange={() => onChange(value)} className="mt-1 size-4 accent-[var(--brand)]" />
      <span className="flex flex-col">
        <span className="flex items-center gap-2 font-bold text-ink">{icon}{title}</span>
        <span className="mt-1 text-sm text-ink-2">{detail}</span>
      </span>
    </label>
  )
}
