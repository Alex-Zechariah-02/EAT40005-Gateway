import { useEffect, useState, type ReactNode } from 'react'
import type { Admin } from './http'
import type { Activation, ProvisionalView } from './api'
import { usePaged, type Mutation, type Resource } from './hooks'
import { actorName, Problem, ShowMore } from './replies'
import { Remaining, Time } from './time'
import { Badge, Empty, TableScroll } from './ui'
import type { Asking } from './trial'

// A trial in place: the Overview tile, what it changed, its deadline bar, and the list of past trials.

// ChangeSummary says in one line what the change on trial changes.
function ChangeSummary({ view }: { view: ProvisionalView }) {
  if (view.changes.length === 0) return <>No control changed.</>
  const first = view.changes[0]
  return (
    <>
      Changes {controls[first.control] ?? first.control} <code>{first.target}</code>
      {view.changes.length > 1 ? ` and ${view.changes.length - 1} more` : null}
    </>
  )
}

// DeadlineBar draws how much of the confirmation period has passed, from the
// server's activation and deadline times. Like the countdown beside it, it is
// a display aid: the gateway decides when the deadline has passed.
export function DeadlineBar(props: { from: string; until: string }) {
  const start = new Date(props.from).getTime()
  const end = new Date(props.until).getTime()
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const passed = end > start ? Math.min(1, Math.max(0, (now - start) / (end - start))) : 1
  // An SVG attribute, not a style: the content policy allows no inline style.
  return (
    <svg className="deadline-bar" aria-hidden="true" focusable="false">
      <rect height="100%" width={`${passed * 100}%`} rx="2" />
    </svg>
  )
}

// A long list of changed controls shows this many, then the rest on request.
const shownChanges = 12

function ChangedControls({ changes }: { changes: ProvisionalView['changes'] }) {
  return (
    <ul className="changes">
      {changes.map((c) => (
        <li key={`${c.control}:${c.target}`} className="change change-changed">
          <span className="change-control">{controls[c.control] ?? c.control}</span> <code>{c.target}</code>
        </li>
      ))}
    </ul>
  )
}

// The kinds of control the gateway reports as changed, in plain words. A
// permit target is written "ROLE:operation".
export const controls: Record<string, string> = {
  upstream: 'service address',
  api: 'API',
  operation: 'endpoint',
  rate: 'rate limit',
  role: 'role',
  permit: 'permission',
  subject: 'account',
}

const dispositions: Record<string, { tone: 'ok' | 'neutral' | 'refused'; word: string }> = {
  confirmed: { tone: 'ok', word: 'Kept' },
  rolled_back: { tone: 'neutral', word: 'Undone' },
  timed_out: { tone: 'neutral', word: 'Undone at the Deadline' },
  quarantined: { tone: 'refused', word: 'Set aside after a restore' },
}

// ActivationHistory lists every trial and how it ended.
export function ActivationHistory({ admin, reads }: { admin: Admin; reads: unknown }) {
  const history = usePaged<Activation>(admin, '/admin/activations', [reads])
  return (
    <section className="section" aria-labelledby="trials-title">
      <h2 id="trials-title">Past Trials</h2>
      {history.reply && history.reply.kind !== 'ok' ? <Problem reply={history.reply} /> : null}
      {history.reply?.kind === 'ok' && history.items.length === 0 ? <Empty title="No change has been put on trial yet." /> : null}
      {history.items.length > 0 ? (
        <TableScroll label="Past Trials">
          <table>
            <thead>
              <tr>
                <th scope="col">Version</th>
                <th scope="col">From Draft Change</th>
                <th scope="col">Started</th>
                <th scope="col">Deadline</th>
                <th scope="col">How It Ended</th>
              </tr>
            </thead>
            <tbody>
              {history.items.map((a) => (
                <tr key={a.revision}>
                  <td className="num">{a.revision}</td>
                  <td className="num">{a.candidate}</td>
                  <td>
                    <Time value={a.activated_at} /> <span className="muted">by {a.actor}</span>
                  </td>
                  <td>
                    <Time value={a.deadline} />
                  </td>
                  <td>
                    {a.disposition ? (
                      <Badge tone={dispositions[a.disposition]?.tone ?? 'neutral'}>{dispositions[a.disposition]?.word ?? a.disposition}</Badge>
                    ) : (
                      <Badge tone="pending">On Trial Now</Badge>
                    )}
                    {a.resolved_at ? (
                      <span className="cell-sub">
                        <Time value={a.resolved_at} /> by {actorName(a.resolved_by)}
                        {a.disposition && a.disposition !== 'confirmed' ? ` · back to version ${a.rollback_target}` : null}
                      </span>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      ) : null}
      <ShowMore list={history} />
    </section>
  )
}

// TrialSide lists what the change on trial changed and where it came from.
export function TrialSide({ view }: { view: ProvisionalView }) {
  return (
    <div className="trial-side">
      <h3>What Changed from Version {view.activation.rollback_target}</h3>
      {view.changes.length === 0 ? (
        <p className="hint">Nothing, as the gateway reports it.</p>
      ) : (
        <>
          <p className="hint">
            {view.changes.length} {view.changes.length === 1 ? 'change' : 'changes'}, as the gateway reports them.
          </p>
          <ChangedControls changes={view.changes.slice(0, shownChanges)} />
          {view.changes.length > shownChanges ? (
            <details className="more-changes">
              <summary>Show the Other {view.changes.length - shownChanges}</summary>
              <ChangedControls changes={view.changes.slice(shownChanges)} />
            </details>
          ) : null}
        </>
      )}
      <dl className="facts facts-compact">
        <div className="fact">
          <dt>From Draft Change</dt>
          <dd>{view.activation.candidate}</dd>
        </div>
        <div className="fact">
          <dt>Started</dt>
          <dd>
            <Time value={view.activation.activated_at} /> by {view.activation.actor}
          </dd>
        </div>
        <div className="fact">
          <dt>Active Generation</dt>
          <dd>{view.generation ? <code>{view.generation}</code> : 'not the active generation'}</dd>
        </div>
      </dl>
    </div>
  )
}

// TrialTile is the change on trial as an Overview tile, with Undo and a link to Changes.
export function TrialTile(props: {
  view: ProvisionalView | undefined
  current: Resource<ProvisionalView>
  none: boolean
  manage: boolean
  mutation: Mutation<unknown>
  refresh: () => void
  setAsking: (value: Asking) => void
  stale: ReactNode
  result: ReactNode
  dialogs: ReactNode
}) {
  const { view, current, none, manage, mutation, refresh, setAsking, stale, result, dialogs } = props
  if (!view) {
    return (
      <div className="tile tile-empty">
        <p className="tile-label">Change on Trial</p>
        {current.reply && current.reply.kind !== 'ok' && !none ? (
          <Problem reply={current.reply} />
        ) : (
          <p className="tile-value tile-value-quiet">None</p>
        )}
        {mutation.phase.state === 'replied' ? null : (
          <p className="tile-foot">
            <a href="#/changes">Open Changes</a>
          </p>
        )}
        {result}
      </div>
    )
  }
  return (
    <div className="tile tile-trial tone-pending">
      <div className="tile-row">
        <div>
          <p className="tile-label tile-label-tone">Change on Trial</p>
          <p className="tile-value">Version {view.activation.revision}</p>
        </div>
        <div className="tile-end">
          <p className="tile-label">Undone in</p>
          <p className="tile-countdown">
            <Remaining key={view.activation.deadline} until={view.activation.deadline} onPassed={refresh} format="clock" />
          </p>
        </div>
      </div>
      <DeadlineBar from={view.activation.activated_at} until={view.activation.deadline} />
      <div className="tile-row tile-row-end">
        <p className="tile-note">
          <ChangeSummary view={view} />
        </p>
        <div className="actions">
          {manage ? (
            <button type="button" className="btn btn-small" onClick={() => setAsking('rollback')} disabled={mutation.pending}>
              Undo Change
            </button>
          ) : null}
          <a className="btn btn-small btn-primary" href="#/changes">
            {manage ? 'Review and Keep' : 'Open Changes'}
          </a>
        </div>
      </div>
      {stale}
      {result}
      {dialogs}
    </div>
  )
}
