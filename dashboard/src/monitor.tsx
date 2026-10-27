import { useEffect, useId, useMemo, useRef, useState, type FormEvent } from 'react'
import { newKey, type Admin, type HistoryEntry, type Page, type Policy, type Reply, type Revision } from './api'
import { useGatewayState, useMutation, useResource, type Resource } from './hooks'
import { Badge, Confirm, Empty, Field, MutationResult, PageHeader, Problem, ReadStamp, ReplyView, TableScroll, Time, TimeOfDay, type Tone } from './ui'
import type { ViewProps } from './views'

// The Monitor and Admin pages: live traffic, request lookup, APIs, accounts,
// team and policy as code, reading the gateway's Admin API.
// Everything shown comes from the gateway; nothing here decides or evaluates
// policy.

export type Stage = 'route' | 'access' | 'token' | 'blocked' | 'permit' | 'rate' | 'validation' | 'record' | 'forward'

export interface RequestEvent {
  request_id: string
  time: string
  method: string
  path: string
  operation: string | null
  api: string | null
  subject: string | null
  roles: string[]
  client_address: string
  decision: 'allowed' | 'refused' | 'failed'
  status: number
  stage: Stage
  code: string | null
  generation: string
  duration_ms: number
}

export interface Traffic {
  window_seconds: number
  total: number
  allowed: number
  refused: number
  by_api: { api: string; total: number; allowed: number; refused: number; p50_ms: number; p95_ms: number }[]
  refusal_reasons: { code: string; count: number }[]
  per_second: { at: string; allowed: number; refused: number }[]
}

// requests = previewed + identity_unknown. A request whose sign-in was never
// checked (refused before the token check, or a public operation) cannot be
// replayed under a policy that would check it; the gateway counts it apart.
interface Impact {
  window: { from: string; to: string; requests: number; previewed: number; identity_unknown: number }
  flips: { operation: string; subject: string | null; roles: string[]; before: string; after: string; after_code: string | null; count: number }[]
  not_previewed: string[]
}

interface BlockedAccount {
  subject: string
  reason: string
  blocked_by: string
  blocked_at: string
}
interface Administrator {
  name: string
  permissions: string[]
  status: string
}
interface SigningKey {
  kid: string
  fingerprint: string
  loaded_at: string
}

// The gateway's checks in the order it applies them; plain names first.
export const stages: { id: Stage; name: string }[] = [
  { id: 'route', name: 'Route' },
  { id: 'access', name: 'Access' },
  { id: 'token', name: 'Sign-in' },
  { id: 'blocked', name: 'Blocked list' },
  { id: 'permit', name: 'Role' },
  { id: 'rate', name: 'Rate' },
  { id: 'validation', name: 'Body' },
  { id: 'record', name: 'Record' },
  { id: 'forward', name: 'Forward' },
]

// Plain words for the gateway's refusal codes; the code stays one
// hover away.
export const reasons: Record<string, string> = {
  bad_request: 'Request refused by validation',
  unauthenticated: 'Not signed in or token not accepted',
  forbidden: 'No permission',
  account_blocked: 'Account blocked',
  not_found: 'No such route',
  method_not_allowed: 'Method not allowed',
  payload_too_large: 'Body too large',
  unsupported_media_type: 'Body not JSON',
  rate_limited: 'Too many requests',
  upstream_unavailable: 'Service unreachable',
  gateway_unavailable: 'Gateway could not record or decide',
  upstream_timeout: 'Service too slow; outcome unknown',
}

export function reasonOf(code: string | null): string {
  return code ? (reasons[code] ?? code) : 'Allowed'
}

const decisionTone: Record<RequestEvent['decision'], Tone> = { allowed: 'ok', refused: 'refused', failed: 'unknown' }
const decisionWord: Record<RequestEvent['decision'], string> = { allowed: 'Allowed', refused: 'Refused', failed: 'Unknown' }

// usePoll rereads a resource every ms while the page is visible.
function usePoll(refresh: () => void, ms: number) {
  const latest = useRef(refresh)
  latest.current = refresh
  useEffect(() => {
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') latest.current()
    }, ms)
    return () => clearInterval(timer)
  }, [ms])
}

type StepState = 'cleared' | 'refused' | 'unknown' | 'skipped' | 'unreached'
const stepWords: Record<StepState, string> = { cleared: 'passed', refused: 'refused here', unknown: 'outcome unknown', skipped: 'not checked', unreached: 'not reached' }

// beforeRouting says the gateway decided a request before matching it to an
// operation: it was refused on its form, or no policy could be captured (a
// gateway that admits nothing). The event then names a later check, but none
// of the checks ahead of it ran.
function beforeRouting(event: Pick<RequestEvent, 'stage' | 'decision' | 'operation'>): boolean {
  return event.decision !== 'allowed' && event.operation === null && event.stage !== 'route'
}

// pathStates says what happened at each of the gateway's checks: those ahead
// of the deciding check passed, unless the request was decided before
// routing; those after it were never reached.
function pathStates(event: Pick<RequestEvent, 'stage' | 'decision' | 'operation'>): StepState[] {
  const stop = stages.findIndex((s) => s.id === event.stage)
  const early = beforeRouting(event)
  return stages.map((_, n) => {
    if (event.decision === 'allowed') return 'cleared'
    if (n === stop) return event.decision === 'failed' ? 'unknown' : 'refused'
    if (n > stop) return 'unreached'
    return early ? 'skipped' : 'cleared'
  })
}

// DecisionPath shows the checks a request cleared, the one that decided it
// and those it never reached.
export function DecisionPath({ event }: { event: Pick<RequestEvent, 'stage' | 'decision' | 'operation'> }) {
  const states = pathStates(event)
  const allowed = event.decision === 'allowed'
  const early = beforeRouting(event)
  const name = stages.find((s) => s.id === event.stage)?.name ?? event.stage
  const cleared = states.filter((s) => s === 'cleared').length
  return (
    <span className="decision-path">
      <span
        className="dp-steps"
        role="img"
        aria-label={allowed ? 'Cleared every check' : early ? `Decided before routing, at ${name}` : `Cleared ${cleared} checks, decided at ${name}`}
      >
        {stages.map((s, n) => (
          <span key={s.id} className={`dp-step dp-${states[n] === 'skipped' ? 'unreached' : states[n]}`} title={`${s.name}: ${stepWords[states[n]]}`} />
        ))}
      </span>
      <span className={`dp-label dp-label-${event.decision}`}>{allowed ? 'Forwarded' : early ? 'Before routing' : name}</span>
    </span>
  )
}

// intervals spreads the gateway's per-second counts, which list only the
// seconds that saw requests, over the whole window ending at end: 60 equal
// intervals, the quiet ones empty.
function intervals(traffic: Traffic, end: number) {
  const count = 60
  const step = (traffic.window_seconds * 1000) / count
  const start = end - traffic.window_seconds * 1000
  const bars = Array.from({ length: count }, (_, n) => ({
    at: new Date(start + n * step).toISOString(),
    to: new Date(start + (n + 1) * step).toISOString(),
    allowed: 0,
    refused: 0,
  }))
  for (const p of traffic.per_second) {
    const n = Math.min(count - 1, Math.max(0, Math.floor((Date.parse(p.at) - start) / step)))
    bars[n].allowed += p.allowed
    bars[n].refused += p.refused
  }
  return bars
}

// TrafficChart draws requests per interval: allowed in grey, refused in red.
// The grey marks sit below 3:1 against the card, so every value is also in the
// readout and the table view.
export function TrafficChart({ traffic, end }: { traffic: Traffic; end: number }) {
  const [active, setActive] = useState<number | null>(null)
  const id = useId()
  const bars = useMemo(() => intervals(traffic, end), [traffic, end])
  const points = traffic.per_second
  if (points.length === 0) return <Empty title="No requests in this window yet." />
  const max = Math.max(1, ...bars.map((p) => p.allowed + p.refused))
  const width = 600
  const height = 140
  const band = width / bars.length
  const bar = Math.max(1.5, Math.min(10, band * 0.78))
  const shown = active !== null ? bars[active] : null
  return (
    <figure className="traffic-chart">
      <p className="chart-readout" aria-live="polite">
        {shown ? (
          <>
            <TimeOfDay value={shown.at} seconds /> to <TimeOfDay value={shown.to} seconds /> · {shown.allowed} allowed · {shown.refused} refused
          </>
        ) : (
          <span className="muted">Point at a bar for its values</span>
        )}
      </p>
      <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" role="img" aria-labelledby={`${id}-t`} onMouseLeave={() => setActive(null)}>
        <title id={`${id}-t`}>Requests allowed and refused over the window</title>
        {bars.map((p, n) => {
          const total = ((p.allowed + p.refused) / max) * (height - 4)
          const refused = (p.refused / max) * (height - 4)
          // A 2px surface gap separates the stacked segments.
          const gap = p.refused > 0 && p.allowed > 0 ? 2 : 0
          const x = n * band + (band - bar) / 2
          return (
            <g key={p.at} onMouseEnter={() => setActive(n)}>
              <rect x={n * band} y={0} width={band} height={height} className={active === n ? 'chart-hover' : 'chart-hit'} />
              <rect x={x} y={height - total} width={bar} height={Math.max(0, total - refused - gap)} rx={1.5} className="chart-allowed" />
              <rect x={x} y={height - refused} width={bar} height={refused} rx={1.5} className="chart-refused" />
            </g>
          )
        })}
      </svg>
      <figcaption className="chart-axis">
        <TimeOfDay value={bars[0].at} />
        <TimeOfDay value={bars[bars.length / 2].at} />
        <span>now</span>
      </figcaption>
      <details className="chart-table">
        <summary>Show as a table</summary>
        <TableScroll label="Seconds with requests">
          <table>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col" className="num end">
                  Allowed
                </th>
                <th scope="col" className="num end">
                  Refused
                </th>
              </tr>
            </thead>
            <tbody>
              {points.map((p) => (
                <tr key={p.at}>
                  <td>
                    <TimeOfDay value={p.at} seconds />
                  </td>
                  <td className="num end">{p.allowed}</td>
                  <td className="num end">{p.refused}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      </details>
    </figure>
  )
}

function Figures({ traffic }: { traffic: Traffic }) {
  const share = traffic.total ? ((traffic.refused / traffic.total) * 100).toFixed(1) : '0.0'
  return (
    <dl className="figures">
      <div>
        <dt>Requests</dt>
        <dd>{traffic.total.toLocaleString()}</dd>
      </div>
      <div>
        <dt>
          <span className="key key-allowed" aria-hidden="true" />
          Allowed
        </dt>
        <dd>{traffic.allowed.toLocaleString()}</dd>
      </div>
      <div>
        <dt>
          <span className="key key-refused" aria-hidden="true" />
          Refused
        </dt>
        <dd>
          {traffic.refused.toLocaleString()} <span className="figure-share">{share}%</span>
        </dd>
      </div>
    </dl>
  )
}

function Reasons({ traffic }: { traffic: Traffic }) {
  const top = Math.max(1, ...traffic.refusal_reasons.map((r) => r.count))
  if (traffic.refusal_reasons.length === 0) return <p className="hint">Nothing was refused in this window.</p>
  return (
    <ul className="reasons">
      {traffic.refusal_reasons.map((r) => (
        <li key={r.code}>
          <span className="reason-name" title={r.code}>
            {reasonOf(r.code)}
          </span>
          <span className="num">{r.count}</span>
          <svg className="reason-bar" viewBox="0 0 100 4" preserveAspectRatio="none" aria-hidden="true">
            <rect width="100" height="4" rx="2" className="reason-track" />
            <rect width={(r.count / top) * 100} height="4" rx="2" className="reason-fill" />
          </svg>
        </li>
      ))}
    </ul>
  )
}

const windows = [
  { seconds: 60, label: '1 min' },
  { seconds: 300, label: '5 min' },
  { seconds: 3600, label: '1 hour' },
]

function WindowSwitch({ value, onChange }: { value: number; onChange: (seconds: number) => void }) {
  return (
    <div className="segmented" role="group" aria-label="Time window">
      {windows.map((w) => (
        <button key={w.seconds} type="button" aria-pressed={value === w.seconds} onClick={() => onChange(w.seconds)}>
          {w.label}
        </button>
      ))}
    </div>
  )
}

// windowEnd is when a traffic reading's window ends: the gateway's clock when
// it answered, or the browser's when the reading arrived if no date came.
function windowEnd(resource: Resource<Traffic>): number {
  if (resource.reply?.kind === 'ok' && resource.reply.date !== undefined) return resource.reply.date
  return resource.readAt?.getTime() ?? Date.now()
}

function TrafficCards({ traffic, end, span, onSpan }: { traffic: Traffic; end: number; span: number; onSpan: (seconds: number) => void }) {
  return (
    <div className="section grid-main-side">
      <section className="card card-pad" aria-labelledby="traffic-title">
        <div className="card-head">
          <h2 id="traffic-title">Traffic</h2>
          <WindowSwitch value={span} onChange={onSpan} />
        </div>
        <Figures traffic={traffic} />
        <TrafficChart traffic={traffic} end={end} />
      </section>
      <section className="card card-pad" aria-labelledby="reasons-title">
        <div className="card-head">
          <h2 id="reasons-title">Why requests were refused</h2>
        </div>
        <Reasons traffic={traffic} />
      </section>
    </div>
  )
}

function EventRows({ events, open, label }: { events: RequestEvent[]; open: (ref: string) => void; label: string }) {
  return (
    <TableScroll label={label}>
      <table className="events">
        <thead>
          <tr>
            <th scope="col">Time</th>
            <th scope="col">Request</th>
            <th scope="col">Account</th>
            <th scope="col">What happened</th>
            <th scope="col">Decided at</th>
            <th scope="col" className="end">
              Ref
            </th>
          </tr>
        </thead>
        <tbody>
          {events.map((e) => (
            <tr key={e.request_id}>
              <td className="num nowrap muted">
                <TimeOfDay value={e.time} seconds />
              </td>
              <td className="nowrap">
                <code>
                  <span className="method">{e.method}</span> {e.path}
                </code>
              </td>
              <td className="nowrap">
                <code className="soft">{e.subject ?? e.client_address}</code>
              </td>
              <td>
                {e.decision === 'allowed' ? (
                  <span className="muted">
                    Allowed in {e.duration_ms.toFixed(1)} ms
                  </span>
                ) : (
                  <span title={e.code ?? undefined}>{e.decision === 'failed' ? `${reasonOf(e.code)}` : reasonOf(e.code)}</span>
                )}{' '}
                <span className="status-chip">{e.status}</span>
              </td>
              <td>
                <DecisionPath event={e} />
              </td>
              <td className="nowrap end">
                <button type="button" className="link-button" onClick={() => open(e.request_id)} title="Open in Request lookup">
                  <code>{e.request_id.slice(0, 8)}</code>
                  <span className="visually-hidden"> (open request {e.request_id})</span>
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </TableScroll>
  )
}

// lookupRef opens Request lookup for one reference.
function lookupRef(ref: string) {
  window.location.hash = `#/lookup?ref=${encodeURIComponent(ref)}`
}

// OverviewLive is the Overview's live half: traffic in a chosen window, the
// latest refusals, each API's figures and the policy timeline.
export function OverviewLive({ admin, history }: { admin: Admin; history: boolean }) {
  const [span, setSpan] = useState(300)
  const traffic = useResource<Traffic>((signal) => admin.get(`/admin/traffic?window=${span}`, signal), [span])
  const refusals = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?decision=refused&limit=5', signal), [])
  usePoll(() => {
    traffic.refresh()
    refusals.refresh()
  }, 10_000)
  const t = traffic.reply?.kind === 'ok' ? traffic.reply.body : undefined
  return (
    <>
      {traffic.reply && traffic.reply.kind !== 'ok' ? <Problem reply={traffic.reply} /> : null}
      {t ? <TrafficCards traffic={t} end={windowEnd(traffic)} span={span} onSpan={setSpan} /> : null}
      <section className="section card" aria-labelledby="refusals-title">
        <div className="card-head card-head-pad">
          <h2 id="refusals-title">Recent refusals</h2>
          <a className="card-link" href="#/traffic">
            Open live traffic
          </a>
        </div>
        <div className="card-body">
          <ReplyView reply={refusals.reply}>
            {(r) => (r.items.length ? <EventRows events={r.items} open={lookupRef} label="Recent refusals" /> : <p className="hint card-empty">Nothing refused recently.</p>)}
          </ReplyView>
        </div>
      </section>
      <div className={history ? 'section grid-main-side grid-top' : 'section'}>
        <section className="card" aria-labelledby="apis-title">
          <div className="card-head card-head-pad">
            <h2 id="apis-title">APIs</h2>
            <a className="card-link" href="#/apis">
              Open APIs
            </a>
          </div>
          <div className="card-body">{t ? <ApiFigures traffic={t} /> : <p className="hint card-empty">No traffic figures.</p>}</div>
        </section>
        {history ? <PolicyTimeline admin={admin} /> : null}
      </div>
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
              Typical time
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
                {a.refused.toLocaleString()} <span className="muted">({a.total ? ((a.refused / a.total) * 100).toFixed(1) : '0.0'}%)</span>
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

// apiName names an API; the gateway counts requests that matched no route
// under "unknown".
function apiName(id: string): string {
  if (id === 'unknown') return 'No route matched'
  return id.slice(0, 1).toUpperCase() + id.slice(1)
}

// Each kind of policy event: what to call its subject and what happened.
// A revision is named by its number and an account by its ID.
const timelineKinds: Record<string, { name: string; word: string; tone: Tone }> = {
  genesis: { name: 'revision', word: 'First policy', tone: 'ok' },
  recovery_base: { name: 'revision', word: 'Recovery base', tone: 'pending' },
  'cpa.activate': { name: 'revision', word: 'Put on trial', tone: 'pending' },
  'cpa.confirm': { name: 'revision', word: 'Confirmed', tone: 'ok' },
  'cpa.rollback': { name: 'revision', word: 'Rolled back', tone: 'neutral' },
  'cpa.timeout': { name: 'revision', word: 'Rolled back at its deadline', tone: 'neutral' },
  'tpo.create': { name: 'Temporary rule', word: 'Added', tone: 'pending' },
  'tpo.revoke': { name: 'Temporary rule', word: 'Revoked', tone: 'neutral' },
  'tpo.expire': { name: 'Temporary rule', word: 'Ended', tone: 'neutral' },
  'account.block': { name: 'account', word: 'Blocked', tone: 'refused' },
  'account.unblock': { name: 'account', word: 'Unblocked', tone: 'ok' },
  'keys.reload': { name: 'Signing keys', word: 'Reloaded', tone: 'neutral' },
}

function timelineName(entry: HistoryEntry, kind: { name: string }): string {
  if (kind.name === 'revision') return entry.revision ? `Revision ${entry.revision}` : 'Policy'
  if (kind.name === 'account') return entry.target || 'Account'
  return kind.name
}

// The gateway lists its history oldest first, so the timeline reads to the
// end (at most 20 pages of 100) and keeps the newest entries.
async function newestHistory(admin: Admin, signal: AbortSignal): Promise<Reply<HistoryEntry[]>> {
  let items: HistoryEntry[] = []
  let after = ''
  for (let page = 0; page < 20; page++) {
    const reply = await admin.get<Page<HistoryEntry>>(`/admin/history?limit=100${after ? `&after=${encodeURIComponent(after)}` : ''}`, signal)
    if (reply.kind !== 'ok') return reply
    items = [...items, ...reply.body.items].slice(-200)
    if (!reply.body.next) break
    after = reply.body.next
  }
  return { kind: 'ok', status: 200, body: items, etag: null }
}

// PolicyTimeline lists the latest policy events from the administrative
// history, newest first: trials, confirmations, rollbacks, temporary
// rules and blocks.
function PolicyTimeline({ admin }: { admin: Admin }) {
  // It rereads whenever the shell rereads the gateway's status (each change
  // of view, each change made and each deadline shown), not on a timer: only
  // traffic figures refresh by themselves.
  const state = useGatewayState()
  const history = useResource<HistoryEntry[]>((signal) => newestHistory(admin, signal), [state.readAt])
  return (
    <section className="card card-pad" aria-labelledby="timeline-title">
      <div className="card-head">
        <h2 id="timeline-title">Policy timeline</h2>
        <a className="card-link" href="#/history">
          Full history
        </a>
      </div>
      <ReplyView reply={history.reply}>
        {(entries) => {
          const items = entries
            .filter((e) => timelineKinds[e.kind] && e.outcome === 'committed')
            .reverse()
            .slice(0, 6)
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
                        <TimeOfDay value={e.recorded_at} />
                      </p>
                      <p className="timeline-note">by {e.actor}</p>
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

// LiveTraffic streams request events as they happen and summarises a
// chosen window. Pause keeps the list still for inspection; events that
// arrive meanwhile are counted, not lost from the gateway's record.
// The page keeps more requests than it draws, so a filter can still find
// writes made minutes ago between the boards' frequent reads.
const keptEvents = 1000
const shownEvents = 200
const reads = new Set(['GET', 'HEAD', 'OPTIONS'])

export function LiveTraffic({ admin }: ViewProps) {
  const [span, setSpan] = useState(60)
  const traffic = useResource<Traffic>((signal) => admin.get(`/admin/traffic?window=${span}`, signal), [span])
  usePoll(traffic.refresh, 5_000)
  const [events, setEvents] = useState<RequestEvent[]>([])
  const [paused, setPaused] = useState(false)
  const [held, setHeld] = useState(0)
  const [dropped, setDropped] = useState(0)
  const [stream, setStream] = useState<'connecting' | 'live' | 'closed'>('connecting')
  const [only, setOnly] = useState<'all' | 'changes' | 'refused'>('all')
  const pausedRef = useRef(paused)
  pausedRef.current = paused
  // Requests that arrive while paused wait here, newest first, and join the
  // list on Resume.
  const waiting = useRef<RequestEvent[]>([])

  useEffect(() => {
    const source = new EventSource('/admin/events/stream')
    source.onopen = () => setStream('live')
    source.onerror = () => setStream(source.readyState === EventSource.CLOSED ? 'closed' : 'connecting')
    source.addEventListener('request', (message) => {
      const event = JSON.parse((message as MessageEvent<string>).data) as RequestEvent
      if (pausedRef.current) {
        waiting.current = [event, ...waiting.current].slice(0, keptEvents)
        setHeld((n) => n + 1)
      } else setEvents((list) => [event, ...list].slice(0, keptEvents))
    })
    source.addEventListener('dropped', (message) => {
      const { count } = JSON.parse((message as MessageEvent<string>).data) as { count: number }
      setDropped((n) => n + count)
    })
    return () => source.close()
  }, [])

  // The list starts with the latest recorded requests; the stream adds newer
  // ones on top. A request in both appears once.
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?limit=50', signal), [])
  const recentItems = recent.reply?.kind === 'ok' ? recent.reply.body.items : undefined
  const all = useMemo(() => {
    const live = new Set(events.map((e) => e.request_id))
    return [...events, ...(recentItems ?? []).filter((e) => !live.has(e.request_id))]
  }, [events, recentItems])
  const matching = useMemo(
    () => (only === 'refused' ? all.filter((e) => e.decision !== 'allowed') : only === 'changes' ? all.filter((e) => !reads.has(e.method)) : all),
    [all, only],
  )
  const shown = matching.slice(0, shownEvents)
  return (
    <>
      <PageHeader title="Live traffic" lede="Every request the gateway decides, as it happens, with the check that decided it. Pausing only stops this list; the gateway records every request." />
      {traffic.reply && traffic.reply.kind !== 'ok' ? <Problem reply={traffic.reply} /> : null}
      {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
      {traffic.reply?.kind === 'ok' ? <TrafficCards traffic={traffic.reply.body} end={windowEnd(traffic)} span={span} onSpan={setSpan} /> : null}
      <section className="section card" aria-labelledby="stream-title">
        <div className="card-head card-head-pad">
          <h2 id="stream-title">
            Requests as they happen{' '}
            <span className={`live-state live-${stream}`} role="status">
              {stream === 'live' ? (paused ? 'Paused' : 'Live') : stream === 'closed' ? 'Stream closed' : 'Connecting…'}
            </span>
          </h2>
          <div className="card-tools">
            <div className="segmented" role="group" aria-label="Show">
              <button type="button" aria-pressed={only === 'all'} onClick={() => setOnly('all')}>
                All
              </button>
              <button type="button" aria-pressed={only === 'changes'} onClick={() => setOnly('changes')} title="Requests that may change something; reads are hidden">
                Changes only
              </button>
              <button type="button" aria-pressed={only === 'refused'} onClick={() => setOnly('refused')}>
                Refused only
              </button>
            </div>
            <button
              type="button"
              className="btn btn-small"
              aria-pressed={paused}
              onClick={() => {
                if (paused) {
                  const arrived = waiting.current
                  waiting.current = []
                  setEvents((list) => [...arrived, ...list].slice(0, keptEvents))
                }
                setPaused(!paused)
                setHeld(0)
              }}
            >
              {paused ? `Resume${held ? ` (${held} new)` : ''}` : 'Pause'}
            </button>
          </div>
        </div>
        {dropped > 0 ? (
          <p className="notice tone-pending card-notice">This view fell behind and missed {dropped} events. The gateway's security log still has every request.</p>
        ) : null}
        <div className="card-body">
          {shown.length ? (
            <>
              <EventRows events={shown} open={lookupRef} label="Requests as they happen" />
              {matching.length > shown.length ? (
                <p className="hint card-empty">
                  Showing the newest {shown.length} of {matching.length.toLocaleString()}. Request lookup finds any request by its Ref.
                </p>
              ) : null}
            </>
          ) : (
            <p className="hint card-empty">
              {stream === 'closed'
                ? 'The stream closed. Reload the page to reconnect.'
                : all.length
                  ? 'No request of this kind yet.'
                  : 'Waiting for requests. Use the Fetch app and they appear here within a second.'}
            </p>
          )}
        </div>
      </section>
    </>
  )
}

// RequestLookup finds request events by reference, account, operation or
// decision and shows one request's decision path.
export function RequestLookup({ admin }: ViewProps) {
  const id = useId()
  const initial = new URLSearchParams(window.location.hash.split('?')[1] ?? '')
  const [form, setForm] = useState({ ref: initial.get('ref') ?? '', subject: '', operation: '', decision: '' })
  const [query, setQuery] = useState(() => (initial.get('ref') ? `ref=${encodeURIComponent(initial.get('ref')!)}` : ''))
  const results = useResource<Page<RequestEvent> | null>(
    (signal) => (query ? admin.get(`/admin/events?${query}&limit=50`, signal) : Promise.resolve({ kind: 'ok', status: 200, body: null, etag: null } as Reply<null>)),
    [query],
  )
  const [selected, setSelected] = useState<string | null>(null)
  const search = (event: FormEvent) => {
    event.preventDefault()
    const q = new URLSearchParams()
    for (const [key, value] of Object.entries(form)) if (value.trim()) q.set(key, value.trim())
    setSelected(null)
    setQuery(q.toString())
  }
  const items = results.reply?.kind === 'ok' && results.reply.body ? results.reply.body.items : []
  const chosen = items.find((e) => e.request_id === selected) ?? (items.length === 1 ? items[0] : undefined)
  return (
    <>
      <PageHeader
        title="Request lookup"
        search={false}
        lede="Find a request by the reference (Ref) a person quotes from an error, then see exactly which check decided it. The lookup covers the gateway's most recent 10,000 requests."
      />
      <form className="section card card-pad lookup-form" onSubmit={search} role="search" aria-label="Find requests">
        <Field id={`${id}-ref`} label="Reference (Ref)">
          <input id={`${id}-ref`} className="input mono" value={form.ref} onChange={(e) => setForm({ ...form, ref: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-subject`} label="Account">
          <input id={`${id}-subject`} className="input mono" value={form.subject} onChange={(e) => setForm({ ...form, subject: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-op`} label="Operation">
          <input id={`${id}-op`} className="input mono" value={form.operation} onChange={(e) => setForm({ ...form, operation: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-decision`} label="Decision">
          <select id={`${id}-decision`} className="input" value={form.decision} onChange={(e) => setForm({ ...form, decision: e.target.value })}>
            <option value="">Any</option>
            <option value="allowed">Allowed</option>
            <option value="refused">Refused</option>
          </select>
        </Field>
        <div className="lookup-submit">
          <button type="submit" className="btn btn-primary">
            Find requests
          </button>
        </div>
      </form>
      {results.reply && results.reply.kind !== 'ok' ? <Problem reply={results.reply} /> : null}
      {query && results.reply?.kind === 'ok' && items.length === 0 ? (
        <Empty title="No request matches.">
          <p className="hint">Older requests are only in the security log on the gateway host. Check the Ref for typing mistakes: it is 32 letters and digits.</p>
        </Empty>
      ) : null}
      {!query ? (
        <RecentRefusals
          admin={admin}
          pick={(ref) => {
            setForm({ ref, subject: '', operation: '', decision: '' })
            setSelected(null)
            setQuery(`ref=${encodeURIComponent(ref)}`)
          }}
        />
      ) : null}
      {chosen ? <EventDetail event={chosen} /> : null}
      {items.length > 1 ? (
        <section className="section card" aria-labelledby="matches-title">
          <div className="card-head card-head-pad">
            <h2 id="matches-title">{items.length} matching requests</h2>
            <span className="muted">Newest first. Choose a Ref to see its decision.</span>
          </div>
          <div className="card-body">
            <EventRows events={items} open={setSelected} label="Matching requests" />
          </div>
        </section>
      ) : null}
    </>
  )
}

// RecentRefusals gives Request lookup a starting point before any search: the
// latest refusals, each one choice away from its full decision.
function RecentRefusals({ admin, pick }: { admin: Admin; pick: (ref: string) => void }) {
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?decision=refused&limit=10', signal), [])
  return (
    <section className="section card" aria-labelledby="recent-title">
      <div className="card-head card-head-pad">
        <h2 id="recent-title">Latest refusals</h2>
        <span className="muted">A Ref from an error message finds exactly one request.</span>
      </div>
      <div className="card-body">
        <ReplyView reply={recent.reply}>
          {(r) => (r.items.length ? <EventRows events={r.items} open={pick} label="Latest refusals" /> : <p className="hint card-empty">Nothing refused recently.</p>)}
        </ReplyView>
      </div>
    </section>
  )
}

function EventDetail({ event }: { event: RequestEvent }) {
  return (
    <section className="section card card-pad event-detail" aria-labelledby="detail-title">
      <div className="card-head">
        <h2 id="detail-title">
          <Badge tone={decisionTone[event.decision]}>{decisionWord[event.decision]}</Badge>{' '}
          {event.decision === 'allowed' ? 'Forwarded to the service' : reasonOf(event.code)}
        </h2>
      </div>
      <ol className="path-steps" aria-label="The gateway's checks, in order">
        {pathStates(event).map((state, n) => (
          <li key={stages[n].id} className={`path-step dp-${state === 'skipped' ? 'unreached' : state}`}>
            <span className="path-bar" aria-hidden="true" />
            <span className="path-name">{stages[n].name}</span>
            <span className="path-word">{stepWords[state]}</span>
          </li>
        ))}
      </ol>
      {beforeRouting(event) ? (
        <p className="hint">The gateway decided this before matching it to an operation, so no check ahead of this one ran.</p>
      ) : null}
      <dl className="facts">
        <div className="fact">
          <dt>Reference</dt>
          <dd>
            <code>{event.request_id}</code>
          </dd>
        </div>
        <div className="fact">
          <dt>When</dt>
          <dd>
            <Time value={event.time} />
          </dd>
        </div>
        <div className="fact">
          <dt>Request</dt>
          <dd>
            <code>
              {event.method} {event.path}
            </code>
          </dd>
        </div>
        <div className="fact">
          <dt>Operation</dt>
          <dd>{event.operation ? <code>{event.operation}</code> : 'none matched'}</dd>
        </div>
        <div className="fact">
          <dt>Account</dt>
          <dd>
            {event.subject ? <code>{event.subject}</code> : 'not signed in'}
            {event.roles.length ? <> as {event.roles.join(', ')}</> : null}
          </dd>
        </div>
        <div className="fact">
          <dt>Client address</dt>
          <dd>
            <code>{event.client_address}</code>
          </dd>
        </div>
        <div className="fact">
          <dt>Answer</dt>
          <dd>
            <code>
              {event.status}
              {event.code ? ` ${event.code}` : ''}
            </code>{' '}
            in {event.duration_ms.toFixed(1)} ms
          </dd>
        </div>
        <div className="fact">
          <dt>Policy generation</dt>
          <dd>
            <code>{event.generation}</code>
          </dd>
        </div>
      </dl>
      <p className="hint">Security events never contain tokens, passwords or request bodies.</p>
    </section>
  )
}

// ApisView shows one block per API: its operations, who may call them, their
// limits and how the API is answering now.
export function ApisView({ admin }: ViewProps) {
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const traffic = useResource<Traffic>((signal) => admin.get('/admin/traffic?window=300', signal), [])
  usePoll(traffic.refresh, 10_000)
  const health = traffic.reply?.kind === 'ok' ? traffic.reply.body.by_api : []
  return (
    <>
      <PageHeader title="APIs" lede="Each API behind the gateway: its operations, who may call them, their limits, and its traffic in the last 5 minutes.">
        <ReadStamp
          readAt={policy.readAt}
          loading={policy.loading}
          onRefresh={() => {
            policy.refresh()
            traffic.refresh()
          }}
        />
      </PageHeader>
      {traffic.reply && traffic.reply.kind !== 'ok' ? <Problem reply={traffic.reply} /> : null}
      <ReplyView reply={policy.reply}>
        {(revision) => {
          const doc = revision.document as Policy | undefined
          if (!doc) return <Empty title="No confirmed policy is in force." />
          return (
            <>
              {doc.apis.map((api) => {
                const ops = doc.operations.filter((o) => o.api === api.id)
                const h = health.find((x) => x.api === api.id)
                const upstream = doc.upstreams.find((u) => u.id === api.upstream)
                return (
                  <section key={api.id} className="section card" aria-labelledby={`api-${api.id}`}>
                    <div className="card-head card-head-pad api-head">
                      <div>
                        <h2 id={`api-${api.id}`}>{apiName(api.id)}</h2>
                        <p className="hint">
                          {ops.length} operations · forwards to <code>{upstream?.dial ?? api.upstream}</code>
                        </p>
                      </div>
                      {h ? (
                        <dl className="api-figures">
                          <div>
                            <dt>Requests</dt>
                            <dd>{h.total.toLocaleString()}</dd>
                          </div>
                          <div>
                            <dt>Refused</dt>
                            <dd>{h.refused.toLocaleString()}</dd>
                          </div>
                          <div>
                            <dt>Typical</dt>
                            <dd>{h.p50_ms.toFixed(1)} ms</dd>
                          </div>
                          <div>
                            <dt>Slowest 5%</dt>
                            <dd>{h.p95_ms.toFixed(1)} ms</dd>
                          </div>
                        </dl>
                      ) : (
                        <span className="muted">No requests in the last 5 minutes</span>
                      )}
                    </div>
                    <div className="card-body">
                      <TableScroll label={`Operations of ${api.id}`}>
                        <table className="ops-table">
                          <thead>
                            <tr>
                              <th scope="col">Operation</th>
                              <th scope="col">Request</th>
                              <th scope="col">Who may call it</th>
                              <th scope="col">Limit</th>
                              <th scope="col">Body</th>
                            </tr>
                          </thead>
                          <tbody>
                            {ops.map((o) => {
                              const roles = doc.permits.filter((p) => p.operation === o.id).map((p) => p.role)
                              return (
                                <tr key={o.id}>
                                  <td>
                                    <code>{o.id}</code>
                                  </td>
                                  <td className="nowrap">
                                    <code>
                                      <span className="method">{o.method}</span> {o.target}
                                    </code>
                                  </td>
                                  <td>
                                    {o.access !== 'allow' ? (
                                      <Badge tone="refused">Closed</Badge>
                                    ) : o.auth !== 'required' ? (
                                      'Anyone'
                                    ) : roles.length ? (
                                      roles.join(', ')
                                    ) : (
                                      <span className="muted">No role yet</span>
                                    )}
                                  </td>
                                  <td className="nowrap">
                                    {o.rate ? (
                                      `${o.rate.count} per ${o.rate.window_seconds} s, ${o.rate.key === 'client_address' ? 'per address' : 'per account'}`
                                    ) : (
                                      <span className="muted">None</span>
                                    )}
                                  </td>
                                  <td className="nowrap">{o.body ? `JSON, up to ${o.body.max_bytes.toLocaleString()} bytes` : <span className="muted">None</span>}</td>
                                </tr>
                              )
                            })}
                          </tbody>
                        </table>
                      </TableScroll>
                    </div>
                  </section>
                )
              })}
            </>
          )
        }}
      </ReplyView>
    </>
  )
}

// AccountsView blocks and unblocks accounts instantly. A block is not a
// policy change: no trial, no rollback, and it lasts until an explicit unblock.
// Accounts recently seen in traffic, with the roles their tokens carried, are
// listed so an administrator can act on what is happening.
export function AccountsView({ admin, can }: ViewProps) {
  const id = useId()
  const list = useResource<{ items: BlockedAccount[] }>((signal) => admin.get('/admin/blocked-accounts', signal), [])
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?limit=100', signal), [])
  const refresh = () => {
    list.refresh()
    recent.refresh()
  }
  const block = useMutation<BlockedAccount>(admin, list.refresh)
  const unblock = useMutation<{ subject: string; unblocked: boolean }>(admin, list.refresh)
  const [subject, setSubject] = useState('')
  const [reason, setReason] = useState('')
  const [confirming, setConfirming] = useState<string | null>(null)
  const subjectInput = useRef<HTMLInputElement>(null)
  const manage = can('manage_subjects')
  const blocked = new Set(list.reply?.kind === 'ok' ? list.reply.body.items.map((b) => b.subject) : [])
  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!subject.trim() || !reason.trim()) return
    void block.run({ method: 'PUT', path: `/admin/blocked-accounts/${encodeURIComponent(subject.trim())}`, body: { reason: reason.trim() }, key: newKey() })
  }

  // The accounts seen in the latest events, newest first, with the roles
  // their last request carried and how it was decided.
  const seen = new Map<string, RequestEvent>()
  if (recent.reply?.kind === 'ok') for (const e of recent.reply.body.items) if (e.subject && !seen.has(e.subject)) seen.set(e.subject, e)

  return (
    <>
      <PageHeader
        title="Accounts"
        lede="Block an account instantly: its requests are refused from the next one, whatever roles its token carries. A block is not a policy change, so it never goes on trial and no rollback undoes it."
      >
        <ReadStamp readAt={list.readAt} loading={list.loading} onRefresh={refresh} />
      </PageHeader>
      <div className="section grid-main-side grid-top">
        <section className="card" aria-labelledby={`${id}-list`}>
          <div className="card-head card-head-pad">
            <h2 id={`${id}-list`}>Blocked accounts</h2>
          </div>
          <div className="card-body">
            <ReplyView reply={list.reply}>
              {(l) =>
                l.items.length === 0 ? (
                  <p className="hint card-empty">No account is blocked.</p>
                ) : (
                  <TableScroll label="Table of blocked accounts">
                    <table>
                      <thead>
                        <tr>
                          <th scope="col">Account</th>
                          <th scope="col">Reason</th>
                          <th scope="col">Blocked</th>
                          {manage ? (
                            <th scope="col">
                              <span className="visually-hidden">Actions</span>
                            </th>
                          ) : null}
                        </tr>
                      </thead>
                      <tbody>
                        {l.items.map((b) => (
                          <tr key={b.subject}>
                            <td>
                              <code>{b.subject}</code>
                            </td>
                            <td>{b.reason}</td>
                            <td className="nowrap">
                              <TimeOfDay value={b.blocked_at} /> <span className="muted">by {b.blocked_by}</span>
                            </td>
                            {manage ? (
                              <td className="row-actions">
                                <button type="button" className="btn btn-small" onClick={() => setConfirming(b.subject)} disabled={unblock.pending}>
                                  Unblock<span className="visually-hidden"> {b.subject}</span>
                                </button>
                              </td>
                            ) : null}
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </TableScroll>
                )
              }
            </ReplyView>
            <MutationResult mutation={unblock} onRefresh={list.refresh} success={(u) => <p className="notice-title"><code>{u.subject}</code> is unblocked and can use Fetch again</p>} />
          </div>
        </section>
        {manage ? (
          <form className="card card-pad block-form" onSubmit={submit} aria-labelledby={`${id}-title`}>
            <h2 id={`${id}-title`}>Block an account</h2>
            <Field id={`${id}-subject`} label="Account ID" hint="As shown in requests, for example cust-002.">
              <input
                ref={subjectInput}
                id={`${id}-subject`}
                className="input mono"
                value={subject}
                onChange={(e) => setSubject(e.target.value)}
                maxLength={128}
                spellCheck={false}
                autoComplete="off"
                aria-describedby={`${id}-subject-hint`}
              />
            </Field>
            <Field id={`${id}-reason`} label="Reason" hint="Recorded in the history with your name.">
              <input id={`${id}-reason`} className="input" value={reason} onChange={(e) => setReason(e.target.value)} maxLength={200} aria-describedby={`${id}-reason-hint`} />
            </Field>
            <button type="submit" className="btn btn-danger btn-wide" disabled={block.pending || !subject.trim() || !reason.trim()}>
              Block now
            </button>
            <MutationResult mutation={block} onRefresh={list.refresh} success={(b) => <p className="notice-title"><code>{b.subject}</code> is blocked: its requests are refused from now on</p>} />
          </form>
        ) : null}
      </div>
      <section className="section card" aria-labelledby={`${id}-seen`}>
        <div className="card-head card-head-pad">
          <h2 id={`${id}-seen`}>Recently seen</h2>
          <span className="muted">Accounts in the latest 100 requests, with the roles their token carried</span>
        </div>
        <div className="card-body">
          {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
          {recent.reply?.kind === 'ok' && seen.size === 0 ? <p className="hint card-empty">No signed-in requests recently.</p> : null}
          {seen.size > 0 ? (
            <TableScroll label="Recently seen accounts">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Account</th>
                    <th scope="col">Roles</th>
                    <th scope="col">Last request</th>
                    <th scope="col">Seen</th>
                    {manage ? (
                      <th scope="col">
                        <span className="visually-hidden">Actions</span>
                      </th>
                    ) : null}
                  </tr>
                </thead>
                <tbody>
                  {[...seen.values()].map((e) => (
                    <tr key={e.subject}>
                      <td>
                        <code>{e.subject}</code> {blocked.has(e.subject!) ? <Badge tone="refused">Blocked</Badge> : null}
                      </td>
                      <td>{e.roles.length ? e.roles.join(', ') : <span className="muted">none the policy defines</span>}</td>
                      <td>
                        <code className="soft">
                          <span className="method">{e.method}</span> {e.path}
                        </code>{' '}
                        <span className={e.decision === 'allowed' ? 'muted' : 'refused-word'}>{e.decision === 'allowed' ? 'allowed' : reasonOf(e.code)}</span>
                      </td>
                      <td className="nowrap muted">
                        <TimeOfDay value={e.time} seconds />
                      </td>
                      {manage ? (
                        <td className="row-actions">
                          {blocked.has(e.subject!) ? null : (
                            <button
                              type="button"
                              className="btn btn-small"
                              onClick={() => {
                                setSubject(e.subject!)
                                subjectInput.current?.focus()
                              }}
                            >
                              Block…<span className="visually-hidden"> {e.subject}</span>
                            </button>
                          )}
                        </td>
                      ) : null}
                    </tr>
                  ))}
                </tbody>
              </table>
            </TableScroll>
          ) : null}
        </div>
      </section>
      <Confirm
        open={confirming !== null}
        title="Unblock this account?"
        confirmLabel="Unblock"
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          if (confirming) void unblock.run({ method: 'DELETE', path: `/admin/blocked-accounts/${encodeURIComponent(confirming)}`, key: newKey() })
          setConfirming(null)
        }}
      >
        <p>
          <code>{confirming}</code> can use Fetch again at once, with whatever roles its token carries and the policy allows.
        </p>
      </Confirm>
    </>
  )
}

const permissionWords: Record<string, string> = {
  read: 'View',
  read_history: 'Read history',
  manage_subjects: 'Block accounts',
  manage_policy: 'Change policy',
}

// TeamView lists the gateway's administrators and the token signing keys it
// trusts. Adding an administrator stays an operator command.
export function TeamView({ admin, can }: ViewProps) {
  const admins = useResource<{ items: Administrator[] }>((signal) => admin.get('/admin/administrators', signal), [])
  const keys = useResource<{ items: SigningKey[] }>((signal) => admin.get('/admin/keys', signal), [])
  const reload = useMutation<{ items: SigningKey[] }>(admin, keys.refresh)
  return (
    <>
      <PageHeader title="Team" lede="Who administers the gateway, and the keys it trusts to verify sign-in tokens. This page cannot grant access: the operator adds administrators on the gateway host.">
        <ReadStamp
          readAt={admins.readAt}
          loading={admins.loading}
          onRefresh={() => {
            admins.refresh()
            keys.refresh()
          }}
        />
      </PageHeader>
      <section className="section card" aria-labelledby="team-title">
        <div className="card-head card-head-pad">
          <h2 id="team-title">Administrators</h2>
          <span className="muted">
            Added by the operator with <code>gateway enrol</code>
          </span>
        </div>
        <div className="card-body">
          <ReplyView reply={admins.reply}>
            {(a) => (
              <TableScroll label="Administrators">
                <table>
                  <thead>
                    <tr>
                      <th scope="col">Name</th>
                      <th scope="col">Can</th>
                      <th scope="col">Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {a.items.map((x) => (
                      <tr key={x.name}>
                        <td>
                          <code>{x.name}</code>
                        </td>
                        <td>
                          <ul className="chips">
                            {x.permissions.map((p) => (
                              <li key={p} className="chip" title={p}>
                                {permissionWords[p] ?? p}
                              </li>
                            ))}
                          </ul>
                        </td>
                        <td>
                          <Badge tone={x.status === 'active' ? 'ok' : 'neutral'}>{x.status === 'active' ? 'Active' : x.status}</Badge>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableScroll>
            )}
          </ReplyView>
        </div>
      </section>
      <section className="section card" aria-labelledby="keys-title">
        <div className="card-head card-head-pad">
          <h2 id="keys-title">Token signing keys</h2>
          {can('manage_policy') ? (
            <button type="button" className="btn btn-small" disabled={reload.pending} onClick={() => void reload.run({ method: 'POST', path: '/admin/keys/reload', key: newKey() })}>
              {reload.pending ? 'Reloading…' : 'Reload keys from settings'}
            </button>
          ) : null}
        </div>
        <div className="card-body">
          <ReplyView reply={keys.reply}>
            {(k) => (
              <TableScroll label="Signing keys">
                <table>
                  <thead>
                    <tr>
                      <th scope="col">Key ID</th>
                      <th scope="col">Fingerprint</th>
                      <th scope="col">Loaded</th>
                    </tr>
                  </thead>
                  <tbody>
                    {k.items.map((x) => (
                      <tr key={x.kid}>
                        <td>
                          <code>{x.kid}</code>
                        </td>
                        <td>
                          <code className="soft">{x.fingerprint}</code>
                        </td>
                        <td className="nowrap">
                          <Time value={x.loaded_at} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableScroll>
            )}
          </ReplyView>
          <div className="card-pad-x">
            <MutationResult mutation={reload} onRefresh={keys.refresh} success={(r) => <p className="notice-title">Reloaded: {r.items.length} key{r.items.length === 1 ? '' : 's'} trusted.</p>} />
          </div>
        </div>
      </section>
      <p className="hint section">
        Keys live in the gateway's operator settings, never in the policy, so a policy rollback can never trust a retired key again. The gateway reads them only from disk.
      </p>
    </>
  )
}

// PolicyCode exports the policy in force as one JSON document and shows how
// to stage a change from a file, a script or CI.
export function PolicyCode({ admin, open }: ViewProps) {
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const [copied, setCopied] = useState(false)
  const text = policy.reply?.kind === 'ok' ? JSON.stringify(policy.reply.body.document ?? {}, null, 2) : ''
  const [href, setHref] = useState('')
  // The download link is a blob URL of the exported text, released when the
  // text changes or the page closes.
  useEffect(() => {
    if (!text) return
    const url = URL.createObjectURL(new Blob([text], { type: 'application/json' }))
    setHref(url)
    return () => URL.revokeObjectURL(url)
  }, [text])
  return (
    <>
      <PageHeader title="Policy as code" lede="The whole policy is one JSON document: export it, keep it in Git, and stage changes from a file. Staging only creates a change; it goes live only through a trial you confirm.">
        <ReadStamp readAt={policy.readAt} loading={policy.loading} onRefresh={policy.refresh} />
      </PageHeader>
      <ReplyView reply={policy.reply}>
        {(revision) => (
          <section className="section card" aria-labelledby="export-title">
            <div className="card-head card-head-pad">
              <div>
                <h2 id="export-title">Policy in force · revision {revision.id}</h2>
                <p className="hint">
                  Checksum <code>{revision.checksum}</code>
                </p>
              </div>
              <div className="card-tools">
                <button
                  type="button"
                  className="btn btn-small"
                  onClick={() =>
                    navigator.clipboard.writeText(text).then(
                      () => setCopied(true),
                      () => setCopied(false),
                    )
                  }
                >
                  {copied ? 'Copied' : 'Copy'}
                </button>
                <a className="btn btn-small btn-primary" href={href || undefined} download={`gateway-policy-r${revision.id}.json`}>
                  Download JSON
                </a>
              </div>
            </div>
            <pre className="code-block" tabIndex={0} aria-label={`Policy revision ${revision.id} as JSON`}>
              {text}
            </pre>
          </section>
        )}
      </ReplyView>
      <section className="section card card-pad" aria-labelledby="stage-title">
        <h2 id="stage-title">Stage a change from a file</h2>
        <p>In this Dashboard, load the file on the Changes page. From a script or CI, the demo tool calls the same Admin API with an administrator session:</p>
        <pre className="code-block code-block-short" tabIndex={0} aria-label="Commands">{`go run ./cmd/demo policy export -dir <state> -out policy.json
go run ./cmd/demo policy stage  -dir <state> -file policy.json`}</pre>
        <p className="hint">The gateway's compiler checks the whole document when it is staged. Nothing changes until the change is put on trial and confirmed.</p>
        <div className="actions">
          <button type="button" className="btn" onClick={() => open('candidates')}>
            Open Changes
          </button>
        </div>
      </section>
    </>
  )
}

const notPreviewed: Record<string, string> = {
  rate: 'rate limits',
  body: 'body checks (bodies are never stored)',
  unobserved_identity: 'sign-in on requests whose token was never checked',
}

function listOf(items: string[]): string {
  return items.length < 2 ? items.join('') : `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`
}

function noneChanged(previewed: number): string {
  if (previewed === 0) return 'No recent request could be replayed.'
  if (previewed === 1) return 'The one replayed request would not be decided differently.'
  return `None of the ${previewed.toLocaleString()} replayed requests would be decided differently.`
}

// unknownText explains the requests the preview leaves out: the gateway never
// checked their sign-in token (a public operation, or a refusal before the
// sign-in check), and this change would need to know it.
function unknownText(count: number): string {
  const requests = count === 1 ? '1 request was' : `${count.toLocaleString()} requests were`
  const which = count === 1 ? 'a public operation, or a refusal before the sign-in check' : 'public operations, or refusals before the sign-in check'
  return `${requests} decided without the gateway checking a sign-in token (${which}), so the gateway cannot tell how this change would decide ${count === 1 ? 'it' : 'them'}; ${count === 1 ? 'it is' : 'they are'} left out.`
}

// ImpactPreview asks the gateway which recent requests a staged change would
// decide differently. The gateway's own compiler evaluates it; rate
// limits and bodies cannot be replayed and are said so.
export function ImpactPreview({ admin, candidate }: { admin: Admin; candidate: number }) {
  const preview = useMutation<Impact>(admin, () => {})
  const result = preview.phase.state === 'replied' && preview.phase.reply.kind === 'ok' ? preview.phase.reply.body : null
  const changed = result ? result.flips.reduce((n, f) => n + f.count, 0) : 0
  const refused = result ? result.flips.filter((f) => f.after !== 'allowed').reduce((n, f) => n + f.count, 0) : 0
  const unknown = result?.window.identity_unknown ?? 0
  return (
    <section className="impact" aria-labelledby={`impact-${candidate}`}>
      <div className="impact-head">
        <h3 id={`impact-${candidate}`}>Impact on recent traffic</h3>
        <button type="button" className="btn btn-small" disabled={preview.pending} onClick={() => void preview.run({ method: 'POST', path: `/admin/candidates/${candidate}/impact` })}>
          {preview.pending ? 'Replaying…' : result ? 'Preview again' : 'Preview impact'}
        </button>
      </div>
      {result ? (
        <>
          {result.flips.length === 0 ? (
            <p className={unknown ? 'notice tone-pending' : 'notice tone-ok'}>
              <span className="notice-title">
                {unknown
                  ? noneChanged(result.window.previewed)
                  : 'No recent request would be decided differently.'}
              </span>
              {unknown ? <> {unknownText(unknown)}</> : null}
            </p>
          ) : (
            <>
              <p className={refused ? 'notice tone-refused' : 'notice tone-pending'}>
                <span className="notice-title">
                  {changed === 1 ? '1 recent request' : `${changed.toLocaleString()} recent requests`} would be decided differently
                  {refused ? (changed === 1 ? '; it would now be refused' : `; ${refused.toLocaleString()} of them would now be refused`) : ''}.
                </span>
                {unknown ? <> {unknownText(unknown)}</> : null}
              </p>
              <TableScroll label="Decisions that would change">
                <table>
                  <thead>
                    <tr>
                      <th scope="col">Operation</th>
                      <th scope="col">Account</th>
                      <th scope="col">Now</th>
                      <th scope="col">With this change</th>
                      <th scope="col" className="num end">
                        Requests
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {result.flips.map((f) => (
                      <tr key={`${f.operation}|${f.subject}|${f.before}|${f.after}`}>
                        <td>
                          <code>{f.operation}</code>
                        </td>
                        <td>
                          <code>{f.subject ?? 'anyone'}</code>
                          {f.roles.length ? <span className="muted"> {f.roles.join(', ')}</span> : null}
                        </td>
                        <td>
                          <Badge tone={f.before === 'allowed' ? 'ok' : 'refused'}>{f.before === 'allowed' ? 'Allowed' : 'Refused'}</Badge>
                        </td>
                        <td>
                          <Badge tone={f.after === 'allowed' ? 'ok' : 'refused'}>{f.after === 'allowed' ? 'Allowed' : 'Refused'}</Badge>
                          {f.after_code ? <span className="muted"> {reasonOf(f.after_code)}</span> : null}
                        </td>
                        <td className="num end">{f.count.toLocaleString()}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableScroll>
            </>
          )}
          <p className="hint">
            Replayed {result.window.previewed.toLocaleString()} of {result.window.requests.toLocaleString()} requests from <Time value={result.window.from} /> to{' '}
            <Time value={result.window.to} />. Not replayed: {listOf(result.not_previewed.map((n) => notPreviewed[n] ?? n))}.
          </p>
        </>
      ) : preview.phase.state === 'replied' ? (
        <Problem reply={preview.phase.reply} />
      ) : (
        <p className="hint">Replays the gateway's recent requests against this change before you put it on trial. Read-only: nothing changes.</p>
      )}
    </section>
  )
}
