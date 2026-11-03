import type { ReactNode } from 'react'
import { committedAnyway, committedMessage, message, type Outcome, type Reply } from './api'
import type { Mutation } from './hooks'
import { Glyph, MoreDetails } from './ui'

// How replies are shown: refusals, unknown outcomes, receipts and mutation results.

// Problem describes a refused or unanswered call: what happened in plain
// words, what to do, and the exact code, status and field under More
// details. Refused, applied with a problem and no answer keep their own
// titles and tones, because each means something different for a change.
export function Problem<T>(props: { reply: Reply<T> }) {
  const r = props.reply
  if (r.kind === 'unknown') {
    return (
      <div role="alert" className="notice tone-unknown">
        <p className="notice-title">
          <Glyph name="unknown" />
          No answer from the gateway
        </p>
        <p>Check that the gateway is running, then refresh. If you were making a change, check whether it was applied before sending it again.</p>
        <MoreDetails facts={[['Reason', r.message]]} />
      </div>
    )
  }
  if (r.kind !== 'refused') return null
  const code = r.outcome?.reason ?? r.error
  const field = r.outcome?.field ?? r.field
  // A committed change whose audit record or publication failed is
  // reported with an error status, but it was applied: never "Refused".
  const committed = committedAnyway(r)
  const words = committed ? committedMessage(code) : message(code)
  return (
    <div role="alert" className={`notice ${committed ? 'tone-pending' : 'tone-refused'}`}>
      <p className="notice-title">
        <Glyph name={committed ? 'pending' : 'refused'} />
        <span>{words.title}</span>
      </p>
      <p>{words.action}</p>
      <MoreDetails
        facts={[
          ['Error code', code],
          ['HTTP status', String(r.status)],
          ['Field', field],
          ['Transition ID', committed ? r.outcome?.transition_id : undefined],
        ]}
      />
    </div>
  )
}

// ShowMore reads the next page of a list, and says why when it cannot.
export function ShowMore(props: { list: { next?: string; paging: boolean; pageProblem: Reply<unknown> | null; more: () => void } }) {
  const { list } = props
  return (
    <>
      {list.pageProblem ? <Problem reply={list.pageProblem} /> : null}
      {list.next ? (
        <button type="button" className="btn show-more" onClick={list.more} disabled={list.paging}>
          {list.paging ? 'Reading…' : 'Show More'}
        </button>
      ) : null}
    </>
  )
}

// ReplyView renders a loaded resource or the reason it could not be read.
export function ReplyView<T>(props: { reply: Reply<T> | null; children: (body: T) => ReactNode }) {
  const r = props.reply
  if (r === null) return null
  if (r.kind === 'ok') return <>{props.children(r.body)}</>
  return <Problem reply={r} />
}

const receiptFields: [keyof Outcome, string][] = [
  ['outcome', 'Outcome'],
  ['disposition', 'Result (Kept or Undone)'],
  ['reason', 'Reason Code'],
  ['field', 'Field'],
  ['transition_id', 'Transition'],
  ['revision', 'Active Version Now'],
  ['generation', 'Active Generation'],
  ['candidate', 'Draft Change'],
  ['provisional', 'Version on Trial'],
  ['rollback_target', 'Version restored if undone'],
  ['deadline', 'Trial Deadline'],
  ['override', 'Temporary Rule'],
  ['target', 'Target'],
  ['expires_at', 'Expires'],
]

// Receipt shows the controller's record of a mutation exactly as returned.
export function Receipt(props: { outcome: Outcome }) {
  return (
    <MoreDetails facts={receiptFields.map(([key, label]) => [label, props.outcome[key] === undefined ? undefined : String(props.outcome[key])])} />
  )
}

// actorName shows the gateway's own automatic actions as the gateway's.
export function actorName(actor: string | undefined): string {
  return actor === 'controller' ? 'the gateway' : (actor ?? '')
}

// MutationResult reports a mutation's state in a live region.
export function MutationResult<T>(props: {
  mutation: Mutation<T>
  success: (body: T) => ReactNode
  onRefresh: () => void
  unknown?: ReactNode
}) {
  const { phase, run } = props.mutation
  return (
    <div aria-live="polite" className="mutation-result">
      {phase.state === 'pending' ? (
        <p className="notice tone-pending">
          <span className="notice-title">
            <Glyph name="pending" />
            Sent; waiting for the gateway's answer…
          </span>
        </p>
      ) : null}
      {phase.state === 'replied' && phase.reply.kind === 'ok' ? (
        <div className="notice tone-ok">{props.success(phase.reply.body)}</div>
      ) : null}
      {phase.state === 'replied' && phase.reply.kind === 'refused' ? <Problem reply={phase.reply} /> : null}
      {phase.state === 'replied' && phase.reply.kind === 'unknown'
        ? (props.unknown ?? (
            <div role="alert" className="notice tone-unknown">
              <p className="notice-title">
                <Glyph name="unknown" />
                No answer from the gateway: outcome unknown
              </p>
              <p>
                The request was sent but no answer arrived. The gateway may or may not have applied it. Refresh to see the current state before doing
                anything else.
              </p>
              {phase.request.key !== undefined ? (
                <p>
                  Sending the identical request again is safe: it carries the same request key, so the gateway returns its recorded result
                  or applies it once. Until its outcome is known it stays listed at the top of every page.
                </p>
              ) : null}
              <div className="actions">
                <button type="button" className="btn" onClick={props.onRefresh}>
                  Refresh State
                </button>
                {phase.request.key !== undefined ? (
                  <button type="button" className="btn" onClick={() => run(phase.request)}>
                    Send the Identical Request Again
                  </button>
                ) : null}
              </div>
              <MoreDetails
                facts={[
                  ['Reason', phase.reply.message],
                  ['Request key', phase.request.key],
                ]}
              />
            </div>
          ))
        : null}
    </div>
  )
}
