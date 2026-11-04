import { CheckCircle, ShieldCheck, WarningCircle } from '@phosphor-icons/react'
import { answeredBy, type Answer } from '../lib/api'
import type { List, Order, User } from '../lib/types'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { cx } from '../ui/cx'
import { RefTag } from '../ui/notice'
import type { Attack, Expected } from './attacks'
import { observedBy, verdict, verdicts, type Result } from './verdict'

// The security lab's cards: one test, the baseline's answer and the answer observed.

export function AttackCard(props: { attack: Attack; user: User | null; result?: Result; busy: boolean; disabled: boolean; onFire: () => void }) {
  const { attack, user, result } = props
  const expected = result?.expected ?? attack.expected(user)
  return (
    <li className="flex min-w-0 flex-col rounded-2xl bg-surface p-5 shadow-card">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h3 className="font-bold text-ink">{attack.title}</h3>
          <p className="mt-1 font-mono text-xs break-words text-ink-3">{attack.request}</p>
        </div>
        <Button size="sm" variant="secondary" onClick={props.onFire} disabled={props.disabled} busy={props.busy} busyLabel="Sending…">
          Send
        </Button>
      </div>
      <dl className="mt-4 grid grid-cols-1 gap-x-3 gap-y-1 text-sm sm:grid-cols-[6.5rem_minmax(0,1fr)] sm:gap-y-2">
        <dt className="text-ink-3">Baseline</dt>
        <dd className="text-ink-2">
          <span className="font-semibold text-ink">{expectedWords(expected)}</span> · {expected.why}
        </dd>
        <dt className="text-ink-3">Observed</dt>
        <dd>
          {result ? (
            <Observed result={result} user={attack.id === 'forged-headers' ? user : null} />
          ) : (
            <span className="text-ink-3">{attack.token && !user ? 'Sign in to send this one' : 'Not sent yet'}</span>
          )}
        </dd>
      </dl>
    </li>
  )
}

function expectedWords(expected: Expected): string {
  return `${expected.by === 'gateway' ? 'Refused by the gateway' : 'Answered by the service'} · ${expected.status}`
}

// Observed reports the gateway's answer. owner, when given, is the person
// whose orders an allowed answer must contain and nobody else's.
function Observed({ result, user: owner }: { result: Result; user: User | null }) {
  const last = result.answers[result.answers.length - 1]
  if (result.unsent) return <span className="text-warn">Not sent: this tab’s token could not be read to build the request.</span>
  if (!last) return <span className="text-ink-3">Stopped: the person changed.</span>
  if (last.kind === 'no-answer') return <span className="text-warn">No answer ({last.reason === 'timeout' ? 'it took too long' : 'the connection failed'}). Nothing can be concluded.</span>
  if (last.kind === 'cancelled') return null
  const outcome = verdicts[verdict(result) ?? 'differs']
  const by = observedBy(last)
  return (
    <div className="flex flex-col gap-2">
      <p className="flex flex-wrap items-center gap-2">
        <Badge tone={outcome.tone}>
          {outcome.tone === 'ok' ? <CheckCircle size={14} weight="bold" /> : <WarningCircle size={14} weight="bold" />}
          {outcome.label}
        </Badge>
        <span className="font-semibold text-ink">
          {by === 'gateway' ? 'Answered by the gateway' : `Answered by ${answeredBy(last)}`} · {last.status}
          {'code' in last ? ` ${last.code}` : ''}
        </span>
      </p>
      {result.answers.length > 1 && <Tally answers={result.answers} />}
      {last.kind === 'ok' && owner && <OkDetail answer={last} user={owner} />}
      {'message' in last && last.message && <p className="text-ink-2">{last.message}</p>}
      {outcome.note && <p className="text-ink-2">{outcome.note}</p>}
      {last.ref && (
        <p>
          <RefTag value={last.ref} />
        </p>
      )}
    </div>
  )
}

// Tally sums a burst: how many answers had each status, in order of first
// appearance.
function Tally({ answers }: { answers: Answer<unknown>[] }) {
  const counts = new Map<string, number>()
  for (const answer of answers) {
    const key = 'status' in answer ? String(answer.status) : 'no answer'
    counts.set(key, (counts.get(key) ?? 0) + 1)
  }
  return (
    <p className="flex flex-wrap gap-1.5" aria-label="Answers in order">
      {answers.map((answer, n) => (
        <span
          key={n}
          className={cx(
            'rounded px-1.5 py-0.5 font-mono text-[11px] font-bold tabular-nums',
            'status' in answer && answer.status === 429 ? 'bg-bad-soft text-bad' : 'bg-sunken text-ink-2 ring-1 ring-line',
          )}
        >
          {'status' in answer ? answer.status : '—'}
        </span>
      ))}
      <span className="sr-only">{[...counts].map(([status, count]) => `${count} × ${status}`).join(', ')}</span>
    </p>
  )
}

// OkDetail shows what an allowed request proves: for the forged headers,
// whether every order returned belongs to the signed-in customer.
function OkDetail({ answer, user }: { answer: Extract<Answer<unknown>, { kind: 'ok' }>; user: User }) {
  const data = answer.data as Partial<List<Order>> | null
  if (!data || !Array.isArray(data.items)) return null
  const items = data.items as Order[]
  if (items.length === 0) return <p className="text-ink-2">No orders came back.</p>
  if (!items.every((o) => typeof o.customer === 'string')) return <p className="text-ink-2">{items.length} records came back.</p>
  const own = items.filter((o) => o.customer === user.id).length
  return (
    <p className={cx('flex items-center gap-1.5', own === items.length ? 'text-ok' : 'text-ink-2')}>
      <ShieldCheck size={16} weight="bold" aria-hidden="true" />
      {own === items.length ? `All ${items.length} orders are ${user.id}’s own.` : `${own} of ${items.length} orders are ${user.id}’s own.`}
    </p>
  )
}
