import { CaretRight, Receipt } from '@phosphor-icons/react'
import { rm, when, reference } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link } from '../lib/router'
import type { List, Order } from '../lib/types'
import { OrderBadge } from '../ui/badge'
import { ButtonLink } from '../ui/button'
import { Empty, Loading, ProblemNotice, RefreshProblem, Skeleton } from '../ui/notice'
import { Photo } from '../ui/photo'

export function OrdersPage() {
  const orders = useQuery<List<Order>>('/orders/mine?limit=50', { poll: 15_000 })
  const problem = orders.answer && !orders.data ? describe(orders.answer) : null
  const items = orders.data?.items ?? []
  const active = items.filter((o) => o.status !== 'delivered' && o.status !== 'cancelled')
  const past = items.filter((o) => o.status === 'delivered' || o.status === 'cancelled')
  return (
    <div className="mx-auto mt-8 max-w-[860px]">
      <h1 className="text-[36px] leading-none font-extrabold tracking-[-0.035em]">My orders</h1>
      <RefreshProblem query={orders} report className="mt-6" />
      {problem ? (
        <ProblemNotice className="mt-6" problem={problem} report action={<button type="button" onClick={orders.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
      ) : !orders.data ? (
        <Loading label="Loading your orders">
          <div className="mt-6 flex flex-col gap-3">{[0, 1, 2].map((n) => <Skeleton key={n} className="h-24" />)}</div>
        </Loading>
      ) : items.length === 0 ? (
        <Empty className="mt-6" icon={<Receipt size={26} weight="bold" />} title="No orders yet" body="When you place an order, you can follow it here from the kitchen to your door." action={<ButtonLink to="/">Find something to order</ButtonLink>} />
      ) : (
        <>
          {active.length > 0 && <OrderList title="In progress" orders={active} />}
          {past.length > 0 && <OrderList title="Past orders" orders={past} />}
          {orders.data.next && <p className="mt-6 text-sm text-ink-3">Showing your 50 most recent orders.</p>}
        </>
      )}
    </div>
  )
}

function OrderList({ title, orders }: { title: string; orders: Order[] }) {
  return (
    <section className="mt-8" aria-label={title}>
      <h2 className="text-lg font-extrabold tracking-[-0.015em]">{title}</h2>
      <ul className="mt-3 flex flex-col gap-3">
        {orders.map((order) => {
          const count = order.items.reduce((sum, line) => sum + line.quantity, 0)
          return (
            <li key={order.id}>
              <Link to={`/my-orders/${order.id}`} className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-2xl bg-surface p-4 shadow-card transition-shadow hover:shadow-raised">
                <Photo id={order.merchant} className="size-16 shrink-0 rounded-xl" />
                <span className="min-w-0 flex-1 basis-40">
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="font-bold text-ink">{order.merchant_name}</span>
                    <OrderBadge status={order.status} />
                  </span>
                  <span className="mt-1 block truncate text-sm text-ink-2">{order.items.map((l) => `${l.quantity} × ${l.name}`).join(', ')}</span>
                  <span className="mt-1 block text-xs text-ink-3">{when(order.created_at)} · Order {reference(order.id)} · {count === 1 ? '1 item' : `${count} items`}</span>
                </span>
                <span className="ml-auto font-bold tabular-nums">{rm(order.total_sen)}</span>
                <CaretRight size={18} className="text-ink-3" aria-hidden="true" />
              </Link>
            </li>
          )
        })}
      </ul>
    </section>
  )
}
