import { useState } from 'react'
import { CaretRight, Check, Clock, ForkKnife, Storefront } from '@phosphor-icons/react'
import { maxQuantity } from '../lib/basket'
import { minutes, rm } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link } from '../lib/router'
import type { ItemPage as ItemAnswer } from '../lib/types'
import { StockBadge } from '../ui/badge'
import { Button, ButtonLink } from '../ui/button'
import { AnswerLine, Empty, Loading, ProblemNotice, Skeleton } from '../ui/notice'
import { Photo } from '../ui/photo'
import { QuantityStepper } from '../ui/stepper'
import { unavailableReason, useAdd } from './parts'
import { storeLink } from '../lib/links'

export function ItemPage({ id }: { id: string }) {
  const page = useQuery<ItemAnswer>(`/inventory/items/${encodeURIComponent(id)}`)
  const problem = page.answer && !page.data ? describe(page.answer) : null
  if (problem) {
    return problem.status === 404 ? (
      <Empty className="mt-10" icon={<ForkKnife size={26} weight="bold" />} title="This item isn’t on Fetch" body="It may have been taken off the menu." action={<ButtonLink to="/">Browse shops</ButtonLink>} footer={<AnswerLine problem={problem} className="mt-5 justify-center" />} />
    ) : (
      <ProblemNotice className="mt-10" problem={problem} action={<button type="button" onClick={page.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
    )
  }
  if (!page.data) {
    return (
      <Loading label="Loading the item">
        <div className="mt-10 grid grid-cols-1 gap-10 lg:grid-cols-2"><Skeleton className="aspect-square w-full rounded-[28px]" /><div className="flex flex-col gap-4"><Skeleton className="h-12 w-3/4" /><Skeleton className="h-20 w-full" /></div></div>
      </Loading>
    )
  }
  return <ItemView data={page.data} />
}

function ItemView({ data }: { data: ItemAnswer }) {
  const { item, merchant } = data
  const [quantity, setQuantity] = useState(1)
  const { add, added, dialog } = useAdd(merchant)
  const reason = unavailableReason(item, merchant)
  return (
    <>
      <nav aria-label="Breadcrumb" className="mt-6">
        <ol className="flex items-center gap-1.5 text-[13px] font-medium text-ink-3">
          <li><Link to="/" className="hover:text-ink">Home</Link></li>
          <li aria-hidden="true"><CaretRight size={12} /></li>
          <li><Link to={storeLink(merchant.name)} className="hover:text-ink">{merchant.name}</Link></li>
          <li aria-hidden="true"><CaretRight size={12} /></li>
          <li aria-current="page" className="text-ink-2">{item.name}</li>
        </ol>
      </nav>
      <div className="mt-6 grid grid-cols-1 items-start gap-8 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)] lg:gap-14">
        <Photo id={item.id} section={merchant.section} dim={!!reason} className="aspect-square w-full rounded-[28px]" />
        <div className="lg:pt-4">
          <p className="text-sm font-semibold text-ink-3">{item.group}</p>
          <h1 className="mt-1 text-[36px] leading-[1.05] font-extrabold tracking-[-0.03em] sm:text-[44px]">{item.name}</h1>
          <p className="mt-4 max-w-[52ch] text-[17px] text-ink-2">{item.description}</p>
          <p className="mt-6 flex items-center gap-3">
            <span className="text-[28px] font-extrabold tracking-[-0.02em] tabular-nums">{rm(item.price_sen)}</span>
            {reason && reason !== 'Out of stock' ? <span className="text-sm font-bold text-ink-3">{reason}</span> : <StockBadge state={item.stock_state} />}
          </p>
          {reason ? (
            <p className="mt-6 rounded-2xl bg-sunken p-4 text-sm text-ink-2">
              {reason === 'Out of stock' ? 'This has run out at the moment. ' : reason === 'Shop closed' ? `${merchant.name} is closed right now. ` : `${merchant.name} isn’t offering this today. `}
              <Link to={storeLink(merchant.name)} className="font-bold text-brand hover:underline">See the rest of the menu</Link>
            </p>
          ) : (
            <div className="mt-8 flex flex-wrap items-center gap-3">
              <QuantityStepper name={item.name} value={quantity} max={maxQuantity} onChange={(q) => setQuantity(Math.max(1, q))} />
              <Button size="lg" onClick={() => add(item, quantity)} className="w-full sm:w-auto sm:min-w-56">
                {added === item.id ? <><Check size={18} weight="bold" />Added to basket</> : `Add to basket · ${rm(item.price_sen * quantity)}`}
              </Button>
              <span className="sr-only" role="status">{added === item.id ? `${item.name} added to your basket` : ''}</span>
            </div>
          )}
          <Link to={storeLink(merchant.name)} className="mt-10 flex items-center gap-4 rounded-2xl bg-surface p-4 shadow-card hover:shadow-raised">
            <Photo id={merchant.id} section={merchant.section} className="size-14 shrink-0 rounded-xl" />
            <span className="min-w-0 flex-1">
              <span className="flex items-center gap-1.5 font-bold"><Storefront size={16} aria-hidden="true" />{merchant.name}</span>
              <span className="mt-0.5 flex items-center gap-1.5 text-sm text-ink-2"><Clock size={14} aria-hidden="true" />{minutes(merchant)} · delivery {rm(merchant.delivery_fee_sen)}</span>
            </span>
            <CaretRight size={18} className="text-ink-3" aria-hidden="true" />
          </Link>
        </div>
      </div>
      {dialog}
    </>
  )
}
