import { Receipt } from '@phosphor-icons/react'
import { day, rm, statusNames, when, reference } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { navigate, useLocation } from '../lib/router'
import type { List, Order, OrderStatus, Sales, Summary, TopItems } from '../lib/types'
import { OrderBadge } from '../ui/badge'
import { Button } from '../ui/button'
import { BarChart } from '../ui/chart'
import { cx } from '../ui/cx'
import { Empty, Loading, ProblemNotice, RefreshProblem, Skeleton } from '../ui/notice'
import { Photo } from '../ui/photo'
import { Tabs, WorkspaceTitle } from './staff'

const monthName = new Intl.DateTimeFormat('en-MY', { month: 'long', year: 'numeric', timeZone: 'UTC' })

export function OpsPage() {
  const { params } = useLocation()
  const view = params.get('view') === 'orders' ? 'orders' : 'reports'
  return (
    <>
      <WorkspaceTitle title={view === 'reports' ? 'Platform reports' : 'All orders'} />
      <Tabs tabs={[{ id: 'reports', label: 'Reports' }, { id: 'orders', label: 'All orders' }]} current={view} href={(id) => (id === 'reports' ? '/ops' : '/ops?view=orders')} />
      <div className="mt-6">{view === 'reports' ? <Reports /> : <AllOrders />}</div>
    </>
  )
}

function Reports() {
  const summary = useQuery<Summary>('/reports/summary')
  const sales = useQuery<Sales>('/reports/sales')
  const top = useQuery<TopItems>('/reports/top-items?limit=6')
  return (
    <div className="flex flex-col gap-6">
      <Panel query={summary} label="Loading the summary" height="h-28">
        {(s) => (
          <section aria-label="Summary" className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Figure label="Revenue" value={rm(s.revenue_sen)} note={`${day(s.from)} to ${day(s.to)}`} lead />
            <Figure label="Orders" value={s.orders.toLocaleString('en-MY')} note={`Average ${rm(s.average_order_sen)}`} />
            <Figure label="Customers" value={s.customers.toLocaleString('en-MY')} note="Ordered at least once" />
            <Figure label="Shops" value={s.merchants.toLocaleString('en-MY')} note="Food and Mart" />
          </section>
        )}
      </Panel>
      <div className="grid grid-cols-1 items-start gap-6 xl:grid-cols-[1.6fr_1fr]">
        <Panel query={sales} label="Loading sales" height="h-80">
          {(s) => {
            const months = [...new Set(s.months.map((m) => m.month))].sort()
            const value = (month: string, section: 'food' | 'mart') => s.months.find((m) => m.month === month && m.section === section)?.revenue_sen ?? 0
            return (
              <section className="rounded-3xl bg-surface p-6 shadow-card" aria-labelledby="sales-heading">
                <h2 id="sales-heading" className="text-lg font-extrabold tracking-[-0.015em]">Revenue by month</h2>
                <p className="mb-4 text-sm text-ink-3">Food and Fetch Mart side by side</p>
                {months.length === 0 ? <p className="text-sm text-ink-3">No sales yet.</p> : (
                  <BarChart
                    label="Revenue by month for Food and Fetch Mart"
                    categories={months.map((m) => monthName.format(new Date(`${m}-01T00:00:00Z`)))}
                    series={[
                      { name: 'Food', values: months.map((m) => value(m, 'food')), fill: 'fill-chart-1', swatch: 'bg-chart-1' },
                      { name: 'Fetch Mart', values: months.map((m) => value(m, 'mart')), fill: 'fill-chart-2', swatch: 'bg-chart-2' },
                    ]}
                    format={(v) => rm(v).replace('.00', '')}
                  />
                )}
              </section>
            )
          }}
        </Panel>
        <Panel query={top} label="Loading top items" height="h-80">
          {(t) => (
            <section className="rounded-3xl bg-surface p-6 shadow-card" aria-labelledby="top-heading">
              <h2 id="top-heading" className="text-lg font-extrabold tracking-[-0.015em]">Top items</h2>
              <p className="text-sm text-ink-3">By quantity sold</p>
              <ol className="mt-4 flex flex-col gap-3">
                {t.items.map((item, index) => (
                  <li key={item.item} className="flex items-center gap-3">
                    <span className="w-5 text-right text-sm font-bold text-ink-3 tabular-nums">{index + 1}</span>
                    <Photo id={item.item} className="size-10 shrink-0 rounded-lg" />
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-bold">{item.name}</span>
                      <span className="block text-xs text-ink-3 tabular-nums">{item.quantity.toLocaleString('en-MY')} sold</span>
                    </span>
                    <span className="text-sm font-semibold tabular-nums">{rm(item.revenue_sen)}</span>
                  </li>
                ))}
              </ol>
            </section>
          )}
        </Panel>
      </div>
    </div>
  )
}

function Figure({ label, value, note, lead = false }: { label: string; value: string; note: string; lead?: boolean }) {
  return (
    <div className={cx('rounded-3xl p-5', lead ? 'bg-brand-soft' : 'bg-surface shadow-card')}>
      <p className="text-sm font-semibold text-ink-2">{label}</p>
      <p className="mt-1 text-[28px] leading-tight font-extrabold tracking-[-0.025em] tabular-nums">{value}</p>
      <p className="mt-1 text-xs text-ink-3">{note}</p>
    </div>
  )
}

function Panel<T>({ query, label, height, children }: { query: ReturnType<typeof useQuery<T>>; label: string; height: string; children: (data: T) => React.ReactNode }) {
  if (query.data) {
    return (
      <>
        <RefreshProblem query={query} className="mb-4" />
        {children(query.data)}
      </>
    )
  }
  const problem = query.answer ? describe(query.answer) : null
  if (problem) return <ProblemNotice problem={problem} action={<Button size="sm" variant="secondary" onClick={query.reload}>Try again</Button>} />
  return <Loading label={label}><Skeleton className={cx(height, 'rounded-3xl')} /></Loading>
}

const filters: (OrderStatus | 'all')[] = ['all', 'placed', 'accepted', 'ready', 'picked_up', 'delivered', 'cancelled']

function AllOrders() {
  const { params } = useLocation()
  const status = (filters.includes(params.get('status') as OrderStatus) ? params.get('status') : 'all') as OrderStatus | 'all'
  const orders = useQuery<List<Order>>(`/orders?limit=100${status === 'all' ? '' : `&status=${status}`}`, { poll: 15_000 })
  return (
    <>
      <div className="flex flex-wrap gap-2" role="group" aria-label="Filter by status">
        {filters.map((f) => (
          <button key={f} type="button" aria-pressed={status === f} onClick={() => navigate(`/ops?view=orders${f === 'all' ? '' : `&status=${f}`}`, { replace: true })} className={cx('rounded-full px-4 py-2 text-sm font-semibold', status === f ? 'bg-ink text-surface' : 'bg-surface text-ink-2 ring-1 ring-line hover:text-ink')}>
            {f === 'all' ? 'All' : statusNames[f]}
          </button>
        ))}
      </div>
      <div className="mt-4">
        <Panel query={orders} label="Loading orders" height="h-80">
          {(list) => list.items.length === 0 ? (
            <Empty icon={<Receipt size={26} weight="bold" />} title="No orders with this status" body="Try another filter." />
          ) : (
            <div className="overflow-x-auto rounded-3xl bg-surface shadow-card" role="region" aria-label="All orders, scrolls sideways" tabIndex={0}>
              <table className="w-full min-w-[760px] text-left text-sm tabular-nums">
                <thead className="border-b border-line text-xs text-ink-3">
                  <tr><th className="px-5 py-3 font-semibold">Order</th><th className="px-3 py-3 font-semibold">Placed</th><th className="px-3 py-3 font-semibold">Shop</th><th className="px-3 py-3 font-semibold">Customer</th><th className="px-3 py-3 font-semibold">Status</th><th className="px-3 py-3 font-semibold">Rider</th><th className="px-5 py-3 text-right font-semibold">Total</th></tr>
                </thead>
                <tbody className="divide-y divide-line">
                  {list.items.map((o) => (
                    <tr key={o.id} className="hover:bg-sunken/60">
                      <td className="px-5 py-3 font-bold">{reference(o.id)}</td>
                      <td className="px-3 py-3 text-ink-2">{when(o.created_at)}</td>
                      <td className="px-3 py-3">{o.merchant_name}</td>
                      <td className="px-3 py-3 text-ink-2">{o.customer}</td>
                      <td className="px-3 py-3"><OrderBadge status={o.status} /></td>
                      <td className="px-3 py-3 text-ink-2">{o.rider ?? '—'}</td>
                      <td className="px-5 py-3 text-right font-semibold">{rm(o.total_sen)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {list.next && <p className="border-t border-line px-5 py-3 text-xs text-ink-3">Showing the 100 most recent. Narrow by status to see older orders.</p>}
            </div>
          )}
        </Panel>
      </div>
    </>
  )
}
