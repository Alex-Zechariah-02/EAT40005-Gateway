import { useState, type FormEvent } from 'react'
import { ChartBar, CookingPot, Storefront } from '@phosphor-icons/react'
import { day, rm } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { useLocation } from '../lib/router'
import { useSession } from '../lib/session'
import type { Item, List, MerchantPage as MerchantAnswer, MerchantReport, Order, Stock } from '../lib/types'
import { StockBadge } from '../ui/badge'
import { Button } from '../ui/button'
import { BarChart } from '../ui/chart'
import { cx } from '../ui/cx'
import { Empty, Loading, ProblemNotice, RefreshProblem, Skeleton } from '../ui/notice'
import { Photo } from '../ui/photo'
import { Column, OrderCard, Tabs, WorkspaceTitle, useAction } from './staff'

type View = 'orders' | 'menu' | 'sales'

export function MerchantPage() {
  const { session } = useSession()
  const { params } = useLocation()
  const view = (['orders', 'menu', 'sales'].includes(params.get('view') ?? '') ? params.get('view') : 'orders') as View
  const shopId = session!.user.merchant
  const shop = useQuery<MerchantAnswer>(shopId ? `/inventory/merchants/${encodeURIComponent(shopId)}` : null)
  return (
    <>
      <WorkspaceTitle title={shop.data?.merchant.name ?? 'My shop'} subtitle={shop.data ? `${shop.data.merchant.area} · ${shop.data.merchant.open ? 'Open for orders' : 'Closed'}` : 'Your orders, menu and sales'} />
      <Tabs tabs={[{ id: 'orders', label: 'Orders' }, { id: 'menu', label: 'Menu and stock' }, { id: 'sales', label: 'Sales' }]} current={view} href={(id) => `/merchant${id === 'orders' ? '' : `?view=${id}`}`} />
      <div className="mt-6">
        {!shopId ? (
          <Empty icon={<Storefront size={26} weight="bold" />} title="No shop linked to this account" body="Fetch Accounts hasn’t linked a shop to you yet." />
        ) : view === 'orders' ? (
          <OrdersBoard />
        ) : view === 'menu' ? (
          <MenuEditor query={shop} />
        ) : (
          <Sales />
        )}
      </div>
    </>
  )
}

function OrdersBoard() {
  const orders = useQuery<List<Order>>('/orders/merchant', { poll: 5_000 })
  const action = useAction(orders.reload)
  const problem = orders.answer && !orders.data ? describe(orders.answer) : null
  if (problem) return <ProblemNotice problem={problem} action={<Button size="sm" variant="secondary" onClick={orders.reload}>Try again</Button>} />
  if (!orders.data) return <Loading label="Loading orders"><div className="grid grid-cols-1 gap-4 lg:grid-cols-3">{[0, 1, 2].map((n) => <Skeleton key={n} className="h-64 rounded-3xl" />)}</div></Loading>
  const by = (status: Order['status']) => orders.data!.items.filter((o) => o.status === status)
  const placed = by('placed')
  const accepted = by('accepted')
  const ready = by('ready')
  const cardProblem = (key: string) => (action.problem?.key === key ? action.problem.problem : null)
  return (
    <>
      <RefreshProblem query={orders} className="mb-4" />
      <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-3">
        <Column title="New" hint="Accept to start preparing." count={placed.length}>
          {placed.length === 0 && <p className="px-2 pb-2 text-sm text-ink-3">No new orders. New ones appear here within seconds.</p>}
          {placed.map((o) => (
            <OrderCard key={o.id} order={o} view="shop" problem={cardProblem(o.id)} action={<Button block busyLabel="Accepting…" busy={action.busy === o.id} onClick={() => void action.run(o.id, `/orders/accept/${encodeURIComponent(o.id)}`, { object: `accepting ${o.id}`, done: 'accepted', check: 'Wait for the board to refresh' })}>Accept order</Button>} />
          ))}
        </Column>
        <Column title="Being prepared" hint="Mark ready when it’s packed." count={accepted.length}>
          {accepted.length === 0 && <p className="px-2 pb-2 text-sm text-ink-3">Nothing in the kitchen.</p>}
          {accepted.map((o) => (
            <OrderCard key={o.id} order={o} view="shop" problem={cardProblem(o.id)} action={<Button block busyLabel="Updating…" busy={action.busy === o.id} onClick={() => void action.run(o.id, `/orders/ready/${encodeURIComponent(o.id)}`, { object: `marking ${o.id} ready`, done: 'marked ready', check: 'Wait for the board to refresh' })}>Mark ready for pickup</Button>} />
          ))}
        </Column>
        <Column title="Waiting for a rider" hint="A rider picks these up from the job board." count={ready.length}>
          {ready.length === 0 && <p className="px-2 pb-2 text-sm text-ink-3">Nothing waiting.</p>}
          {ready.map((o) => <OrderCard key={o.id} order={o} view="shop" />)}
        </Column>
      </div>
      <p className="mt-4 text-xs text-ink-3">Updates every few seconds.</p>
    </>
  )
}

function MenuEditor({ query }: { query: ReturnType<typeof useQuery<MerchantAnswer>> }) {
  const problem = query.answer && !query.data ? describe(query.answer) : null
  if (problem) return <ProblemNotice problem={problem} action={<Button size="sm" variant="secondary" onClick={query.reload}>Try again</Button>} />
  if (!query.data) return <Loading label="Loading your menu"><div className="flex flex-col gap-3">{[0, 1, 2].map((n) => <Skeleton key={n} className="h-24" />)}</div></Loading>
  const items = query.data.menu.flatMap((g) => g.items)
  if (items.length === 0) return <Empty icon={<CookingPot size={26} weight="bold" />} title="Your menu is empty" body="Items are added through Fetch’s merchant team for now." />
  return (
    <ul className="flex flex-col gap-3">
      {items.map((item) => <MenuRow key={item.id} item={item} section={query.data!.merchant.section} onChanged={query.reload} />)}
    </ul>
  )
}

function MenuRow({ item, section, onChanged }: { item: Item; section: 'food' | 'mart'; onChanged: () => void }) {
  const stocked = item.stock_state !== 'made_to_order'
  const stock = useQuery<Stock>(stocked ? `/inventory/stock/${encodeURIComponent(item.id)}` : null)
  const action = useAction(() => {
    onChanged()
    stock.reload()
  })
  const [price, setPrice] = useState((item.price_sen / 100).toFixed(2))
  const [delta, setDelta] = useState('')
  const priceSen = Math.round(Number(price) * 100)
  const priceValid = /^\d{1,4}(\.\d{1,2})?$/.test(price) && priceSen >= 50 && priceSen <= 100000
  const deltaValue = Number(delta)
  const deltaValid = /^-?\d{1,3}$/.test(delta) && deltaValue !== 0 && Math.abs(deltaValue) <= 100

  const savePrice = (event: FormEvent) => {
    event.preventDefault()
    if (!priceValid || priceSen === item.price_sen) return
    void action.run('price', `/inventory/items/${encodeURIComponent(item.id)}`, { object: 'the new price', done: 'saved', check: 'Refresh the menu' }, { price_sen: priceSen })
  }
  const adjust = (event: FormEvent) => {
    event.preventDefault()
    if (!deltaValid) return
    void action.run('stock', `/inventory/stock/${encodeURIComponent(item.id)}`, { object: 'the stock change', done: 'applied', check: 'Check the stock figure' }, { delta: deltaValue })
    setDelta('')
  }
  return (
    <li className="grid grid-cols-1 items-center gap-4 rounded-2xl bg-surface p-4 shadow-card md:grid-cols-[minmax(0,1.4fr)_auto_auto_auto]">
      <div className="flex min-w-0 items-center gap-3">
        <Photo id={item.id} section={section} dim={!item.available} className="size-14 shrink-0 rounded-xl" />
        <div className="min-w-0">
          <p className="truncate font-bold">{item.name}</p>
          <p className="flex items-center gap-2 text-xs text-ink-3">{item.group}<StockBadge state={item.stock_state} /></p>
        </div>
      </div>
      <form onSubmit={savePrice} className="flex items-center gap-2" aria-label={`Price of ${item.name}`}>
        <label className="flex h-10 items-center rounded-xl bg-sunken px-3 text-sm ring-1 ring-line focus-within:ring-2 focus-within:ring-brand">
          <span className="text-ink-3">RM</span>
          <input value={price} onChange={(e) => setPrice(e.target.value)} inputMode="decimal" aria-label="Price in ringgit" aria-invalid={!priceValid || undefined} className="w-16 bg-transparent pl-1.5 text-right font-semibold tabular-nums outline-none" />
        </label>
        <Button type="submit" size="sm" variant="secondary" busy={action.busy === 'price'} busyLabel="Saving" disabled={!priceValid || priceSen === item.price_sen}>Save</Button>
      </form>
      <label className="flex cursor-pointer items-center gap-2.5 text-sm font-semibold">
        <span className="relative inline-flex">
          <input type="checkbox" role="switch" checked={item.available} disabled={!!action.busy} onChange={(e) => void action.run('available', `/inventory/items/${encodeURIComponent(item.id)}`, { object: 'the availability change', done: 'saved', check: 'Refresh the menu' }, { available: e.target.checked })} className="peer sr-only" />
          <span className="h-6 w-11 rounded-full bg-line-strong transition-colors peer-checked:bg-brand peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-brand" aria-hidden="true" />
          <span className="absolute top-0.5 left-0.5 size-5 rounded-full bg-surface shadow-card transition-transform peer-checked:translate-x-5" aria-hidden="true" />
        </span>
        {item.available ? 'Available' : 'Unavailable'}
      </label>
      {stocked ? (
        <form onSubmit={adjust} className="flex items-center gap-2" aria-label={`Stock of ${item.name}`}>
          <span className="text-sm text-ink-2 tabular-nums">{stock.data ? <><span className="font-bold text-ink">{stock.data.stock}</span> in stock{stock.data.reserved ? ` · ${stock.data.reserved} reserved` : ''}</> : '…'}</span>
          <input value={delta} onChange={(e) => setDelta(e.target.value.replace(/[^\d-]/g, '').slice(0, 4))} inputMode="numeric" placeholder="+10" aria-label="Change stock by" className="h-10 w-16 rounded-xl bg-sunken px-2 text-right text-sm font-semibold tabular-nums ring-1 ring-line outline-none focus:ring-2 focus:ring-brand" />
          <Button type="submit" size="sm" variant="secondary" busy={action.busy === 'stock'} busyLabel="Saving" disabled={!deltaValid}>Adjust</Button>
        </form>
      ) : (
        <span className="text-sm text-ink-3">Made to order</span>
      )}
      <p className="text-xs font-semibold text-ok md:col-span-4 empty:hidden" role="status">
        {action.done === 'price' ? 'New price saved.' : action.done === 'available' ? (item.available ? 'Now available to order.' : 'Marked unavailable.') : action.done === 'stock' ? 'Stock updated.' : ''}
      </p>
      {action.problem && <ProblemNotice className="md:col-span-4" problem={action.problem.problem} />}
    </li>
  )
}

function Sales() {
  const report = useQuery<MerchantReport>('/reports/merchant')
  const problem = report.answer && !report.data ? describe(report.answer) : null
  if (problem) return <ProblemNotice problem={problem} action={<Button size="sm" variant="secondary" onClick={report.reload}>Try again</Button>} />
  if (!report.data) return <Loading label="Loading your sales"><Skeleton className="h-80 rounded-3xl" /></Loading>
  const r = report.data
  if (r.by_day.length === 0) return <Empty icon={<ChartBar size={26} weight="bold" />} title="No sales in this period" body="Delivered orders will show up here." />
  return (
    <section className="rounded-3xl bg-surface p-6 shadow-card" aria-labelledby="sales-heading">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 id="sales-heading" className="text-lg font-extrabold tracking-[-0.015em]">Revenue by day</h2>
          <p className="text-sm text-ink-3">{day(r.from)} to {day(r.to)}</p>
        </div>
        <dl className="flex gap-8 tabular-nums">
          <div><dt className="text-xs text-ink-3">Orders</dt><dd className="text-2xl font-extrabold">{r.orders.toLocaleString('en-MY')}</dd></div>
          <div><dt className="text-xs text-ink-3">Revenue</dt><dd className="text-2xl font-extrabold">{rm(r.revenue_sen)}</dd></div>
        </dl>
      </div>
      <div className={cx('mt-6')}>
        <BarChart
          label={`Revenue by day, ${day(r.from)} to ${day(r.to)}`}
          categories={r.by_day.map((d) => day(d.date).replace(/ \d{4}$/, ''))}
          series={[{ name: 'Revenue', values: r.by_day.map((d) => d.revenue_sen), fill: 'fill-chart-1', swatch: 'bg-chart-1' }]}
          format={(v) => rm(v).replace('.00', '')}
          every={Math.ceil(r.by_day.length / 10)}
        />
      </div>
    </section>
  )
}
