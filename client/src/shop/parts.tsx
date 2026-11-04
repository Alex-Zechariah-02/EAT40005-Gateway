import { useState } from 'react'
import { Check, Clock, Moped, Plus } from '@phosphor-icons/react'
import { addToBasket, basketSubtotal, setQuantity, maxQuantity, useBasket } from '../lib/basket'
import { minutes, rm } from '../lib/format'
import { useQuery } from '../lib/query'
import { Link } from '../lib/router'
import { hasRole, useSession } from '../lib/session'
import type { Category, Item, Merchant, Quote } from '../lib/types'
import { StockBadge } from '../ui/badge'
import { ButtonLink } from '../ui/button'
import { cx } from '../ui/cx'
import { ConfirmDialog } from '../ui/dialog'
import { Photo } from '../ui/photo'
import { QuantityStepper } from '../ui/stepper'
import { itemLink, storeLink } from '../lib/links'

export function categoryName(categories: Category[] | undefined, id: string): string {
  return categories?.find((c) => c.id === id)?.name ?? id.replace(/-/g, ' ')
}

export function ShopCard({ merchant, categories }: { merchant: Merchant; categories?: Category[] }) {
  return (
    <Link to={storeLink(merchant.name)} className="group block rounded-2xl">
      <div className="relative overflow-hidden rounded-2xl">
        <Photo id={merchant.id} section={merchant.section} className="aspect-[4/3] w-full transition-transform duration-500 ease-out-quint group-hover:scale-[1.03]" />
        {!merchant.open && <span className="absolute top-3 left-3 rounded-full bg-surface px-2.5 py-1 text-xs font-bold text-ink shadow-card">Closed now</span>}
      </div>
      <h3 className="mt-3 text-[17px] font-bold tracking-[-0.01em] text-ink group-hover:text-brand-strong">{merchant.name}</h3>
      <p className="mt-0.5 text-sm text-ink-2">{categoryName(categories, merchant.category)} · {merchant.area.split(',')[0]}</p>
      <p className="mt-2 flex items-center gap-4 text-[13px] font-medium text-ink-2 tabular-nums">
        <span className="flex items-center gap-1.5"><Clock size={15} className="text-ink-3" aria-hidden="true" /><span className="sr-only">Delivery time</span>{minutes(merchant)}</span>
        <span className="flex items-center gap-1.5"><Moped size={15} className="text-ink-3" aria-hidden="true" /><span className="sr-only">Delivery fee</span>{rm(merchant.delivery_fee_sen)}</span>
      </p>
    </Link>
  )
}

export function ShopGrid({ merchants, categories }: { merchants: Merchant[]; categories?: Category[] }) {
  return (
    <div className="grid grid-cols-2 gap-x-4 gap-y-7 sm:gap-x-6 sm:gap-y-8 lg:grid-cols-4">
      {merchants.map((m) => <ShopCard key={m.id} merchant={m} categories={categories} />)}
    </div>
  )
}

export function unavailableReason(item: Item, merchant: Pick<Merchant, 'open'>): string | null {
  if (!merchant.open) return 'Shop closed'
  if (!item.available) return 'Not available today'
  if (item.stock_state === 'out') return 'Out of stock'
  return null
}

// useAdd adds an item to the basket, asking first when the basket holds
// another shop's items (one order per shop).
export function useAdd(merchant: Pick<Merchant, 'id' | 'name'>) {
  const [pending, setPending] = useState<{ item: Item; quantity: number; other: string } | null>(null)
  const [added, setAdded] = useState<string | null>(null)
  const basket = useBasket()
  const flash = (item: Item) => {
    setAdded(item.id)
    setTimeout(() => setAdded((current) => (current === item.id ? null : current)), 1400)
  }
  const add = (item: Item, quantity = 1) => {
    const result = addToBasket(merchant, item, quantity)
    if (result === 'other-shop') setPending({ item, quantity, other: basket?.merchant.name ?? 'another shop' })
    else if (result === 'added') flash(item)
  }
  const dialog = (
    <ConfirmDialog
      open={!!pending}
      onOpenChange={(open) => !open && setPending(null)}
      title="Start a new basket?"
      body={pending && <>Your basket has items from <strong className="text-ink">{pending.other}</strong>. A Fetch order comes from one shop, so adding this empties your current basket.</>}
      confirmLabel="Start a new basket"
      cancelLabel="Keep my basket"
      onConfirm={() => {
        if (pending) {
          addToBasket(merchant, pending.item, pending.quantity, true)
          flash(pending.item)
        }
        setPending(null)
      }}
    />
  )
  return { add, added, dialog }
}

export function ItemRow({ item, merchant, onAdd, justAdded }: { item: Item; merchant: Merchant; onAdd: (item: Item) => void; justAdded: boolean }) {
  const reason = unavailableReason(item, merchant)
  const muted = !!reason
  return (
    <article className="flex items-center gap-5 rounded-2xl bg-surface p-4 shadow-card">
      <div className="min-w-0 flex-1">
        <h3 className={cx('text-base font-bold', muted ? 'text-ink-3' : 'text-ink')}>
          <Link to={itemLink(merchant.name, item.name)} className="rounded hover:underline hover:underline-offset-2">{item.name}</Link>
        </h3>
        <p className="mt-1 text-sm leading-snug text-ink-2">{item.description}</p>
        <p className="mt-3 flex flex-wrap items-center gap-2">
          <span className={cx('text-base font-bold whitespace-nowrap tabular-nums', muted ? 'text-ink-3' : 'text-ink')}>{rm(item.price_sen)}</span>
          {reason && reason !== 'Out of stock' ? <span className="text-xs font-bold text-ink-3">{reason}</span> : <StockBadge state={item.stock_state} />}
        </p>
      </div>
      <div className="relative shrink-0">
        <Photo id={item.id} section={merchant.section} dim={muted} className="size-24 rounded-xl" />
        <button
          type="button"
          disabled={muted}
          onClick={() => onAdd(item)}
          aria-label={muted ? `${item.name}: ${reason}` : `Add ${item.name} to basket`}
          className="absolute -right-1.5 -bottom-1.5 flex size-10 items-center justify-center rounded-full bg-brand text-on-brand shadow-press transition-[background-color,scale] duration-150 hover:bg-brand-strong active:scale-95 disabled:bg-line-strong disabled:text-ink-3 disabled:shadow-none"
        >
          {justAdded ? <Check size={18} weight="bold" /> : <Plus size={18} weight="bold" />}
        </button>
      </div>
    </article>
  )
}

// useQuote reads this customer's delivery fee for a shop (the first Fetch
// Mart order is free). Anyone else sees the shop's standard fee.
export function useQuote(merchantId: string | null) {
  const { session } = useSession()
  const customer = hasRole(session?.user, 'CUSTOMER')
  const quote = useQuery<Quote>(customer && merchantId ? `/orders/quote?merchant=${encodeURIComponent(merchantId)}` : null)
  return { customer, quote: quote.data, answer: quote.answer }
}

export function BasketPanel({ merchant }: { merchant?: Merchant }) {
  const basket = useBasket()
  const { quote, customer } = useQuote(basket?.merchant.id ?? null)
  const fee = quote ? quote.delivery_fee_sen : merchant && basket?.merchant.id === merchant.id ? merchant.delivery_fee_sen : null
  const subtotal = basketSubtotal(basket)
  return (
    <aside aria-label="Your basket" className="rounded-[22px] bg-surface p-6 shadow-raised">
      <h2 className="text-lg font-extrabold tracking-[-0.02em]">Your basket</h2>
      {!basket ? (
        <p className="mt-2 text-sm text-ink-2">Add something from {merchant ? merchant.name : 'a shop'} and it will show up here.</p>
      ) : (
        <>
          <p className="text-[13px] text-ink-3">From {basket.merchant.name}</p>
          <ul className="mt-5 flex flex-col gap-4">
            {basket.lines.map((line) => (
              <li key={line.item} className="flex items-center gap-3">
                <Photo id={line.item} className="size-12 shrink-0 rounded-lg" />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-semibold">{line.name}</p>
                  <p className="text-[13px] text-ink-3 tabular-nums">{rm(line.price_sen * line.quantity)}</p>
                </div>
                <QuantityStepper size="sm" name={line.name} value={line.quantity} max={maxQuantity} onChange={(q) => setQuantity(line.item, q)} />
              </li>
            ))}
          </ul>
          <dl className="mt-6 flex flex-col gap-2 border-t border-line pt-5 text-sm tabular-nums">
            <div className="flex justify-between text-ink-2"><dt>Subtotal</dt><dd>{rm(subtotal)}</dd></div>
            <div className="flex flex-wrap justify-between text-ink-2">
              <dt>Delivery</dt>
              <dd>{fee === null ? <span className="text-ink-3">At checkout</span> : quote?.free_delivery ? <span className="font-semibold text-brand">Free</span> : rm(fee)}</dd>
              {quote?.free_delivery && <dd className="w-full text-xs text-ink-3">Free because this is your first Fetch Mart order.</dd>}
            </div>
            <div className="mt-2 flex justify-between text-base font-extrabold text-ink"><dt>Total</dt><dd>{rm(subtotal + (fee ?? 0))}</dd></div>
          </dl>
          <ButtonLink to="/checkout" block size="lg" className="mt-5">Go to checkout</ButtonLink>
          <p className="mt-3 text-center text-xs text-ink-3">{customer ? 'Pay with cash on delivery or your Fetch wallet' : 'You’ll sign in at checkout'}</p>
        </>
      )}
    </aside>
  )
}
