import { CaretRight, Clock, Moped, ShoppingBagOpen, Storefront } from '@phosphor-icons/react'
import { basketCount, basketSubtotal, useBasket } from '../lib/basket'
import { minutes, rm } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link } from '../lib/router'
import type { Category, List, MerchantPage } from '../lib/types'
import { ButtonLink } from '../ui/button'
import { AnswerLine, Empty, Loading, ProblemNotice, Skeleton } from '../ui/notice'
import { Photo } from '../ui/photo'
import { BasketPanel, ItemRow, categoryName, useAdd, useQuote } from './parts'
import { itemIdIn, slugOf, storeLink, useMerchantId } from '../lib/links'
import { ItemPage } from './item'

const groupAnchor = (group: string) => `group-${slugOf(group)}`

// StoreRoute opens a shop by its readable address, or one of its items when
// the address names one (?item=).
export function StoreRoute({ address, item }: { address: string; item: string | null }) {
  const { id, list } = useMerchantId(address)
  const problem = list?.answer && !list.data ? describe(list.answer) : null
  if (problem) return <ProblemNotice className="mt-10" problem={problem} action={<button type="button" onClick={list!.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
  if (id === undefined) return <Loading label="Loading the shop"><Skeleton className="mt-10 h-72 w-full rounded-[28px]" /></Loading>
  if (id === null) return <MissingShop />
  if (item) return <StoreItem merchant={id} item={item} />
  return <StorePage id={id} />
}

function MissingShop() {
  return <Empty className="mt-10" icon={<Storefront size={26} weight="bold" />} title="This shop isn’t on Fetch" body="It may have closed or the link may be old." action={<ButtonLink to="/">Browse shops</ButtonLink>} />
}

function StoreItem({ merchant, item }: { merchant: string; item: string }) {
  const page = useQuery<MerchantPage>(`/inventory/merchants/${encodeURIComponent(merchant)}`)
  const problem = page.answer && !page.data ? describe(page.answer) : null
  if (problem) return problem.status === 404 ? <MissingShop /> : <ProblemNotice className="mt-10" problem={problem} action={<button type="button" onClick={page.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
  if (!page.data) return <Loading label="Loading the item"><Skeleton className="mt-10 aspect-square w-full max-w-xl rounded-[28px]" /></Loading>
  const id = itemIdIn(page.data, item)
  return id ? <ItemPage key={id} id={id} /> : <Empty className="mt-10" icon={<Storefront size={26} weight="bold" />} title="This item isn’t on the menu" body="It may have been removed or the link may be old." action={<ButtonLink to={storeLink(page.data.merchant.name)}>See the menu</ButtonLink>} />
}

export function StorePage({ id }: { id: string }) {
  const page = useQuery<MerchantPage>(`/inventory/merchants/${encodeURIComponent(id)}`)
  const categories = useQuery<List<Category>>('/inventory/categories')
  const problem = page.answer && !page.data ? describe(page.answer) : null

  if (problem) {
    return problem.status === 404 ? (
      <Empty className="mt-10" icon={<Storefront size={26} weight="bold" />} title="This shop isn’t on Fetch" body="It may have closed or the link may be old." action={<ButtonLink to="/">Browse shops</ButtonLink>} footer={<AnswerLine problem={problem} className="mt-5 justify-center" />} />
    ) : (
      <ProblemNotice className="mt-10" problem={problem} action={<button type="button" onClick={page.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
    )
  }
  if (!page.data) {
    return (
      <Loading label="Loading the shop">
        <Skeleton className="mt-10 h-72 w-full rounded-[28px]" />
        <div className="mt-8 grid gap-4 lg:w-[620px]">{[0, 1, 2].map((n) => <Skeleton key={n} className="h-32" />)}</div>
      </Loading>
    )
  }
  return <Store data={page.data} categories={categories.data?.items} />
}

function Store({ data, categories }: { data: MerchantPage; categories?: Category[] }) {
  const { merchant, menu } = data
  const { add, added, dialog } = useAdd(merchant)
  const { quote } = useQuote(merchant.id)
  const section = merchant.section === 'food' ? { to: '/food', label: 'Food' } : { to: '/mart', label: 'Mart' }
  return (
    <>
      <nav aria-label="Breadcrumb" className="mt-6">
        <ol className="flex items-center gap-1.5 text-[13px] font-medium text-ink-3">
          <li><Link to="/" className="hover:text-ink">Home</Link></li>
          <li aria-hidden="true"><CaretRight size={12} /></li>
          <li><Link to={section.to} className="hover:text-ink">{section.label}</Link></li>
          <li aria-hidden="true"><CaretRight size={12} /></li>
          <li aria-current="page" className="text-ink-2">{merchant.name}</li>
        </ol>
      </nav>
      <section className="mt-4 overflow-hidden rounded-[28px] bg-surface shadow-card">
        <Photo id={merchant.id} section={merchant.section} className="h-44 w-full sm:h-52" />
        <div className="flex flex-col gap-4 px-6 py-6 sm:px-8 lg:flex-row lg:items-end lg:justify-between">
          <div>
            <h1 className="text-[34px] leading-none font-extrabold tracking-[-0.035em] sm:text-[40px]">{merchant.name}</h1>
            <p className="mt-2 text-[15px] text-ink-2">{categoryName(categories, merchant.category)} · {merchant.area}</p>
          </div>
          <ul className="flex flex-wrap items-center gap-2 text-sm font-semibold tabular-nums" aria-label="Shop details">
            <li className={merchant.open ? 'rounded-full bg-ok-soft px-3 py-1.5 text-ok' : 'rounded-full bg-sunken px-3 py-1.5 text-ink-2'}>{merchant.open ? 'Open' : 'Closed now'}</li>
            <li className="flex items-center gap-1.5 rounded-full bg-sunken px-3 py-1.5 text-ink-2"><Clock size={15} aria-hidden="true" />{minutes(merchant)}</li>
            <li className="flex items-center gap-1.5 rounded-full bg-accent-soft px-3 py-1.5 text-ink">
              <Moped size={15} aria-hidden="true" />
              {quote ? (quote.free_delivery ? 'Free delivery · your first Mart order' : `Delivery ${rm(quote.delivery_fee_sen)}`) : `Delivery ${rm(merchant.delivery_fee_sen)}`}
            </li>
          </ul>
        </div>
      </section>

      <nav aria-label="Menu sections" className="sticky top-[72px] z-20 -mx-4 mt-6 overflow-x-auto bg-bg/95 px-4 py-2 backdrop-blur lg:hidden">
        <ul className="flex gap-2">
          {menu.map((g) => (
            <li key={g.group}><a href={`#${groupAnchor(g.group)}`} className="block rounded-full bg-surface px-4 py-2 text-sm font-semibold whitespace-nowrap text-ink-2 ring-1 ring-line">{g.group}</a></li>
          ))}
        </ul>
      </nav>

      <div className="mt-6 grid grid-cols-1 items-start gap-8 lg:mt-8 lg:grid-cols-[200px_1fr_360px]">
        <nav aria-label="Menu sections" className="sticky top-24 hidden flex-col text-sm font-semibold lg:flex">
          {menu.map((g) => (
            <a key={g.group} href={`#${groupAnchor(g.group)}`} className="flex justify-between rounded-lg px-3 py-2.5 text-ink-2 hover:bg-sunken hover:text-ink">
              {g.group}<span className="tabular-nums opacity-60">{g.items.length}</span>
            </a>
          ))}
        </nav>
        <div className="flex min-w-0 flex-col gap-10">
          {menu.length === 0 && <Empty icon={<Storefront size={26} weight="bold" />} title="Nothing on the menu yet" body={`${merchant.name} hasn’t added anything to Fetch yet.`} />}
          {menu.map((g) => (
            <section key={g.group} id={groupAnchor(g.group)} className="scroll-mt-36 lg:scroll-mt-24" aria-labelledby={`${groupAnchor(g.group)}-heading`}>
              <h2 id={`${groupAnchor(g.group)}-heading`} className="text-xl font-extrabold tracking-[-0.02em]">{g.group}</h2>
              <div className="mt-4 flex flex-col gap-3">
                {g.items.map((item) => <ItemRow key={item.id} item={item} merchant={merchant} onAdd={(it) => add(it)} justAdded={added === item.id} />)}
              </div>
            </section>
          ))}
        </div>
        <div className="sticky top-24 hidden lg:block">
          <BasketPanel merchant={merchant} />
        </div>
      </div>
      <MobileBasketBar />
      {dialog}
    </>
  )
}

function MobileBasketBar() {
  const basket = useBasket()
  if (!basket) return null
  const count = basketCount(basket)
  return (
    <div className="fixed inset-x-0 bottom-0 z-30 border-t border-line bg-surface/95 px-4 py-3 backdrop-blur lg:hidden">
      <Link to="/cart" className="flex h-12 items-center gap-3 rounded-xl bg-brand px-4 font-bold text-on-brand">
        <ShoppingBagOpen size={20} aria-hidden="true" />
        <span className="flex-1">View basket · {count === 1 ? '1 item' : `${count} items`}</span>
        <span className="tabular-nums">{rm(basketSubtotal(basket))}</span>
      </Link>
    </div>
  )
}
