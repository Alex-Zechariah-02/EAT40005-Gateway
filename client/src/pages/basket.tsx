import { ShoppingBagOpen, Storefront } from '@phosphor-icons/react'
import { basketCount, basketSubtotal, emptyBasket, maxQuantity, setQuantity, useBasket } from '../lib/basket'
import { rm } from '../lib/format'
import { Link } from '../lib/router'
import { useSession } from '../lib/session'
import { Button, ButtonLink } from '../ui/button'
import { Empty } from '../ui/notice'
import { Photo } from '../ui/photo'
import { QuantityStepper } from '../ui/stepper'
import { useQuote } from './parts'
import { itemLink, storeLink } from '../lib/links'

export function BasketPage() {
  const basket = useBasket()
  const { session } = useSession()
  const { quote } = useQuote(basket?.merchant.id ?? null)
  if (!basket) {
    return (
      <Empty
        className="mt-10"
        icon={<ShoppingBagOpen size={26} weight="bold" />}
        title="Your basket is empty"
        body="Add dishes or groceries from one shop, then check out here."
        action={<><ButtonLink to="/food">Browse food</ButtonLink><ButtonLink to="/mart" variant="secondary">Browse Fetch Mart</ButtonLink></>}
      />
    )
  }
  const subtotal = basketSubtotal(basket)
  const fee = quote?.delivery_fee_sen ?? null
  return (
    <div className="mt-8 grid grid-cols-1 items-start gap-8 lg:grid-cols-[1fr_380px]">
      <section aria-labelledby="basket-heading">
        <div className="flex flex-wrap items-end justify-between gap-3">
          <div>
            <h1 id="basket-heading" className="text-[36px] leading-none font-extrabold tracking-[-0.035em]">Your basket</h1>
            <p className="mt-2 flex items-center gap-1.5 text-ink-2">
              <Storefront size={16} aria-hidden="true" />
              From <Link to={storeLink(basket.merchant.name)} className="font-semibold text-brand hover:underline">{basket.merchant.name}</Link>
            </p>
          </div>
          <Button variant="ghost" size="sm" onClick={emptyBasket}>Empty basket</Button>
        </div>
        <ul className="mt-6 flex flex-col gap-3">
          {basket.lines.map((line) => (
            <li key={line.item} className="flex flex-wrap items-center gap-x-4 gap-y-3 rounded-2xl bg-surface p-4 shadow-card">
              <Photo id={line.item} className="size-16 shrink-0 rounded-xl" />
              <div className="min-w-0 flex-1 basis-40">
                <Link to={itemLink(basket.merchant.name, line.name)} className="font-bold hover:underline">{line.name}</Link>
                <p className="text-sm text-ink-3 tabular-nums">{rm(line.price_sen)} each</p>
              </div>
              <QuantityStepper name={line.name} value={line.quantity} max={maxQuantity} onChange={(q) => setQuantity(line.item, q)} />
              <p className="ml-auto w-24 text-right font-bold tabular-nums">{rm(line.price_sen * line.quantity)}</p>
            </li>
          ))}
        </ul>
        <p className="mt-4 text-sm text-ink-3">Prices and stock are checked again when you place the order.</p>
      </section>
      <aside aria-label="Order total" className="rounded-[22px] bg-surface p-6 shadow-raised lg:sticky lg:top-24">
        <h2 className="text-lg font-extrabold tracking-[-0.02em]">Total</h2>
        <dl className="mt-4 flex flex-col gap-2 text-sm tabular-nums">
          <div className="flex justify-between text-ink-2"><dt>{basketCount(basket) === 1 ? '1 item' : `${basketCount(basket)} items`}</dt><dd>{rm(subtotal)}</dd></div>
          <div className="flex flex-wrap justify-between text-ink-2">
            <dt>Delivery</dt>
            <dd>{fee === null ? <span className="text-ink-3">{session ? '…' : 'After sign-in'}</span> : quote?.free_delivery ? <span className="font-semibold text-brand">Free</span> : rm(fee)}</dd>
            {quote?.free_delivery && <dd className="w-full text-xs text-ink-3">Free because this is your first Fetch Mart order.</dd>}
          </div>
          <div className="mt-2 flex justify-between border-t border-line pt-3 text-base font-extrabold text-ink"><dt>To pay</dt><dd>{rm(subtotal + (fee ?? 0))}</dd></div>
        </dl>
        <ButtonLink to="/checkout" block size="lg" className="mt-5">Go to checkout</ButtonLink>
        <p className="mt-3 text-center text-xs text-ink-3">Cash on delivery or your Fetch wallet. No card details needed.</p>
      </aside>
    </div>
  )
}
