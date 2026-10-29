import { useState, type FormEvent } from 'react'
import { CaretRight, ChatsCircle, Lifebuoy } from '@phosphor-icons/react'
import { outcomeOfWrite } from '../lib/api'
import { when, reference } from '../lib/format'
import { describe, type Problem } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link, navigate, useLocation } from '../lib/router'
import { call } from '../lib/session'
import type { List, Order, Ticket } from '../lib/types'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { SelectField, TextArea, TextField } from '../ui/field'
import { Empty, Loading, ProblemNotice, RefreshProblem, Skeleton } from '../ui/notice'

export const ticketTone = { open: 'warn', answered: 'brand', resolved: 'ok' } as const
export const ticketStatus = { open: 'Waiting for Fetch', answered: 'Fetch replied', resolved: 'Resolved' } as const

export function HelpPage() {
  const tickets = useQuery<List<Ticket>>('/support/mine', { poll: 20_000 })
  const problem = tickets.answer && !tickets.data ? describe(tickets.answer) : null
  return (
    <div className="mt-8 grid grid-cols-1 items-start gap-8 lg:grid-cols-[1fr_440px]">
      <section aria-labelledby="tickets-heading">
        <h1 className="text-[36px] leading-none font-extrabold tracking-[-0.035em]">Help</h1>
        <h2 id="tickets-heading" className="mt-8 text-lg font-extrabold tracking-[-0.015em]">Your requests</h2>
        <RefreshProblem query={tickets} className="mt-3" />
        {problem ? (
          <ProblemNotice className="mt-3" problem={problem} action={<button type="button" onClick={tickets.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
        ) : !tickets.data ? (
          <Loading label="Loading your requests"><div className="mt-3 flex flex-col gap-3">{[0, 1].map((n) => <Skeleton key={n} className="h-20" />)}</div></Loading>
        ) : tickets.data.items.length === 0 ? (
          <Empty className="mt-3" icon={<ChatsCircle size={26} weight="bold" />} title="No help requests" body="If something goes wrong with an order, tell us here and a support agent will reply." />
        ) : (
          <ul className="mt-3 flex flex-col gap-3">
            {tickets.data.items.map((ticket) => (
              <li key={ticket.id}>
                <Link to={`/help/${ticket.id}`} className="flex items-center gap-4 rounded-2xl bg-surface p-4 shadow-card transition-shadow hover:shadow-raised">
                  <span className="min-w-0 flex-1">
                    <span className="flex flex-wrap items-center gap-2"><span className="font-bold">{ticket.subject}</span><Badge tone={ticketTone[ticket.status]}>{ticketStatus[ticket.status]}</Badge></span>
                    <span className="mt-1 block truncate text-sm text-ink-2">{ticket.messages.at(-1)?.body}</span>
                    <span className="mt-1 block text-xs text-ink-3">{reference(ticket.id)}{ticket.order ? ` · Order ${reference(ticket.order)}` : ''} · {when(ticket.messages.at(-1)?.at ?? ticket.created_at)}</span>
                  </span>
                  <CaretRight size={18} className="text-ink-3" aria-hidden="true" />
                </Link>
              </li>
            ))}
          </ul>
        )}
      </section>
      <NewTicket onCreated={(id) => navigate(`/help/${id}`)} />
    </div>
  )
}

function NewTicket({ onCreated }: { onCreated: (id: string) => void }) {
  const { params } = useLocation()
  const orders = useQuery<List<Order>>('/orders/mine?limit=20')
  const [subject, setSubject] = useState(params.get('ref') ? 'Something went wrong' : '')
  const [message, setMessage] = useState('')
  const [order, setOrder] = useState(params.get('order') ?? '')
  const [ref, setRef] = useState(params.get('ref') ?? '')
  const [tried, setTried] = useState(false)
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<Problem | null>(null)

  const errors = {
    subject: subject.trim().length < 5 ? 'Write at least 5 characters, like “Missing item”.' : null,
    message: !message.trim() ? 'Tell us what happened.' : null,
    ref: ref && (ref.length < 16 || ref.length > 64) ? 'A reference is 16 to 64 characters. Copy it from the error message.' : null,
  }
  const invalid = Object.values(errors).some(Boolean)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setTried(true)
    if (invalid || busy) return
    setBusy(true)
    setProblem(null)
    const body = { subject: subject.trim(), message: message.trim(), ...(order ? { order } : {}), ...(ref ? { request_ref: ref.trim() } : {}) }
    const answer = await call<{ ticket: Ticket }>({ method: 'POST', path: '/support/tickets', body })
    setBusy(false)
    if (answer.kind === 'cancelled') return
    if (answer.kind === 'ok') {
      onCreated(answer.data.ticket.id)
      return
    }
    setProblem(describe(answer, outcomeOfWrite(answer) === 'unknown' ? { object: 'your request', done: 'sent', check: 'Check your requests on this page' } : undefined))
  }

  return (
    <section className="rounded-3xl bg-surface p-6 shadow-raised lg:sticky lg:top-24" aria-labelledby="new-ticket-heading">
      <h2 id="new-ticket-heading" className="flex items-center gap-2 text-lg font-extrabold tracking-[-0.015em]"><Lifebuoy size={20} aria-hidden="true" />Ask Fetch for help</h2>
      <form onSubmit={submit} noValidate className="mt-4 flex flex-col gap-4">
        <TextField label="What is it about?" value={subject} onChange={(e) => setSubject(e.target.value)} maxLength={120} error={tried ? errors.subject : null} placeholder="Missing item" />
        <SelectField label="Order" optional value={order} onChange={(e) => setOrder(e.target.value)}>
          <option value="">Not about one order</option>
          {orders.data?.items.map((o) => <option key={o.id} value={o.id}>{reference(o.id)} · {o.merchant_name}</option>)}
          {order && !orders.data?.items.some((o) => o.id === order) && <option value={order}>{reference(order)}</option>}
        </SelectField>
        <TextArea label="What happened?" value={message} onChange={(e) => setMessage(e.target.value.slice(0, 2000))} rows={5} error={tried ? errors.message : null} hint="Include anything that helps, like which item or what you saw on screen." />
        <TextField
          label="Reference from an error message"
          optional
          value={ref}
          onChange={(e) => setRef(e.target.value.trim())}
          maxLength={64}
          error={tried ? errors.ref : null}
          hint="Errors on Fetch show “Ref” followed by a code. It lets support see exactly what happened."
          spellCheck={false}
          autoCapitalize="none"
        />
        {problem && <ProblemNotice problem={problem} />}
        <Button type="submit" size="lg" block busy={busy} busyLabel="Sending…">Send to Fetch support</Button>
      </form>
    </section>
  )
}
