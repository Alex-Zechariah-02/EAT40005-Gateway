import { useEffect, useState } from 'react'
import { newKey, type Activation, type Admin, type Outcome, type ProvisionalView } from './api'
import { useGatewayState, useMutation, usePaged, useResource } from './hooks'
import { Badge, Confirm, Empty, MutationResult, Problem, Receipt, Remaining, ShowMore, TableScroll, Time } from './ui'

// TrialDecision shows the change on trial (the provisional policy) with the
// server's deadline and its changed controls, and offers confirmation and
// rollback. Only an explicit confirmation makes it permanent; traffic, page
// loads and the countdown never do. The Overview shows it as a tile; Changes
// shows it in full.
export function TrialDecision(props: { admin: Admin; manage: boolean; variant: 'tile' | 'full'; onChanged?: () => void }) {
  const { admin, manage, variant } = props
  const state = useGatewayState()
  const current = useResource<ProvisionalView>((signal) => admin.get('/admin/provisional', signal), [state.readAt])
  const refresh = () => {
    state.refresh()
    props.onChanged?.()
  }
  const mutation = useMutation(admin, refresh)
  const [asking, setAsking] = useState<'confirm' | 'rollback' | null>(null)

  const view = current.reply?.kind === 'ok' ? current.reply.body : undefined
  // The ETag names the confirmed revision a confirmation or rollback is
  // based on; the server refuses either if it has changed.
  const etag = current.reply && current.reply.kind !== 'unknown' && current.reply.kind !== 'aborted' ? current.reply.etag : null
  const none = current.reply?.kind === 'refused' && current.reply.error === 'no_provisional'

  function resolve(action: 'confirm' | 'rollback') {
    setAsking(null)
    if (!view) return
    mutation.run({
      method: 'POST',
      path: `/admin/provisional/${action}`,
      body: { revision: view.activation.revision },
      ifMatch: etag ?? undefined,
      key: newKey(),
    })
  }

  const result = (
    <MutationResult<Outcome>
      mutation={mutation}
      onRefresh={refresh}
      success={(o) => (
        <>
          <p className="notice-title">
            {o.disposition === 'confirmed' ? `Confirmed: revision ${o.revision} is now the permanent policy.` : `Rolled back: revision ${o.revision} is in force again.`}
          </p>
          {variant === 'full' ? <Receipt outcome={o} /> : null}
        </>
      )}
    />
  )

  const dialogs = view ? (
    <>
      <Confirm
        open={asking === 'confirm'}
        title={`Confirm revision ${view.activation.revision} permanently?`}
        confirmLabel={`Confirm revision ${view.activation.revision}`}
        onCancel={() => setAsking(null)}
        onConfirm={() => resolve('confirm')}
      >
        <p>
          Revision {view.activation.revision} becomes the confirmed policy and no longer rolls back by itself. Revision {view.activation.rollback_target} stops
          being the rollback target.
        </p>
      </Confirm>
      <Confirm
        open={asking === 'rollback'}
        title={`Roll back to revision ${view.activation.rollback_target}?`}
        confirmLabel={`Roll back to revision ${view.activation.rollback_target}`}
        danger
        onCancel={() => setAsking(null)}
        onConfirm={() => resolve('rollback')}
      >
        <p>
          Revision {view.activation.rollback_target} is enforced again from the next request, and the trial of revision {view.activation.revision} ends.
        </p>
      </Confirm>
    </>
  ) : null

  const buttons = (small: boolean) =>
    view && manage ? (
      <div className="actions">
        <button type="button" className={`btn${small ? ' btn-small' : ''}`} onClick={() => setAsking('rollback')} disabled={mutation.pending}>
          Roll back{small ? null : ` to revision ${view.activation.rollback_target}`}
        </button>
        <button type="button" className={`btn btn-primary${small ? ' btn-small' : ''}`} onClick={() => setAsking('confirm')} disabled={mutation.pending}>
          Confirm{small ? null : ` revision ${view.activation.revision}`}
        </button>
      </div>
    ) : null

  if (variant === 'tile') {
    if (!view) {
      return (
        <div className="tile">
          <p className="tile-label">Change on trial</p>
          {current.reply && current.reply.kind !== 'ok' && !none ? (
            <Problem reply={current.reply} />
          ) : (
            <>
              <p className="tile-value tile-value-quiet">None</p>
              <p className="tile-note">
                {mutation.phase.state === 'replied' ? null : 'Changes go live on trial first and roll back unless confirmed.'}
              </p>
            </>
          )}
          {result}
        </div>
      )
    }
    return (
      <div className="tile tile-trial tone-pending">
        <div className="tile-row">
          <div>
            <p className="tile-label tile-label-tone">Change on trial</p>
            <p className="tile-value">Revision {view.activation.revision}</p>
          </div>
          <div className="tile-end">
            <p className="tile-label">Rolls back in</p>
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
          {manage ? buttons(true) : <a href="#/candidates">Open Changes</a>}
        </div>
        {result}
        {dialogs}
      </div>
    )
  }

  if (none && mutation.phase.state === 'idle') return null
  return (
    <>
      {current.reply && current.reply.kind !== 'ok' && !none ? <Problem reply={current.reply} /> : null}
      {view ? (
        <section className="section card trial tone-pending" aria-labelledby="trial-title">
          <div className="trial-main">
            <p className="tile-label tile-label-tone">Change on trial</p>
            <h2 id="trial-title">Revision {view.activation.revision} is in force but not confirmed</h2>
            <div className="trial-clock">
              <p className="tile-countdown">
                <Remaining key={view.activation.deadline} until={view.activation.deadline} onPassed={refresh} format="clock" />
              </p>
              <p className="hint">
                left to confirm, until <Time value={view.activation.deadline} />. Then the gateway rolls back to revision {view.activation.rollback_target} by
                itself.
              </p>
            </div>
            <DeadlineBar from={view.activation.activated_at} until={view.activation.deadline} />
            {buttons(false)}
          </div>
          <div className="trial-side">
            <h3>What changed from revision {view.activation.rollback_target}</h3>
            {view.changes.length === 0 ? (
              <p className="hint">Nothing, as the gateway reports it.</p>
            ) : (
              <ul className="changes">
                {view.changes.map((c) => (
                  <li key={`${c.control}:${c.target}`} className="change change-changed">
                    <span className="change-control">{controls[c.control] ?? c.control}</span> <code>{c.target}</code>
                  </li>
                ))}
              </ul>
            )}
            <dl className="facts facts-compact">
              <div className="fact">
                <dt>From candidate</dt>
                <dd>{view.activation.candidate}</dd>
              </div>
              <div className="fact">
                <dt>Started</dt>
                <dd>
                  <Time value={view.activation.activated_at} /> by {view.activation.actor}
                </dd>
              </div>
              <div className="fact">
                <dt>Generation served</dt>
                <dd>{view.generation ? <code>{view.generation}</code> : 'not the serving generation'}</dd>
              </div>
            </dl>
          </div>
        </section>
      ) : null}
      {result}
      {dialogs}
    </>
  )
}

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
function DeadlineBar(props: { from: string; until: string }) {
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

// The kinds of control the gateway reports as changed. A permit target is
// written "ROLE:operation".
const controls: Record<string, string> = {
  upstream: 'upstream',
  api: 'API',
  operation: 'operation',
  rate: 'rate limit',
  role: 'role',
  permit: 'permit',
  subject: 'subject',
}

const dispositions: Record<string, { tone: 'ok' | 'neutral' | 'refused'; word: string }> = {
  confirmed: { tone: 'ok', word: 'Confirmed' },
  rolled_back: { tone: 'neutral', word: 'Rolled back' },
  timed_out: { tone: 'neutral', word: 'Rolled back at deadline' },
  quarantined: { tone: 'refused', word: 'Quarantined' },
}

// ActivationHistory lists every trial and how it ended.
export function ActivationHistory({ admin, reads }: { admin: Admin; reads: unknown }) {
  const history = usePaged<Activation>(admin, '/admin/activations', [reads])
  return (
    <section className="section" aria-labelledby="trials-title">
      <h2 id="trials-title">Past trials</h2>
      {history.reply && history.reply.kind !== 'ok' ? <Problem reply={history.reply} /> : null}
      {history.reply?.kind === 'ok' && history.items.length === 0 ? <Empty title="No change has been put on trial yet." /> : null}
      {history.items.length > 0 ? (
        <TableScroll label="Past trials">
          <table>
            <thead>
              <tr>
                <th scope="col">Revision</th>
                <th scope="col">From candidate</th>
                <th scope="col">Started</th>
                <th scope="col">Deadline</th>
                <th scope="col">How it ended</th>
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
                      <Badge tone="pending">On trial now</Badge>
                    )}
                    {a.resolved_at ? (
                      <span className="cell-sub">
                        <Time value={a.resolved_at} /> by {a.resolved_by}
                        {a.disposition && a.disposition !== 'confirmed' ? ` · back to revision ${a.rollback_target}` : null}
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
