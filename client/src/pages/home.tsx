import type { ReactNode } from 'react'
import { Basket, CaretRight, ForkKnife, MagnifyingGlass } from '@phosphor-icons/react'
import { rm } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link, useLocation } from '../lib/router'
import type { Category, List, Merchant, SearchHit } from '../lib/types'
import { StockBadge } from '../ui/badge'
import { ButtonLink } from '../ui/button'
import { Empty, Loading, ProblemNotice, Skeleton } from '../ui/notice'
import { Photo } from '../ui/photo'
import { ShopGrid } from './parts'

export function Home() {
  const { params } = useLocation()
  const q = params.get('q')?.trim() ?? ''
  return q ? <SearchResults q={q} /> : <Storefront />
}

function Storefront() {
  const categories = useQuery<List<Category>>('/inventory/categories')
  const food = useQuery<List<Merchant>>('/inventory/merchants?section=food')
  const mart = useQuery<List<Merchant>>('/inventory/merchants?section=mart')
  const foodCount = food.data?.items.length
  const martCount = mart.data?.items.length
  return (
    <>
      <section className="mt-8 grid gap-6 lg:grid-cols-[1fr_400px]">
        <div className="flex flex-col items-start justify-center rounded-[28px] bg-brand-soft px-6 py-9 sm:px-12 sm:py-11">
          <h1 className="max-w-[16ch] text-[40px] leading-[1.02] font-extrabold tracking-[-0.035em] text-ink sm:text-[52px]">What can we fetch for you?</h1>
          <nav aria-label="Sections" className="mt-8 inline-flex rounded-2xl bg-surface p-1.5 shadow-card">
            <SectionLink to="/food" icon={<ForkKnife size={22} weight="bold" />} label="Food" count={foodCount === undefined ? null : `${foodCount} restaurants`} />
            <SectionLink to="/mart" icon={<Basket size={22} weight="bold" />} label="Mart" count={martCount === undefined ? null : `${martCount} stores`} />
          </nav>
          {categories.data && (
            <ul className="mt-6 flex flex-wrap gap-2" aria-label="Food categories">
              {categories.data.items.filter((c) => c.section === 'food').map((c) => (
                <li key={c.id}>
                  <Link to={`/food?category=${c.id}`} className="block rounded-full bg-surface px-4 py-2 text-sm font-semibold text-ink-2 ring-1 ring-line hover:text-ink hover:ring-line-strong">{c.name}</Link>
                </li>
              ))}
            </ul>
          )}
        </div>
        <Link to="/mart" className="group relative flex flex-col overflow-hidden rounded-[28px] bg-accent">
          <div className="px-8 pt-8">
            <h2 className="text-[26px] leading-[1.1] font-extrabold tracking-[-0.025em] text-on-accent">Free delivery on your first Fetch Mart order</h2>
            <p className="mt-2 text-sm font-medium text-on-accent/75">Applied automatically at checkout.</p>
            <span className="mt-5 inline-flex items-center gap-1.5 rounded-full bg-on-accent px-4 py-2.5 text-sm font-bold text-accent transition-transform group-hover:translate-x-0.5">Shop Fetch Mart <CaretRight size={14} weight="bold" /></span>
          </div>
          <div className="relative mt-6 min-h-36 flex-1">
            <Photo id="MER-005" className="absolute inset-0 size-full" />
          </div>
        </Link>
      </section>

      <ShopSection title="Restaurants" to="/food" more="See all food" query={food} categories={categories.data?.items} />
      <ShopSection title="Fetch Mart" to="/mart" more="See all stores" query={mart} categories={categories.data?.items} />
    </>
  )
}

// The two sections are equal destinations, so neither looks selected.
function SectionLink({ to, icon, label, count }: { to: string; icon: ReactNode; label: string; count: string | null }) {
  return (
    <Link to={to} className="group flex items-center gap-3 rounded-xl px-5 py-3 text-ink transition-colors hover:bg-brand hover:text-on-brand sm:px-6 sm:py-3.5">
      <span className="flex size-10 items-center justify-center rounded-lg bg-brand-soft text-brand transition-colors group-hover:bg-transparent group-hover:text-on-brand" aria-hidden="true">{icon}</span>
      <span className="text-left">
        <span className="block text-[15px] font-bold">{label}</span>
        <span className="block min-h-4 text-xs text-ink-3 group-hover:text-on-brand/80">{count}</span>
      </span>
    </Link>
  )
}

function ShopSection({ title, to, more, query, categories }: { title: string; to: string; more: string; query: ReturnType<typeof useQuery<List<Merchant>>>; categories?: Category[] }) {
  const problem = query.answer && !query.data ? describe(query.answer) : null
  return (
    <section className="mt-14" aria-labelledby={`${to}-heading`}>
      <div className="flex items-baseline justify-between">
        <h2 id={`${to}-heading`} className="text-[26px] font-extrabold tracking-[-0.025em]">{title}</h2>
        <Link to={to} className="text-sm font-semibold text-brand hover:text-brand-strong">{more}</Link>
      </div>
      <div className="mt-5">
        {query.data ? (
          <ShopGrid merchants={query.data.items} categories={categories} />
        ) : problem ? (
          <ProblemNotice problem={problem} action={<button type="button" onClick={query.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
        ) : (
          <Loading label={`Loading ${title.toLowerCase()}`}>
            <div className="grid grid-cols-2 gap-4 sm:gap-6 lg:grid-cols-4">
              {[0, 1, 2, 3].map((n) => <div key={n}><Skeleton className="aspect-[4/3] w-full rounded-2xl" /><Skeleton className="mt-3 h-5 w-2/3" /><Skeleton className="mt-2 h-4 w-1/2" /></div>)}
            </div>
          </Loading>
        )}
      </div>
    </section>
  )
}

function SearchResults({ q }: { q: string }) {
  const tooShort = q.length < 2
  const results = useQuery<List<SearchHit>>(tooShort ? null : `/inventory/search?q=${encodeURIComponent(q)}`)
  const problem = results.answer && results.answer.kind !== 'ok' ? describe(results.answer) : null
  const hits = results.data?.items ?? []
  return (
    <section className="mt-8">
      <h1 className="text-[32px] font-extrabold tracking-[-0.03em]">Results for “{q}”</h1>
      {tooShort ? (
        <p className="mt-2 text-ink-2">Type at least two letters to search.</p>
      ) : problem ? (
        <ProblemNotice className="mt-6" problem={problem} />
      ) : !results.data ? (
        <Loading label="Searching">
          <div className="mt-6 grid gap-4 sm:grid-cols-2">{[0, 1, 2, 3].map((n) => <Skeleton key={n} className="h-28" />)}</div>
        </Loading>
      ) : hits.length === 0 ? (
        <Empty
          className="mt-6"
          icon={<MagnifyingGlass size={26} weight="bold" />}
          title="Nothing matches that yet"
          body="Try a dish, a shop name or an everyday item, like “roti”, “laksa” or “rice”."
          action={<><ButtonLink to="/food" variant="secondary">Browse food</ButtonLink><ButtonLink to="/mart" variant="secondary">Browse Mart</ButtonLink></>}
        />
      ) : (
        <>
          <p className="mt-1 text-ink-2" role="status">{hits.length === 1 ? 'One item' : `${hits.length} items`} from {new Set(hits.map((h) => h.merchant.id)).size === 1 ? 'one shop' : `${new Set(hits.map((h) => h.merchant.id)).size} shops`}</p>
          <ul className="mt-6 grid gap-4 sm:grid-cols-2">
            {hits.map(({ item, merchant }) => (
              <li key={item.id}>
                <Link to={`/item/${item.id}`} className="flex items-center gap-4 rounded-2xl bg-surface p-4 shadow-card hover:shadow-raised">
                  <Photo id={item.id} section={merchant.section} className="size-20 shrink-0 rounded-xl" />
                  <span className="min-w-0 flex-1">
                    <span className="block font-bold text-ink">{item.name}</span>
                    <span className="block text-sm text-ink-2">{merchant.name}</span>
                    <span className="mt-1.5 flex items-center gap-2"><span className="font-bold tabular-nums">{rm(item.price_sen)}</span><StockBadge state={item.stock_state} /></span>
                  </span>
                  <CaretRight size={18} className="shrink-0 text-ink-3" aria-hidden="true" />
                </Link>
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  )
}
