import { useState, type FormEvent } from 'react'
import { ChatsCircle } from '@phosphor-icons/react'
import { outcomeOfWrite } from '../lib/api'
import { when, reference } from '../lib/format'
import { describe, type Problem } from '../lib/messages'
import { useQuery } from '../lib/query'
import { Link } from '../lib/router'
import { call, useSession } from '../lib/session'
import type { Ticket } from '../lib/types'
import { Badge } from '../ui/badge'
import { Button, ButtonLink } from '../ui/button'
import { cx } from '../ui/cx'
import { TextArea } from '../ui/field'
import { AnswerLine, Empty, Loading, ProblemNotice, RefTag, RefreshProblem, Skeleton } from '../ui/notice'
import { ticketStatus, ticketTone } from './help'

export function TicketPage({ id }: { id: string }) {
  const page = useQuery<{ ticket: Ticket }>(`/support/ticket/${encodeURIComponent(id)}`, { poll: 10_000 })
  const problem = page.answer && !page.data ? describe(page.answer) : null
  if (problem) {
    return problem.status === 404 ? (
      <Empty className="mt-10" icon={<ChatsCircle size={26} weight="bold" />} title="We couldn’t find this request" body="It may belong to another account." action={<ButtonLink to="/help">See your requests</ButtonLink>} footer={<AnswerLine problem={problem} className="mt-5 justify-center" />} />
    ) : (
      <ProblemNotice className="mt-10" problem={problem} action={<button type="button" onClick={page.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
    )
  }
  if (!page.data) return <Loading label="Loading your request"><div className="mx-auto mt-8 flex max-w-[760px] flex-col gap-4"><Skeleton className="h-12 w-72" /><Skeleton className="h-40" /></div></Loading>
  return (
    <>
      <RefreshProblem query={page} className="mx-auto mt-8 max-w-[760px]" />
      <Thread ticket={page.data.ticket} onChanged={page.reload} viewer="customer" />
    </>
  )
}

// Thread shows a ticket's messages and a reply box; customers and support
// agents both use it. A message is the viewer's own only when this tab's
// account wrote it: several agents answer the same tickets.
export function Thread({ ticket, onChanged, viewer, actions }: { ticket: Ticket; onChanged: () => void; viewer: 'customer' | 'agent'; actions?: React.ReactNode }) {
  const { session } = useSession()
  const [reply, setReply] = useState('')
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<Problem | null>(null)
  const resolved = ticket.status === 'resolved'

  const send = async (event: FormEvent) => {
    event.preventDefault()
    if (!reply.trim() || busy) return
    setBusy(true)
    setProblem(null)
    const answer = await call<{ ticket: Ticket }>({ method: 'POST', path: `/support/reply/${encodeURIComponent(ticket.id)}`, body: { message: reply.trim() } })
    setBusy(false)
    if (answer.kind === 'cancelled') return
    if (answer.kind === 'ok') setReply('')
    else setProblem(describe(answer, outcomeOfWrite(answer) === 'unknown' ? { object: 'your reply', done: 'sent', check: 'Refresh the conversation' } : undefined))
    onChanged()
  }

  return (
    <div className={cx(viewer === 'customer' && 'mx-auto mt-8 max-w-[760px]')}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          {viewer === 'customer' && <p className="text-sm font-semibold text-ink-3"><Link to="/help" className="hover:text-ink">Help</Link> · Request {reference(ticket.id)}</p>}
          {viewer === 'customer' ? (
            <h1 className="mt-1 text-[32px] leading-none font-extrabold tracking-[-0.03em]">{ticket.subject}</h1>
          ) : (
            <h2 className="text-2xl font-extrabold tracking-[-0.03em]">{ticket.subject}</h2>
          )}
          <p className="mt-2 flex flex-wrap items-center gap-2 text-sm text-ink-2">
            <Badge tone={ticketTone[ticket.status]}>{ticketStatus[ticket.status]}</Badge>
            {ticket.order && (viewer === 'customer' ? <Link to={`/my-orders/${ticket.order}`} className="font-semibold text-brand hover:underline">Order {ticket.order}</Link> : <span>Order {ticket.order}</span>)}
            {ticket.request_ref && <RefTag value={ticket.request_ref} />}
          </p>
        </div>
        {actions}
      </div>
      <ol className="mt-6 flex flex-col gap-3" aria-label="Conversation">
        {ticket.messages.map((message, index) => {
          const mine = message.author === session?.user.id
          let author = message.author_name
          if (message.from === 'agent') author = mine ? 'You · Fetch support' : viewer === 'agent' ? `${message.author_name} · Fetch support` : 'Support agent from Fetch'
          else if (mine) author = 'You'
          return (
            <li key={index} className={cx('max-w-[85%] rounded-2xl p-4', mine ? 'self-end bg-brand-soft' : 'self-start bg-surface shadow-card')}>
              <p className="text-xs font-bold text-ink-3">{author}</p>
              <p className="mt-1 text-[15px] whitespace-pre-wrap text-ink">{message.body}</p>
              <p className="mt-1.5 text-xs text-ink-3">{when(message.at)}</p>
            </li>
          )
        })}
      </ol>
      {problem && <ProblemNotice className="mt-4" problem={problem} />}
      {resolved ? (
        <p className="mt-6 rounded-2xl bg-sunken p-4 text-sm text-ink-2">
          This request is resolved.{viewer === 'customer' && <> If you still need help, <Link to="/help" className="font-semibold text-brand hover:underline">start a new request</Link>.</>}
        </p>
      ) : (
        <form onSubmit={send} className="mt-6 flex flex-col gap-3 rounded-3xl bg-surface p-5 shadow-card">
          <TextArea label={viewer === 'customer' ? 'Reply to Fetch support' : 'Reply to the customer'} value={reply} onChange={(e) => setReply(e.target.value.slice(0, 2000))} rows={3} />
          <div className="flex justify-end">
            <Button type="submit" busy={busy} busyLabel="Sending…" disabled={!reply.trim()}>Send reply</Button>
          </div>
        </form>
      )}
    </div>
  )
}
