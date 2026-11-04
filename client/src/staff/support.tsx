import { CaretRight, ChatsCircle, Receipt } from '@phosphor-icons/react'
import { when, reference } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { navigate, useLocation } from '../lib/router'
import type { List, Order, Ticket } from '../lib/types'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { cx } from '../ui/cx'
import { Empty, Loading, ProblemNotice, RefTag, RefreshProblem, Skeleton } from '../ui/notice'
import { ticketStatus, ticketTone } from '../help/help'
import { OrderCard, Tabs, WorkspaceTitle, useAction } from './staff'
import { Thread } from '../help/ticket'

type Filter = 'open' | 'answered' | 'resolved' | 'all'

// The support desk: the ticket queue, one ticket with its linked order and
// request reference, replies and resolving. The reference is what an
// administrator looks up in the gateway Dashboard.
export function SupportPage() {
  const { params } = useLocation()
  const filter = (['open', 'answered', 'resolved', 'all'].includes(params.get('status') ?? '') ? params.get('status') : 'open') as Filter
  const selected = params.get('ticket')
  const queue = useQuery<List<Ticket>>(`/support/queue${filter === 'all' ? '' : `?status=${filter}`}`, { poll: 10_000 })
  const problem = queue.answer && !queue.data ? describe(queue.answer) : null
  const go = (next: { status?: Filter; ticket?: string | null }) => {
    const q = new URLSearchParams()
    const status = next.status ?? filter
    if (status !== 'open') q.set('status', status)
    const ticket = next.ticket === undefined ? selected : next.ticket
    if (ticket) q.set('ticket', ticket)
    navigate(`/support${q.size ? `?${q}` : ''}`, { replace: true })
  }
  return (
    <>
      <WorkspaceTitle title="Tickets" />
      <Tabs tabs={[{ id: 'open', label: 'Waiting' }, { id: 'answered', label: 'Answered' }, { id: 'resolved', label: 'Resolved' }, { id: 'all', label: 'All' }]} current={filter} href={(id) => (id === 'open' ? '/support' : `/support?status=${id}`)} />
      <div className="mt-6 grid grid-cols-1 items-start gap-6 lg:grid-cols-[380px_1fr]">
        <section aria-label="Ticket queue">
          <RefreshProblem query={queue} className="mb-3" />
          {problem ? (
            <ProblemNotice problem={problem} action={<Button size="sm" variant="secondary" onClick={queue.reload}>Try again</Button>} />
          ) : !queue.data ? (
            <Loading label="Loading tickets"><div className="flex flex-col gap-2">{[0, 1, 2].map((n) => <Skeleton key={n} className="h-20" />)}</div></Loading>
          ) : queue.data.items.length === 0 ? (
            <Empty icon={<ChatsCircle size={26} weight="bold" />} title="Nothing here" body={filter === 'open' ? 'Every customer has an answer.' : 'No tickets with this status.'} />
          ) : (
            <ul className="flex flex-col gap-2">
              {queue.data.items.map((t) => (
                <li key={t.id}>
                  <button type="button" onClick={() => go({ ticket: t.id })} aria-current={selected === t.id ? 'true' : undefined} className={cx('flex w-full items-center gap-3 rounded-2xl p-4 text-left transition-shadow', selected === t.id ? 'bg-brand-soft ring-2 ring-brand' : 'bg-surface shadow-card hover:shadow-raised')}>
                    <span className="min-w-0 flex-1">
                      <span className="flex flex-wrap items-center gap-2"><span className="font-bold">{t.subject}</span><Badge tone={ticketTone[t.status]}>{ticketStatus[t.status]}</Badge></span>
                      <span className="mt-1 block truncate text-sm text-ink-2">{t.messages.at(-1)?.body}</span>
                      <span className="mt-1 block text-xs text-ink-3">{reference(t.id)} · {t.customer_name} · {when(t.created_at)}</span>
                    </span>
                    <CaretRight size={16} className="text-ink-3" aria-hidden="true" />
                  </button>
                </li>
              ))}
            </ul>
          )}
        </section>
        <section aria-label="Ticket" className="min-w-0">
          {selected ? <TicketDetail key={selected} id={selected} onChanged={queue.reload} /> : (
            <Empty icon={<ChatsCircle size={26} weight="bold" />} title="Choose a ticket" body="Pick one from the queue to read it, see the order and reply." />
          )}
        </section>
      </div>
    </>
  )
}

function TicketDetail({ id, onChanged }: { id: string; onChanged: () => void }) {
  const ticket = useQuery<{ ticket: Ticket }>(`/support/ticket/${encodeURIComponent(id)}`, { poll: 10_000 })
  const order = useQuery<{ order: Order }>(ticket.data?.ticket.order ? `/orders/detail/${encodeURIComponent(ticket.data.ticket.order)}` : null)
  const resolve = useAction(() => {
    ticket.reload()
    onChanged()
  })
  const problem = ticket.answer && !ticket.data ? describe(ticket.answer) : null
  if (problem) return <ProblemNotice problem={problem} />
  if (!ticket.data) return <Loading label="Loading the ticket"><Skeleton className="h-80 rounded-3xl" /></Loading>
  const t = ticket.data.ticket
  return (
    <div className="flex flex-col gap-4">
      <RefreshProblem query={ticket} />
      <div className="rounded-3xl bg-surface p-6 shadow-card">
        <Thread
          ticket={t}
          viewer="agent"
          onChanged={() => {
            ticket.reload()
            onChanged()
          }}
          actions={t.status !== 'resolved' && (
            <Button variant="secondary" size="sm" busy={resolve.busy === 'resolve'} busyLabel="Resolving…" onClick={() => void resolve.run('resolve', `/support/resolve/${encodeURIComponent(t.id)}`, { object: 'resolving this ticket', done: 'resolved', check: 'Refresh the ticket' })}>Mark resolved</Button>
          )}
        />
        {resolve.problem && <ProblemNotice className="mt-4" problem={resolve.problem.problem} />}
      </div>
      {t.request_ref && (
        <div className="rounded-3xl bg-surface p-5 shadow-card">
          <h3 className="font-extrabold">Request reference</h3>
          <p className="mt-1 text-sm text-ink-2">The customer reported this reference from an error. A gateway administrator can paste it into the Dashboard’s header search to see exactly which check decided it; the Dashboard’s Security Log finds older requests.</p>
          <div className="mt-3"><RefTag value={t.request_ref} /></div>
        </div>
      )}
      {t.order && (
        <div>
          <h3 className="mb-2 flex items-center gap-2 font-extrabold"><Receipt size={18} aria-hidden="true" />Linked order</h3>
          {order.data ? <OrderCard order={order.data.order} view="support" wide /> : order.answer ? <ProblemNotice problem={describe(order.answer)!} /> : <Skeleton className="h-32" />}
        </div>
      )}
    </div>
  )
}
