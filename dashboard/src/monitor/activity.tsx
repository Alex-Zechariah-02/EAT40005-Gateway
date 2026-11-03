import { useState } from 'react'
import type { Admin } from '../shared/http'
import type { HistoryEntry, Page } from '../shared/api'
import { useGatewayState, liveMs, usePoll, useRereadWhen, useResource } from '../shared/hooks'
import { actorName, Problem, ReplyView } from '../shared/replies'
import { TableScroll, type Tone } from '../shared/ui'
import { Time } from '../shared/time'
import { EventRows, type RequestEvent } from './requests'
import { type Traffic, TrafficCards } from './summary'
import { lookupRef } from './lookup'
import { apiName } from '../policy/catalog'

// The Overview's live half: traffic, latest refusals, API figures and policy timeline.

// OverviewLive is the Overview's live half: traffic in a chosen window, the
// latest refusals, each API's figures and the policy timeline.
export function OverviewLive({ admin, history }: { admin: Admin; history: boolean }) {
  const state = useGatewayState()
  const [span, setSpan] = useState(300)
  const traffic = useResource<Traffic>((signal) => admin.get(`/admin/traffic?window=${span}`, signal), [span])
  const refusals = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?decision=refused&limit=5', signal), [])
  usePoll(
    () => {
      traffic.refresh()
      refusals.refresh()
    },
    liveMs,
    state.critical,
  )
  const t = traffic.last?.body
  // Recent refusals show only when there are some: the Refusals card already
  // says when nothing was refused.
  const recent = refusals.reply?.kind === 'ok' ? refusals.reply.body.items : undefined
  return (
    <>
      {traffic.reply && traffic.reply.kind !== 'ok' ? <Problem reply={traffic.reply} /> : null}
      {refusals.reply && refusals.reply.kind !== 'ok' ? <Problem reply={refusals.reply} /> : null}
      <TrafficCards
        resource={traffic}
        span={span}
        onSpan={setSpan}
        main={
          <>
            {recent?.length ? (
              <section className="card" aria-labelledby="refusals-title">
                <div className="card-head card-head-pad">
                  <h2 id="refusals-title">Recent Refusals</h2>
                  <a className="card-link" href="#/live-traffic">
                    Open Live Traffic
                  </a>
                </div>
                <div className="card-body">
                  <EventRows events={recent} open={lookupRef} label="Recent Refusals" />
                </div>
              </section>
            ) : null}
            <section className="card" aria-labelledby="apis-title">
              <div className="card-head card-head-pad">
                <h2 id="apis-title">APIs</h2>
                <a className="card-link" href="#/apis">
                  Open APIs
                </a>
              </div>
              <div className="card-body">{t ? <ApiFigures traffic={t} /> : <p className="hint card-empty">No traffic figures.</p>}</div>
            </section>
          </>
        }
        side={history ? <PolicyTimeline admin={admin} /> : null}
      />
    </>
  )
}

function ApiFigures({ traffic }: { traffic: Traffic }) {
  if (traffic.by_api.length === 0) return <p className="hint card-empty">No API has had a request in this window.</p>
  return (
    <TableScroll label="Requests per API">
      <table>
        <thead>
          <tr>
            <th scope="col">API</th>
            <th scope="col" className="num end">
              Requests
            </th>
            <th scope="col" className="num end">
              Refused
            </th>
            <th scope="col" className="num end">
              Typical Time
            </th>
            <th scope="col" className="num end">
              Slowest 5%
            </th>
          </tr>
        </thead>
        <tbody>
          {traffic.by_api.map((a) => (
            <tr key={a.api}>
              <td>
                <a href="#/apis" className="quiet-link">
                  {apiName(a.api)}
                </a>
              </td>
              <td className="num end">{a.total.toLocaleString()}</td>
              <td className="num end">
                {a.refused.toLocaleString()}{' '}
                <span className="muted">
                  ({a.total ? ((a.refused / a.total) * 100).toFixed(1) : '0.0'}
                  %)
                </span>
              </td>
              <td className="num end">{a.p50_ms.toFixed(1)} ms</td>
              <td className="num end">{a.p95_ms.toFixed(1)} ms</td>
            </tr>
          ))}
        </tbody>
      </table>
    </TableScroll>
  )
}

// Each kind of policy event: what to call its subject and what happened.
// A revision is named by its number and an account by its ID. An undo
// records the revision it restores, so its words say that one is back.
const timelineKinds: Record<string, { name: string; word: string; tone: Tone }> = {
  genesis: { name: 'revision', word: 'First Policy', tone: 'ok' },
  recovery_base: { name: 'revision', word: 'Starting Policy After a Restore', tone: 'pending' },
  'cpa.activate': { name: 'revision', word: 'Put on Trial', tone: 'pending' },
  'cpa.confirm': { name: 'revision', word: 'Kept', tone: 'ok' },
  'cpa.rollback': { name: 'revision', word: 'Active Again After an Undo', tone: 'neutral' },
  'cpa.timeout': { name: 'revision', word: 'Active Again at the Deadline', tone: 'neutral' },
  'tpo.create': { name: 'Temporary rule', word: 'Added', tone: 'pending' },
  'tpo.revoke': { name: 'Temporary rule', word: 'Revoked', tone: 'neutral' },
  'tpo.expire': { name: 'Temporary rule', word: 'Ended', tone: 'neutral' },
  'account.block': { name: 'account', word: 'Blocked', tone: 'refused' },
  'account.unblock': { name: 'account', word: 'Unblocked', tone: 'ok' },
  'keys.reload': { name: 'Signing keys', word: 'Reloaded', tone: 'neutral' },
}

function timelineName(entry: HistoryEntry, kind: { name: string }): string {
  if (kind.name === 'revision') return entry.revision ? `Version ${entry.revision}` : 'Policy'
  if (kind.name === 'account') return entry.target || 'Account'
  return kind.name
}

// The timeline is one read: the gateway lists the newest committed entries
// of the timeline's kinds itself, however long the history is.
const timelinePath = `/admin/history?order=desc&outcome=committed&limit=6&kind=${Object.keys(timelineKinds).join(',')}`

// PolicyTimeline lists the latest policy events from the administrative
// history, newest first: trials, confirmations, rollbacks, temporary
// rules and blocks.
function PolicyTimeline({ admin }: { admin: Admin }) {
  // It rereads when the gateway's status changes or someone asks for a
  // reread (each change made and each deadline shown), not at every poll.
  const state = useGatewayState()
  const history = useResource<HistoryEntry[]>(async (signal) => {
    const reply = await admin.get<Page<HistoryEntry>>(timelinePath, signal)
    return reply.kind === 'ok' ? { ...reply, body: reply.body.items } : reply
  }, [])
  useRereadWhen(state.version, history.refresh)
  return (
    <section className="card card-pad" aria-labelledby="timeline-title">
      <div className="card-head">
        <h2 id="timeline-title">Policy Timeline</h2>
        <a className="card-link" href="#/history">
          Full History
        </a>
      </div>
      <ReplyView reply={history.reply}>
        {(items) => {
          if (items.length === 0) return <p className="hint">No policy changes recorded yet.</p>
          return (
            <ol className="timeline">
              {items.map((e) => {
                const kind = timelineKinds[e.kind]
                return (
                  <li key={e.seq} className={`timeline-item tone-${kind.tone}`}>
                    <span className="timeline-dot" aria-hidden="true" />
                    <div className="timeline-text">
                      <p className="timeline-title">
                        <span>
                          <strong>{timelineName(e, kind)}</strong> <span className="timeline-word">{kind.word}</span>
                        </span>
                        <Time value={e.recorded_at} />
                      </p>
                      <p className="timeline-note">by {actorName(e.actor)}</p>
                    </div>
                  </li>
                )
              })}
            </ol>
          )
        }}
      </ReplyView>
    </section>
  )
}
