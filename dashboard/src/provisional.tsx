import { useEffect, useRef, useState } from 'react'
import { newKey, type Activation, type Admin, type Outcome, type ProvisionalView, type Reply, type Status } from './api'
import { useGatewayState, useMutation, usePaged, liveMs, usePoll, useRereadWhen, useResource } from './hooks'
import { reasonOf, type Traffic } from './monitor'
import { actorName, age, Badge, clock, Confirm, Empty, MutationResult, Problem, Receipt, Remaining, ShowMore, staleAfterMs, TableScroll, Time, TimeOfDay } from './ui'

// trafficWindow is the smallest traffic window, in seconds, that reaches back
// to the start of the trial; a trial older than an hour is counted over the
// last hour only.
function trafficWindow(activatedAt: string): number {
  const elapsed = (Date.now() - new Date(activatedAt).getTime()) / 1000
  return elapsed <= 60 ? 60 : elapsed <= 300 ? 300 : 3600
}

// sinceStart adds up the requests the gateway decided from the second the
// trial started, from the per-second counts of a traffic reading.
function sinceStart(traffic: Traffic, activatedAt: string) {
  const from = Math.floor(new Date(activatedAt).getTime() / 1000) * 1000
  let allowed = 0
  let refused = 0
  for (const second of traffic.per_second) {
    if (new Date(second.at).getTime() < from) continue
    allowed += second.allowed
    refused += second.refused
  }
  return { allowed, refused, total: allowed + refused }
}

// What the administrator was looking at when they chose Confirm.
interface Seen {
  revision: number
  activatedAt: string
  deadline: string
  generation?: string
  overrides: string
  trafficAt: Date | null
  failed: string[]
}

type Check =
  | { phase: 'reading' }
  | { phase: 'blocked'; why: string }
  | { phase: 'ready'; at: Date; view: ProvisionalView; etag: string | null; traffic?: Traffic; reasons: string[] }

// TrialDecision shows the change on trial (the provisional policy) with the
// server's deadline and its changed controls, and offers confirmation and
// rollback. Only an explicit confirmation makes it permanent; traffic, page
// loads and the countdown never do. The Overview shows it as a tile with
// rollback; Changes shows it in full, with the trial's traffic, and is where
// it is confirmed.
export function TrialDecision(props: { admin: Admin; manage: boolean; variant: 'tile' | 'full' }) {
  const { admin, manage, variant } = props
  const state = useGatewayState()
  const current = useResource<ProvisionalView>((signal) => admin.get('/admin/provisional', signal), [])
  useRereadWhen(state.version, current.refresh)
  // Rereading the status rereads this trial and everything else that
  // depends on it.
  const refresh = state.refresh
  const mutation = useMutation(admin, refresh)
  const [asking, setAsking] = useState<'confirm' | 'rollback' | null>(null)
  const [check, setCheck] = useState<Check>({ phase: 'reading' })
  const [reviewed, setReviewed] = useState(false)
  const seen = useRef<Seen | null>(null)
  // The trial the open confirmation is about. The dialog stays open on it
  // when the trial ends meanwhile, to say why it can no longer be confirmed.
  const confirming = useRef<ProvisionalView | null>(null)
  const checks = useRef(0)

  const none = current.reply?.kind === 'refused' && current.reply.error === 'no_provisional'
  // After a failed reread the last reading stays shown, marked as such.
  const view = none ? undefined : current.last?.body
  // The ETag names the confirmed revision a rollback is based on; the
  // server refuses it if that has changed.
  const etag = current.reply && current.reply.kind !== 'unknown' && current.reply.kind !== 'aborted' ? current.reply.etag : null

  // The trial's traffic, reread every 3 s while it is shown in full.
  const activatedAt = view?.activation.activated_at
  const traffic = useResource<Traffic>(
    (signal) =>
      activatedAt && variant === 'full'
        ? admin.get(`/admin/traffic?window=${trafficWindow(activatedAt)}`, signal)
        : Promise.resolve({ kind: 'aborted' } as Reply<Traffic>),
    [activatedAt, variant],
  )
  usePoll(traffic.refresh, activatedAt && variant === 'full' ? liveMs : null, true)

  // runCheck reads the trial, the status and the trial's traffic again and
  // compares them with what the administrator was looking at. It never
  // sends anything: the administrator decides with the result in front of
  // them.
  async function runCheck(whileOpen: boolean) {
    const before = seen.current
    if (!before) return
    const mine = ++checks.current
    setCheck({ phase: 'reading' })
    setReviewed(false)
    const [trial, status, figures] = await Promise.all([
      admin.get<ProvisionalView>('/admin/provisional'),
      admin.get<Status>('/admin/status'),
      admin.get<Traffic>(`/admin/traffic?window=${trafficWindow(before.activatedAt)}`),
    ])
    if (mine !== checks.current) return
    if (trial.kind === 'refused' && trial.error === 'no_provisional') {
      setCheck({ phase: 'blocked', why: 'No change is on trial any more: it was kept or undone elsewhere, or its deadline passed. Nothing was sent.' })
      state.refresh()
      return
    }
    if (trial.kind !== 'ok') {
      const why = trial.kind === 'unknown' ? trial.message : trial.kind === 'refused' ? `HTTP ${trial.status} ${trial.error}` : 'cancelled'
      setCheck({
        phase: 'blocked',
        why: `The trial could not be read again (${why}), so it cannot be kept from here. Undoing it stays possible, and the gateway undoes it by itself at the deadline.`,
      })
      return
    }
    if (trial.body.activation.revision !== before.revision) {
      setCheck({ phase: 'blocked', why: `A different change is on trial now, version ${trial.body.activation.revision}. Nothing was sent.` })
      state.refresh()
      return
    }
    const reasons: string[] = []
    if (whileOpen) reasons.push("The gateway's status changed while this dialog was open.")
    if (trial.body.activation.deadline !== before.deadline) reasons.push('Its deadline is different from the one you were looking at.')
    if (trial.body.generation !== before.generation) reasons.push('The policy that requests use changed (a new generation is active).')
    if (status.kind === 'ok' && JSON.stringify(status.body.overrides ?? []) !== before.overrides) reasons.push('The active temporary rules changed.')
    if (before.failed.length > 0) reasons.push(`An earlier read of ${before.failed.join(' and ')} had failed, so what you saw may not have been current.`)
    if (status.kind !== 'ok') reasons.push("The gateway's status could not be read again.")
    if (figures.kind !== 'ok') reasons.push("The trial's traffic could not be read again.")
    if (before.trafficAt === null) reasons.push('No traffic figures for the trial were shown when you chose Keep change.')
    else if (Date.now() - before.trafficAt.getTime() > staleAfterMs) reasons.push(`The traffic figures you were looking at were read ${age(before.trafficAt)}.`)
    if (figures.kind === 'ok' && sinceStart(figures.body, before.activatedAt).total === 0) {
      reasons.push('No request has been decided since the trial started, so nothing shows yet how the change behaves.')
    }
    setCheck({ phase: 'ready', at: new Date(), view: trial.body, etag: trial.etag, traffic: figures.kind === 'ok' ? figures.body : undefined, reasons })
  }

  function askConfirm() {
    if (!view) return
    confirming.current = view
    seen.current = {
      revision: view.activation.revision,
      activatedAt: view.activation.activated_at,
      deadline: view.activation.deadline,
      generation: view.generation,
      overrides: JSON.stringify(state.facts?.overrides ?? []),
      trafficAt: traffic.last?.at ?? null,
      failed: [current.failed ? 'the trial' : '', state.failed ? "the gateway's status" : '', traffic.failed ? "the trial's traffic" : ''].filter(Boolean),
    }
    setAsking('confirm')
    void runCheck(false)
  }

  // A status change while the confirmation is open is checked again, and
  // the administrator reviews the result before anything can be sent.
  useEffect(() => {
    if (asking === 'confirm' && check.phase === 'ready') void runCheck(true)
  }, [state.version])

  function cancel() {
    checks.current++
    seen.current = null
    setAsking(null)
  }

  function confirm() {
    if (check.phase !== 'ready' || (check.reasons.length > 0 && !reviewed)) return
    cancel()
    mutation.run({
      method: 'POST',
      path: '/admin/provisional/confirm',
      body: { revision: check.view.activation.revision },
      ifMatch: check.etag ?? undefined,
      key: newKey(),
    })
  }

  function rollback() {
    setAsking(null)
    if (!view) return
    mutation.run({
      method: 'POST',
      path: '/admin/provisional/rollback',
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
            {o.disposition === 'confirmed' ? `Kept: version ${o.revision} is now the active policy.` : `Undone: version ${o.revision} is active again.`}
          </p>
          {variant === 'full' ? <Receipt outcome={o} /> : null}
        </>
      )}
    />
  )

  const ready = check.phase === 'ready' ? check : null
  const asked = confirming.current
  const confirmDialog =
    variant === 'full' && asked ? (
      <Confirm
        open={asking === 'confirm'}
        title={`Keep version ${asked.activation.revision}?`}
        confirmLabel="Keep Change"
        disabled={!ready || (ready.reasons.length > 0 && !reviewed)}
        onCancel={cancel}
        onConfirm={confirm}
      >
        <p>
          Version {asked.activation.revision} stays active and is no longer undone at the deadline. Version {asked.activation.rollback_target} is then no
          longer restored by an undo.
        </p>
        <p className="hint">The Admin API calls this “confirm”.</p>
        {asked.changes.length > 0 ? (
          <div className="change-set">
            <h3>What Stays Active ({asked.changes.length} {asked.changes.length === 1 ? 'change' : 'changes'})</h3>
            <ul>
              {asked.changes.slice(0, 8).map((c) => (
                <li key={`${c.control}:${c.target}`}>
                  {controls[c.control] ?? c.control} <code>{c.target}</code>
                </li>
              ))}
            </ul>
            {asked.changes.length > 8 ? <p className="hint">And {asked.changes.length - 8} more, listed on the trial card.</p> : null}
          </div>
        ) : null}
          <p className="hint" role="status">
            {check.phase === 'reading' ? "Reading the gateway's current state…" : ready ? `Read again at ${clock(ready.at.toISOString(), true)}.` : ''}
          </p>
          {ready ? <CheckedFacts check={ready} /> : null}
          {check.phase === 'blocked' ? <p className="notice tone-refused">{check.why}</p> : null}
          {ready && ready.reasons.length > 0 ? (
            <div className="notice tone-pending">
              <p className="notice-title">Before keeping it, note:</p>
              <ul className="check-reasons">
                {ready.reasons.map((reason) => (
                  <li key={reason}>{reason}</li>
                ))}
              </ul>
              <label className="check">
                <input type="checkbox" checked={reviewed} onChange={(e) => setReviewed(e.target.checked)} />
                <span>I have reviewed the current facts above</span>
              </label>
            </div>
          ) : null}
      </Confirm>
    ) : null
  const dialogs = (
    <>
      {confirmDialog}
      {view ? (
        <Confirm
          open={asking === 'rollback'}
          title={`Undo the change and return to version ${view.activation.rollback_target}?`}
          confirmLabel="Undo Change"
          danger
          onCancel={() => setAsking(null)}
          onConfirm={rollback}
        >
          <p>
            Version {view.activation.rollback_target} is active again from the next request, and the trial of version {view.activation.revision} ends.
          </p>
          <p className="hint">The Admin API calls this “roll back”.</p>
        </Confirm>
      ) : null}
    </>
  )

  const stale = current.failed && !none && view ? (
    <p className="notice tone-pending">
      Not current: this trial was read at {current.last ? <TimeOfDay value={current.last.at.toISOString()} seconds /> : null} and the latest read
      failed.
    </p>
  ) : null

  if (variant === 'tile') {
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
              <a href="#/candidates">Open Changes</a>
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
            <a className="btn btn-small btn-primary" href="#/candidates">
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

  // With no trial nothing is shown, except an open confirmation saying why
  // it can no longer go ahead.
  if (none && mutation.phase.state === 'idle' && asking !== 'confirm') return null
  return (
    <>
      {current.reply && current.reply.kind !== 'ok' && !none && !view ? <Problem reply={current.reply} /> : null}
      {view ? (
        <section className="section card trial tone-pending" aria-labelledby="trial-title">
          <div className="trial-main">
            <p className="tile-label tile-label-tone">Change on Trial</p>
            <h2 id="trial-title">Version {view.activation.revision} is on trial</h2>
            <div className="trial-clock">
              <p className="tile-countdown">
                <Remaining key={view.activation.deadline} until={view.activation.deadline} onPassed={refresh} format="clock" />
              </p>
              <p className="hint">
                left to keep it, until <Time value={view.activation.deadline} />. Then the gateway undoes it and returns to version{' '}
                {view.activation.rollback_target} by itself.
              </p>
            </div>
            <DeadlineBar from={view.activation.activated_at} until={view.activation.deadline} />
            {stale}
            {manage ? (
              <div className="actions">
                <button type="button" className="btn" onClick={() => setAsking('rollback')} disabled={mutation.pending}>
                  Undo Change
                </button>
                <button type="button" className="btn btn-primary" onClick={askConfirm} disabled={mutation.pending}>
                  Keep Change
                </button>
              </div>
            ) : null}
            <TrialTraffic traffic={traffic} activatedAt={view.activation.activated_at} />
          </div>
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
        </section>
      ) : null}
      {result}
      {dialogs}
    </>
  )
}

// TrialTraffic shows how requests have been decided since the trial
// started, so a confirmation rests on what the gateway did, with when that
// was read. These are all requests the gateway decided in that time, from
// its memory of the latest 10,000; they are not a comparison with the
// confirmed policy.
function TrialTraffic(props: { traffic: ReturnType<typeof useResource<Traffic>>; activatedAt: string }) {
  const { traffic, activatedAt } = props
  const last = traffic.last
  return (
    <section className="trial-traffic" aria-labelledby="trial-traffic-title">
      <h3 id="trial-traffic-title">Requests Since the Trial Started</h3>
      {last ? <TrafficFigures traffic={last.body} activatedAt={activatedAt} /> : <p className="hint">{traffic.failed ? 'Not read.' : 'Reading…'}</p>}
      <p className={traffic.failed ? 'hint read-stamp-failed' : 'hint'}>
        {last ? `Updated ${clock(last.at.toISOString(), true)}` : ''}
        {last && Date.now() - last.at.getTime() > staleAfterMs ? ` · ${age(last.at)}` : ''}
        {traffic.failed ? `${last ? ' · ' : ''}latest read failed` : ''}
      </p>
    </section>
  )
}

function TrafficFigures({ traffic, activatedAt }: { traffic: Traffic; activatedAt: string }) {
  const counts = sinceStart(traffic, activatedAt)
  const windowWords = traffic.window_seconds === 3600 ? 'last hour' : `last ${traffic.window_seconds / 60 >= 1 ? `${traffic.window_seconds / 60} min` : `${traffic.window_seconds} s`}`
  if (counts.total === 0) {
    return <p className="hint">No request has been decided since the trial started, so nothing shows yet how the change behaves.</p>
  }
  return (
    <>
      <p className="trial-counts">
        <strong>{counts.total.toLocaleString()}</strong> decided · {counts.allowed.toLocaleString()} allowed · {counts.refused.toLocaleString()} refused
        {traffic.window_seconds === 3600 && Date.now() - new Date(activatedAt).getTime() > 3_600_000 ? ' (last hour only)' : ''}
      </p>
      {traffic.refusal_reasons.length > 0 ? (
        <p className="hint">
          Refused in the {windowWords}: {traffic.refusal_reasons.map((r) => `${reasonOf(r.code)} ${r.count}`).join(' · ')}
        </p>
      ) : null}
    </>
  )
}

// CheckedFacts lists what the confirmation's fresh reads found.
function CheckedFacts({ check }: { check: Extract<Check, { phase: 'ready' }> }) {
  const counts = check.traffic ? sinceStart(check.traffic, check.view.activation.activated_at) : null
  return (
    <dl className="facts facts-compact">
      <div className="fact">
        <dt>Deadline</dt>
        <dd>
          <Time value={check.view.activation.deadline} />
        </dd>
      </div>
      <div className="fact">
        <dt>Requests Since It Started</dt>
        <dd>{counts ? `${counts.total} decided: ${counts.allowed} allowed, ${counts.refused} refused` : 'not read'}</dd>
      </div>
    </dl>
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
