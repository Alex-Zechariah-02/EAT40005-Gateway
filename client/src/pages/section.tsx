import { Storefront } from '@phosphor-icons/react'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link, navigate, useLocation } from '../lib/router'
import type { Category, List, Merchant, Section } from '../lib/types'
import { ButtonLink } from '../ui/button'
import { cx } from '../ui/cx'
import { Empty, Loading, ProblemNotice, Skeleton } from '../ui/notice'
import { ShopGrid } from './parts'

const titles: Record<Section, { title: string; other: string; otherLabel: string }> = {
  food: { title: 'Food', other: '/mart', otherLabel: 'Fetch Mart' },
  mart: { title: 'Fetch Mart', other: '/food', otherLabel: 'Food' },
}

// The shop orders a person can choose. Recommended keeps the order Inventory
// sends; nothing is ranked by popularity or ratings, which Fetch does not have.
const shopSorts: { value: string; label: string; compare?: (a: Merchant, b: Merchant) => number }[] = [
  { value: '', label: 'Recommended' },
  { value: 'fastest', label: 'Fastest delivery', compare: (a, b) => a.delivery_minutes.min - b.delivery_minutes.min || a.delivery_minutes.max - b.delivery_minutes.max },
  { value: 'fee', label: 'Lowest delivery fee', compare: (a, b) => a.delivery_fee_sen - b.delivery_fee_sen },
]

// withParam returns the current address with one query parameter set or removed,
// so the choice survives a reload and can be shared as a link.
export function withParam(path: string, params: URLSearchParams, name: string, value: string): string {
  const next = new URLSearchParams(params)
  if (value) next.set(name, value)
  else next.delete(name)
  const query = next.toString()
  return query ? `${path}?${query}` : path
}

export function SectionPage({ section }: { section: Section }) {
  const { path, params } = useLocation()
  const category = params.get('category')
  const sort = shopSorts.find((s) => s.value === (params.get('sort') ?? '')) ?? shopSorts[0]
  const openOnly = params.get('open') === '1'
  const categories = useQuery<List<Category>>('/inventory/categories')
  const merchants = useQuery<List<Merchant>>(`/inventory/merchants?section=${section}${category ? `&category=${encodeURIComponent(category)}` : ''}`)
  const mine = categories.data?.items.filter((c) => c.section === section) ?? []
  const chosen = mine.find((c) => c.id === category)
  const problem = merchants.answer && !merchants.data ? describe(merchants.answer) : null
  const { title, other, otherLabel } = titles[section]
  const shops = merchants.data ? merchants.data.items.filter((m) => !openOnly || m.open) : []
  if (sort.compare) shops.sort(sort.compare)
  return (
    <>
      <div className="mt-8 flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-[40px] leading-none font-extrabold tracking-[-0.035em]">{chosen ? chosen.name : title}</h1>
          {merchants.data && (
            <p className="mt-2 text-ink-2" role="status">
              {shops.length === 1 ? 'One shop' : `${shops.length} shops`} {openOnly ? 'open now' : 'delivering'}{chosen ? ` · ${title}` : ''}
            </p>
          )}
        </div>
        <Link to={other} className="text-sm font-semibold text-brand hover:text-brand-strong">Switch to {otherLabel}</Link>
      </div>
      {mine.length > 0 && (
        <nav aria-label="Categories" className="mt-6">
          <ul className="flex flex-wrap gap-2">
            <li><Chip to={`/${section}`} active={!category}>All</Chip></li>
            {mine.map((c) => (
              <li key={c.id}><Chip to={`/${section}?category=${c.id}`} active={category === c.id}>{c.name}</Chip></li>
            ))}
          </ul>
        </nav>
      )}
      <div className="mt-6 flex flex-wrap items-center gap-4 text-sm">
        <label className="flex items-center gap-2 font-semibold text-ink-2">
          Sort by
          <select
            value={sort.value}
            onChange={(event) => navigate(withParam(path, params, 'sort', event.target.value), { replace: true })}
            className="rounded-lg bg-surface px-3 py-2 font-semibold text-ink ring-1 ring-line"
          >
            {shopSorts.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-2 font-semibold text-ink-2">
          <input type="checkbox" checked={openOnly} onChange={(event) => navigate(withParam(path, params, 'open', event.target.checked ? '1' : ''), { replace: true })} className="size-4 accent-brand" />
          Open now
        </label>
      </div>
      <div className="mt-6">
        {merchants.data ? (
          shops.length ? (
            <ShopGrid merchants={shops} categories={categories.data?.items} />
          ) : openOnly && merchants.data.items.length ? (
            <Empty
              icon={<Storefront size={26} weight="bold" />}
              title="No shop here is open right now"
              body="Shops open at different times. See every shop, including those taking orders later."
              action={<ButtonLink to={withParam(path, params, 'open', '')} variant="secondary">Show all shops</ButtonLink>}
            />
          ) : (
            <Empty
              icon={<Storefront size={26} weight="bold" />}
              title={`No shops in ${chosen?.name ?? 'this section'} yet`}
              body="Fetch adds new shops regularly. In the meantime, see everything else in this section."
              action={<ButtonLink to={`/${section}`} variant="secondary">See all of {title}</ButtonLink>}
            />
          )
        ) : problem ? (
          <ProblemNotice problem={problem} action={<button type="button" onClick={merchants.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
        ) : (
          <Loading label="Loading shops">
            <div className="grid grid-cols-1 gap-6 sm:grid-cols-2 lg:grid-cols-4">
              {[0, 1, 2, 3].map((n) => <div key={n}><Skeleton className="aspect-[4/3] w-full rounded-2xl" /><Skeleton className="mt-3 h-5 w-2/3" /></div>)}
            </div>
          </Loading>
        )}
      </div>
    </>
  )
}

function Chip({ to, active, children }: { to: string; active: boolean; children: React.ReactNode }) {
  return (
    <Link to={to} aria-current={active ? 'page' : undefined} className={cx('block rounded-full px-4 py-2 text-sm font-semibold', active ? 'bg-ink text-surface' : 'bg-surface text-ink-2 ring-1 ring-line hover:text-ink hover:ring-line-strong')}>
      {children}
    </Link>
  )
}
