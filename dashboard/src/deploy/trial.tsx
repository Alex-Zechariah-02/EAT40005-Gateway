import type { ProvisionalView } from '../shared/api'
import { useResource } from '../shared/hooks'
import { reasonOf } from '../monitor/requests'
import type { Traffic } from '../monitor/summary'
import { age, clock, staleAfterMs, Time } from '../shared/time'
import { Confirm } from '../shared/dialogs'
import { controls } from './activation'

// The evidence for keeping a trial: its traffic since the start, and the fresh check in the keep dialog.

// trafficWindow is the smallest traffic window, in seconds, that reaches back
// to the start of the trial; a trial older than an hour is counted over the
// last hour only.
export function trafficWindow(activatedAt: string): number {
  const elapsed = (Date.now() - new Date(activatedAt).getTime()) / 1000
  return elapsed <= 60 ? 60 : elapsed <= 300 ? 300 : 3600
}

// sinceStart adds up the requests the gateway decided from the second the
// trial started, from the per-second counts of a traffic reading.
export function sinceStart(traffic: Traffic, activatedAt: string) {
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
// The dialog open on the trial, if any.
export type Asking = 'confirm' | 'rollback' | null

export type Check =
  | { phase: 'reading' }
  | { phase: 'blocked'; why: string }
  | { phase: 'ready'; at: Date; view: ProvisionalView; etag: string | null; traffic?: Traffic; reasons: string[] }

// TrialTraffic shows how requests have been decided since the trial
// started, so a confirmation rests on what the gateway did, with when that
// was read. These are all requests the gateway decided in that time, from
// its memory of the latest 10,000; they are not a comparison with the
// confirmed policy.
export function TrialTraffic(props: { traffic: ReturnType<typeof useResource<Traffic>>; activatedAt: string }) {
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

// KeepDialog asks to keep the change on trial, with what the fresh reads found
// and anything the administrator must review before it can be sent.
export function KeepDialog(props: {
  asked: ProvisionalView
  asking: Asking
  check: Check
  ready: Extract<Check, { phase: 'ready' }> | null
  reviewed: boolean
  setReviewed: (value: boolean) => void
  cancel: () => void
  confirm: () => void
}) {
  const { asked, asking, check, ready, reviewed, setReviewed, cancel, confirm } = props
  return (
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
  )
}
