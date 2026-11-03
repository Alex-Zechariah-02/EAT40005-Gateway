import { useEffect, useId, useMemo, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { newKey, type Admin, type HistoryEntry, type Page, type Policy, type Reply, type Revision } from './api'
import { useGatewayState, useMutation, liveMs, usePoll, useRemembered, useRereadWhen, useResource, type Resource } from './hooks'
import { RequestRules } from './policy'
import {
  AccountLink,
  actorName,
  Badge,
  Confirm,
  Empty,
  Field,
  ListToolbar,
  matches,
  MoreDetails,
  MutationResult,
  PageHeader,
  Problem,
  ReadStamp,
  ReplyView,
  TableScroll,
  Time,
  TimeOfDay,
  type Tone,
  useFocus,
  useTarget,
  XRef,
} from './ui'
import type { ViewProps } from './views'
import { decisionWords, stages, stepWords, type Stage, type StepState } from './checks'
import { Explain } from './explain'

// The Monitor and Admin pages: live traffic, request lookup, APIs, accounts,
// team and policy as code, reading the gateway's Admin API.
// Everything shown comes from the gateway; nothing here decides or evaluates
// policy.


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
  // The busiest endpoints, who was refused most, the window before this one
  // (null when the gateway no longer holds all of it) and response time per
  // chart interval.
  by_operation?: { operation: string; api: string; total: number; allowed: number; refused: number; p50_ms: number; p95_ms: number }[]
  top_refused?: { kind: 'account' | 'address'; id: string; refused: number; latest_code: string }[]
  previous?: { total: number; allowed: number; refused: number } | null
  latency?: { at: string; count: number; p50_ms: number; p95_ms: number }[]
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
  verified: number
  last_verified_at: string | null
}

// Plain words for the gateway's refusal codes; the code stays one
// hover away.
export const reasons: Record<string, string> = {
  bad_request: 'Request Refused by Validation',
  unauthenticated: 'Not Signed In or Token Not Accepted',
  forbidden: 'No Permission',
  account_blocked: 'Account Blocked',
  not_found: 'No Such Route',
  method_not_allowed: 'Method Not Allowed',
  payload_too_large: 'Body Too Large',
  unsupported_media_type: 'Body Not JSON',
  rate_limited: 'Too Many Requests',
  upstream_unavailable: 'Service Unreachable',
  gateway_unavailable: 'Gateway Could Not Record or Decide',
  upstream_timeout: 'Service Too Slow; Outcome Unknown',
}

export function reasonOf(code: string | null): string {
  return code ? (reasons[code] ?? code) : 'Allowed'
}

const decisionTone: Record<RequestEvent['decision'], Tone> = { allowed: 'ok', refused: 'refused', failed: 'unknown' }

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
    if (event.decision === 'allowed') return 'passed'
    if (n === stop) return event.decision === 'failed' ? 'unknown' : 'refused'
    if (n > stop) return 'unreached'
    return early ? 'skipped' : 'passed'
  })
}

// DecisionPath shows the checks a request cleared, the one that decided it
// and those it never reached.
export function DecisionPath({ event }: { event: Pick<RequestEvent, 'stage' | 'decision' | 'operation'> }) {
  const states = pathStates(event)
  const allowed = event.decision === 'allowed'
  const early = beforeRouting(event)
  const name = stages.find((s) => s.id === event.stage)?.name ?? event.stage
  const cleared = states.filter((s) => s === 'passed').length
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
      <span className={`dp-label dp-label-${event.decision}`}>{allowed ? 'All Passed' : early ? 'Before Routing' : name}</span>
    </span>
  )
}

// intervals spreads the gateway's per-second counts, which list only the
// seconds that saw requests, over 60 equal intervals ending with the one that
// holds end. Each interval starts on a whole multiple of its length on the
// clock (every second, 5 seconds or minute), so a request stays in the same
// interval from one reading to the next and the bars move left in step
// instead of jumping between neighbours.
function intervals(traffic: Traffic, end: number) {
  const count = 60
  const step = Math.max(1000, (traffic.window_seconds * 1000) / count)
  const last = Math.floor(end / step) * step
  const first = last - (count - 1) * step
  const bars = Array.from({ length: count }, (_, n) => ({
    at: new Date(first + n * step).toISOString(),
    to: new Date(first + (n + 1) * step).toISOString(),
    allowed: 0,
    refused: 0,
    p50: null as number | null,
    p95: null as number | null,
  }))
  for (const p of traffic.per_second) {
    const n = Math.floor((Date.parse(p.at) - first) / step)
    if (n < 0 || n >= count) continue
    bars[n].allowed += p.allowed
    bars[n].refused += p.refused
  }
  // The gateway gives response times on the same intervals.
  for (const l of traffic.latency ?? []) {
    const n = Math.floor((Date.parse(l.at) - first) / step)
    if (n < 0 || n >= count) continue
    bars[n].p50 = l.p50_ms
    bars[n].p95 = l.p95_ms
  }
  return { bars, step }
}

function intervalWords(step: number): string {
  return step >= 60_000 ? `${step / 60_000} min` : `${step / 1000} s`
}

// TrafficChart draws requests per interval: allowed in blue, refused in red
// on top. Hovering, or moving along the bars with the arrow keys, shows the
// interval's figures beside it; the table view lists them all.
export function TrafficChart({ traffic, end }: { traffic: Traffic; end: number }) {
  const [active, setActive] = useState<number | null>(null)
  const id = useId()
  const { bars, step } = useMemo(() => intervals(traffic, end), [traffic, end])
  if (traffic.per_second.length === 0) return <Empty title="No requests in this window yet." />
  const peak = Math.max(1, ...bars.map((p) => p.allowed + p.refused))
  const width = 600
  const height = 150
  const band = width / bars.length
  const bar = Math.max(2, Math.min(9, band * 0.7))
  const shown = active !== null ? bars[active] : null
  const seconds = traffic.window_seconds <= 300
  const label = (iso: string) => <TimeOfDay value={iso} seconds={seconds} />
  const tip =
    shown && active !== null ? (
      <div
        className="chart-tip"
        role="status"
        style={{ left: `${((active + 0.5) / bars.length) * 100}%` }}
        data-side={active > bars.length * 0.7 ? 'left' : 'right'}
      >
        <p className="chart-tip-time">
          {label(shown.at)} – {label(shown.to)}
        </p>
        <dl>
          <div>
            <dt>
              <span className="key key-allowed" aria-hidden="true" />
              Allowed
            </dt>
            <dd>{shown.allowed}</dd>
          </div>
          <div>
            <dt>
              <span className="key key-refused" aria-hidden="true" />
              Refused
            </dt>
            <dd>
              {shown.refused}
              {shown.allowed + shown.refused > 0 ? (
                <span className="muted"> {Math.round((shown.refused / (shown.allowed + shown.refused)) * 100)}%</span>
              ) : null}
            </dd>
          </div>
        </dl>
        {shown.p50 !== null && shown.p95 !== null ? (
          <p className="chart-tip-note">
            Typical {shown.p50.toFixed(1)} ms · Slowest 5% {shown.p95.toFixed(1)} ms
          </p>
        ) : null}
        {shown.allowed + shown.refused === 0 ? <p className="chart-tip-note">No requests</p> : null}
      </div>
    ) : null
  return (
    <figure className="traffic-chart">
      <figcaption className="chart-scale">
        <span>Requests every {intervalWords(step)}</span>
        <span className="muted">Peak {peak}</span>
      </figcaption>
      <div
        className="chart-plot"
        tabIndex={0}
        role="group"
        aria-label="Requests allowed and refused over the window"
        aria-describedby={`${id}-h`}
        onMouseLeave={() => setActive(null)}
        onBlur={() => setActive(null)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
            event.preventDefault()
            const from = active ?? bars.length
            setActive(Math.min(bars.length - 1, Math.max(0, from + (event.key === 'ArrowLeft' ? -1 : 1))))
          }
          if (event.key === 'Escape') setActive(null)
        }}
      >
        <span id={`${id}-h`} className="visually-hidden">
          Use the left and right arrow keys to read each interval.
        </span>
        <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" aria-hidden="true">
          <line x1={0} x2={width} y1={0.5} y2={0.5} className="chart-grid" />
          <line x1={0} x2={width} y1={height / 2} y2={height / 2} className="chart-grid" />
          <line x1={0} x2={width} y1={height - 0.5} y2={height - 0.5} className="chart-base" />
          {bars.map((p, n) => {
            const total = ((p.allowed + p.refused) / peak) * (height - 6)
            const refused = (p.refused / peak) * (height - 6)
            // A 2px surface gap separates the stacked segments.
            const gap = p.refused > 0 && p.allowed > 0 ? 2 : 0
            const x = n * band + (band - bar) / 2
            return (
              <g key={p.at} onMouseEnter={() => setActive(n)}>
                <rect x={n * band} y={0} width={band} height={height} className={active === n ? 'chart-hover' : 'chart-hit'} />
                {p.allowed > 0 ? (
                  <rect x={x} y={height - total} width={bar} height={Math.max(1, total - refused - gap)} rx={1.5} className="chart-allowed" />
                ) : null}
                {p.refused > 0 ? <rect x={x} y={height - refused} width={bar} height={Math.max(1, refused)} rx={1.5} className="chart-refused" /> : null}
              </g>
            )
          })}
        </svg>
        <LatencyStrip bars={bars} active={active} onActive={setActive} />
        {tip}
      </div>
      <div className="chart-axis" aria-hidden="true">
        {label(bars[0].at)}
        {label(bars[bars.length / 2].at)}
        <span>now</span>
      </div>
      <details className="chart-table">
        <summary>Show as a Table</summary>
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
              {traffic.per_second.map((p) => (
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

// LatencyStrip draws the typical (median) and slowest-5% response time of
// each interval under the request bars, on the same time scale, so a
// slowdown lines up with the traffic that caused it.
function LatencyStrip(props: { bars: ReturnType<typeof intervals>['bars']; active: number | null; onActive: (n: number) => void }) {
  const { bars } = props
  const slowest = Math.max(0, ...bars.map((b) => b.p95 ?? 0))
  if (slowest === 0) return null
  const width = 600
  const height = 56
  const band = width / bars.length
  const y = (ms: number) => height - 4 - (ms / slowest) * (height - 10)
  // A line runs only through intervals with requests; a gap stays a gap.
  const path = (pick: (b: (typeof bars)[number]) => number | null) =>
    bars
      .map((b, n) => {
        const value = pick(b)
        if (value === null) return ''
        const previous = n > 0 ? pick(bars[n - 1]) : null
        return `${previous === null ? 'M' : 'L'} ${(n + 0.5) * band} ${y(value)}`
      })
      .join(' ')
  return (
    <div className="latency">
      <p className="chart-scale latency-scale">
        <span>
          Response Time · <span className="key key-p50" aria-hidden="true" /> Typical <span className="key key-p95" aria-hidden="true" /> Slowest 5%
        </span>
        <span className="muted">Peak {slowest.toFixed(1)} ms</span>
      </p>
      <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" aria-hidden="true">
        <line x1={0} x2={width} y1={height - 0.5} y2={height - 0.5} className="chart-base" />
        {bars.map((b, n) => (
          <rect
            key={b.at}
            x={n * band}
            y={0}
            width={band}
            height={height}
            className={props.active === n ? 'chart-hover' : 'chart-hit'}
            onMouseEnter={() => props.onActive(n)}
          />
        ))}
        <path d={path((b) => b.p95)} className="latency-p95" />
        <path d={path((b) => b.p50)} className="latency-p50" />
      </svg>
    </div>
  )
}

// Change states a figure against the window before, in words and an arrow,
// so it never depends on colour alone.
function Change({ now, before, span }: { now: number; before: number | undefined; span: string }) {
  if (before === undefined) return null
  const difference = now - before
  if (difference === 0) return <span className="figure-change">Same as the previous {span}</span>
  return (
    <span className={`figure-change ${difference > 0 ? 'is-up' : 'is-down'}`}>
      {difference > 0 ? '▲' : '▼'} {Math.abs(difference).toLocaleString()} vs previous {span}
    </span>
  )
}

function windowWords(seconds: number): string {
  return windows.find((w) => w.seconds === seconds)?.label ?? `${seconds} s`
}

// Insights lists the busiest endpoints and who was refused most in the
// window, each linked to where it can be inspected.
// BusiestCard lists the endpoints with the most requests in the window: a
// compact list for the narrow column, each endpoint's count at the end of
// its row and its refusals and response times beneath.
function BusiestCard({ traffic }: { traffic: Traffic }) {
  const busiest = traffic.by_operation ?? []
  if (traffic.total === 0) return null
  return (
    <section className="card card-pad" aria-labelledby="busiest-title">
      <div className="card-head">
        <h2 id="busiest-title">Busiest Endpoints</h2>
        <span className="muted">Last {windowWords(traffic.window_seconds)}</span>
      </div>
      {busiest.length === 0 ? (
        <p className="hint card-quiet">No request matched an endpoint in this window.</p>
      ) : (
        <ul className="rank-list" aria-label="Busiest Endpoints">
          {busiest.map((o) => (
            <li key={o.operation}>
              <span className="rank-name">
                <XRef view="policy" focus={{ select: `operation:${o.operation}` }}>
                  <code>{o.operation}</code>
                </XRef>
                <span className="cell-sub">
                  {o.refused.toLocaleString()} refused · typical {o.p50_ms.toFixed(1)} ms · slowest 5% {o.p95_ms.toFixed(1)} ms
                </span>
              </span>
              <span className="num" title="Requests">
                {o.total.toLocaleString()}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

// RefusalsCard says why requests were refused in the window and who was
// refused most. With nothing refused it is one short line.
function RefusalsCard({ traffic }: { traffic: Traffic }) {
  const refused = traffic.top_refused ?? []
  return (
    <section className="card card-pad" aria-labelledby="reasons-title">
      <div className="card-head">
        <h2 id="reasons-title">Refusals</h2>
        <span className="muted">Last {windowWords(traffic.window_seconds)}</span>
      </div>
      {traffic.refusal_reasons.length === 0 ? (
        <p className="hint card-quiet">Nothing was refused in this window.</p>
      ) : (
        <>
          <h3 className="card-sub">Reasons</h3>
          <Reasons traffic={traffic} />
          {refused.length ? (
            <>
              <h3 className="card-sub">Most Refused</h3>
              <ul className="rank-list" aria-label="Most Refused Accounts and Addresses">
                {refused.map((r) => (
                  <li key={`${r.kind} ${r.id}`}>
                    <span className="rank-name">
                      {r.kind === 'account' ? (
                        <AccountLink account={r.id}>
                          <code>{r.id}</code>
                        </AccountLink>
                      ) : (
                        <code>{r.id}</code>
                      )}
                      <span className="cell-sub">
                        {r.kind === 'account' ? 'Account' : 'Client address'} · <span title={r.latest_code}>{reasonOf(r.latest_code || null)}</span>
                      </span>
                    </span>
                    <span className="num">{r.refused.toLocaleString()}</span>
                  </li>
                ))}
              </ul>
            </>
          ) : null}
        </>
      )}
    </section>
  )
}

function Figures({ traffic }: { traffic: Traffic }) {
  const share = traffic.total ? ((traffic.refused / traffic.total) * 100).toFixed(1) : '0.0'
  const before = traffic.previous ?? undefined
  const span = windowWords(traffic.window_seconds)
  return (
    <dl className="figures">
      <div>
        <dt>Requests</dt>
        <dd>
          {traffic.total.toLocaleString()}
          <Change now={traffic.total} before={before?.total} span={span} />
        </dd>
      </div>
      <div>
        <dt>
          <span className="key key-allowed" aria-hidden="true" />
          Allowed
        </dt>
        <dd>
          {traffic.allowed.toLocaleString()}
          <Change now={traffic.allowed} before={before?.allowed} span={span} />
        </dd>
      </div>
      <div>
        <dt>
          <span className="key key-refused" aria-hidden="true" />
          Refused
        </dt>
        <dd>
          {traffic.refused.toLocaleString()} <span className="figure-share">{share}%</span>
          <Change now={traffic.refused} before={before?.refused} span={span} />
        </dd>
      </div>
    </dl>
  )
}

function Reasons({ traffic }: { traffic: Traffic }) {
  const top = Math.max(1, ...traffic.refusal_reasons.map((r) => r.count))
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
    <div className="segmented" role="group" aria-label="Time Window">
      {windows.map((w) => (
        <button key={w.seconds} type="button" aria-pressed={value === w.seconds} onClick={() => onChange(w.seconds)}>
          {w.label}
        </button>
      ))}
    </div>
  )
}

// windowEnd is when the shown traffic reading's window ends: the gateway's
// clock when it answered, or the browser's when the reading arrived if no
// date came.
function windowEnd(resource: Resource<Traffic>): number {
  return resource.last?.date ?? resource.last?.at.getTime() ?? Date.now()
}

// TrafficCards shows the last traffic reading that arrived. When a later
// reading failed it says how old the figures are, so they are never taken
// for current ones. The page is two columns that stack their cards
// independently: the traffic chart in the wide one, the refusals and the
// busiest endpoints in the narrow one, and any further cards a view adds to
// each, so the two columns end close together. On Live Traffic the stream
// fills the wide column and the narrow one stays in view beside it.
function TrafficCards(props: {
  resource: Resource<Traffic>
  span: number
  onSpan: (seconds: number) => void
  main?: ReactNode
  side?: ReactNode
  stickySide?: boolean
}) {
  const { resource, span, onSpan } = props
  const reading = resource.last
  const traffic = reading?.body
  return (
    <div className="section grid-main-side grid-top">
      <div className="card-stack">
        {reading && traffic ? (
          <>
            <section className="card card-pad" aria-labelledby="traffic-title">
              <div className="card-head">
                <h2 id="traffic-title">Traffic</h2>
                <WindowSwitch value={span} onChange={onSpan} />
              </div>
              {resource.failed ? (
                <p className="notice tone-pending card-notice">
                  Not current: these figures were read at <TimeOfDay value={reading.at.toISOString()} seconds /> and the latest read failed.
                </p>
              ) : null}
              <Figures traffic={traffic} />
              <TrafficChart traffic={traffic} end={windowEnd(resource)} />
            </section>
          </>
        ) : null}
        {props.main}
      </div>
      <div className={props.stickySide ? 'card-stack card-stack-sticky' : 'card-stack'}>
        {traffic ? (
          <>
            <RefusalsCard traffic={traffic} />
            <BusiestCard traffic={traffic} />
          </>
        ) : null}
        {props.side}
      </div>
    </div>
  )
}

export function EventRows({ events, open, label }: { events: RequestEvent[]; open: (ref: string) => void; label: string }) {
  return (
    <TableScroll label={label}>
      <table className="events">
        <thead>
          <tr>
            <th scope="col">Time</th>
            <th scope="col">Request</th>
            <th scope="col">Account</th>
            <th scope="col">Result</th>
            <th scope="col">Checks</th>
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
                {e.operation ? (
                  <XRef view="policy" focus={{ select: `operation:${e.operation}` }}>
                    <code>
                      <span className="method">{e.method}</span> {e.path}
                    </code>
                  </XRef>
                ) : (
                  <code>
                    <span className="method">{e.method}</span> {e.path}
                  </code>
                )}
              </td>
              <td className="nowrap">
                {e.subject ? (
                  <AccountLink account={e.subject}>
                    <code className="soft">{e.subject}</code>
                  </AccountLink>
                ) : (
                  <code className="soft">{e.client_address}</code>
                )}
              </td>
              <td>
                {e.decision === 'allowed' ? (
                  <span className="muted">
                    Allowed in {e.duration_ms.toFixed(1)} ms
                  </span>
                ) : (
                  <span title={e.code ?? undefined}>{reasonOf(e.code)}</span>
                )}{' '}
                <span className="status-chip">{e.status}</span>
              </td>
              <td>
                <DecisionPath event={e} />
              </td>
              <td className="nowrap end">
                <button type="button" className="link-button" onClick={() => open(e.request_id)} title="Open in Request Lookup">
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
  window.location.hash = `#/request-lookup?ref=${encodeURIComponent(ref)}`
}

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

// apiName names an API; the gateway counts requests that matched no route
// under "unknown".
function apiName(id: string): string {
  if (id === 'unknown') return 'No route matched'
  return id.slice(0, 1).toUpperCase() + id.slice(1)
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
                        <TimeOfDay value={e.recorded_at} />
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

// LiveTraffic streams request events as they happen and summarises a
// chosen window. Pause keeps the list still for inspection; events that
// arrive meanwhile are counted, not lost from the gateway's record.
// The page keeps more requests than it draws, so a filter can still find
// writes made minutes ago between the boards' frequent reads.
const keptEvents = 1000
const shownEvents = 200
const reads = new Set(['GET', 'HEAD', 'OPTIONS'])

export function LiveTraffic({ admin }: ViewProps) {
  const state = useGatewayState()
  const [span, setSpan] = useState(60)
  const traffic = useResource<Traffic>((signal) => admin.get(`/admin/traffic?window=${span}`, signal), [span])
  usePoll(traffic.refresh, liveMs, state.critical)
  const [events, setEvents] = useState<RequestEvent[]>([])
  const [paused, setPaused] = useState(false)
  const [held, setHeld] = useState(0)
  const [dropped, setDropped] = useState(0)
  const [reconnected, setReconnected] = useState(false)
  const [stream, setStream] = useState<'connecting' | 'live' | 'closed'>('connecting')
  const [only, setOnly] = useState<'all' | 'changes' | 'refused'>('all')
  const pausedRef = useRef(paused)
  pausedRef.current = paused
  // Requests that arrive while paused wait here, newest first, and join the
  // list on Resume.
  const waiting = useRef<RequestEvent[]>([])

  useEffect(() => {
    const source = new EventSource('/admin/events/stream')
    let opened = false
    source.onopen = () => {
      if (opened) setReconnected(true)
      opened = true
      setStream('live')
    }
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
      <PageHeader title="Live Traffic" lede="Every request the gateway decides, in real time, with the check that decided it." />
      {traffic.reply && traffic.reply.kind !== 'ok' ? <Problem reply={traffic.reply} /> : null}
      {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
      <TrafficCards
        resource={traffic}
        span={span}
        onSpan={setSpan}
        stickySide
        main={
          <section className="card" aria-labelledby="stream-title">
            <div className="card-head card-head-pad">
              <h2 id="stream-title">
                Request Stream{' '}
                <span className={`live-state live-${stream}`} role="status">
                  {stream === 'live' ? (paused ? 'Paused' : 'Live') : stream === 'closed' ? 'Stream Closed' : 'Connecting…'}
                </span>
              </h2>
              <div className="card-tools">
                <div className="segmented" role="group" aria-label="Show">
                  <button type="button" aria-pressed={only === 'all'} onClick={() => setOnly('all')}>
                    All
                  </button>
                  <button type="button" aria-pressed={only === 'changes'} onClick={() => setOnly('changes')} title="Write requests only; reads are hidden">
                    Changes Only
                  </button>
                  <button type="button" aria-pressed={only === 'refused'} onClick={() => setOnly('refused')}>
                    Refused Only
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
            {reconnected ? (
              <p className="notice tone-pending card-notice">Stream reconnected. Events may have been missed; use Request lookup to search recent requests.</p>
            ) : null}
            {dropped > 0 ? (
              <p className="notice tone-pending card-notice">This view fell behind and skipped {dropped} events. The security log has every request.</p>
            ) : null}
            <div className="card-body">
              {shown.length ? (
                <>
                  <EventRows events={shown} open={lookupRef} label="Request Stream" />
                  {matching.length > shown.length ? (
                    <p className="hint card-empty">
                      Showing the newest {shown.length} of {matching.length.toLocaleString()}. Request lookup finds any request by its Ref.
                    </p>
                  ) : null}
                </>
              ) : (
                <p className="hint card-empty">
                  {stream === 'closed'
                    ? 'Stream closed. Reload the page to reconnect.'
                    : all.length
                      ? 'No matching requests yet.'
                      : 'Waiting for requests.'}
                </p>
              )}
            </div>
          </section>
        }
      />
    </>
  )
}

// RequestLookup finds request events by reference, account, operation or
// decision and shows one request's decision path.
const blankLookup = { ref: '', subject: '', operation: '', decision: '' }

export function RequestLookup({ admin }: ViewProps) {
  const id = useId()
  // A Ref in the address starts a new search. Otherwise the last search of
  // this session is shown again: it is kept in the page's memory, so an
  // account searched for never goes into the address or browser history.
  const linked = new URLSearchParams(window.location.hash.split('?')[1] ?? '').get('ref')
  const [saved, remember] = useRemembered('lookup', { form: blankLookup, query: '' })
  const start = linked ? { form: { ...blankLookup, ref: linked }, query: `ref=${encodeURIComponent(linked)}` } : saved
  const [form, setForm] = useState(start.form)
  const [query, setSearch] = useState(start.query)
  const results = useResource<Page<RequestEvent> | null>(
    (signal) => (query ? admin.get(`/admin/events?${query}&limit=50`, signal) : Promise.resolve({ kind: 'ok', status: 200, body: null, etag: null } as Reply<null>)),
    [query],
  )
  const [selected, setSelected] = useState<string | null>(null)
  const setQuery = (next: string, nextForm = form) => {
    setSearch(next)
    remember({ form: nextForm, query: next })
  }
  useEffect(() => {
    if (linked) remember(start)
  }, [])
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
        title="Request Lookup"
        search={false}
        lede="Find a request by Ref, account or endpoint and see each check it passed or refused."
      />
      <form className="section card card-pad lookup-form" onSubmit={search} role="search" aria-label="Find Requests">
        <Field id={`${id}-ref`} label="Reference (Ref)">
          <input id={`${id}-ref`} className="input mono" value={form.ref} onChange={(e) => setForm({ ...form, ref: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-subject`} label="Account">
          <input id={`${id}-subject`} className="input mono" value={form.subject} onChange={(e) => setForm({ ...form, subject: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-op`} label="Endpoint">
          <input
            id={`${id}-op`}
            className="input mono"
            value={form.operation}
            onChange={(e) => setForm({ ...form, operation: e.target.value })}
            spellCheck={false}
            autoComplete="off"
          />
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
            Find Requests
          </button>
        </div>
      </form>
      {results.reply && results.reply.kind !== 'ok' ? <Problem reply={results.reply} /> : null}
      {query && results.reply?.kind === 'ok' && items.length === 0 ? (
        <Empty title="No request matches.">
          <p className="hint">
            Only the latest 10,000 requests since the gateway started are searched. Older requests are in the security log on the gateway host.
            A full Ref is 32 letters and digits.
          </p>
        </Empty>
      ) : null}
      {!query ? (
        <RecentRefusals
          admin={admin}
          pick={(ref) => {
            const picked = { ...blankLookup, ref }
            setForm(picked)
            setSelected(null)
            setQuery(`ref=${encodeURIComponent(ref)}`, picked)
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
            <EventRows events={items} open={setSelected} label="Matching Requests" />
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
        <h2 id="recent-title">Latest Refusals</h2>
        <span className="muted">A Ref from an error message finds exactly one request.</span>
      </div>
      <div className="card-body">
        <ReplyView reply={recent.reply}>
          {(r) => (r.items.length ? <EventRows events={r.items} open={pick} label="Latest Refusals" /> : <p className="hint card-empty">Nothing refused recently.</p>)}
        </ReplyView>
      </div>
    </section>
  )
}

export function EventDetail({ event }: { event: RequestEvent }) {
  return (
    <section className="section card card-pad event-detail" aria-labelledby="detail-title">
      <div className="card-head">
        <h2 id="detail-title">
          <Badge tone={decisionTone[event.decision]}>{decisionWords[event.decision]}</Badge>{' '}
          {event.decision === 'allowed' ? 'Forwarded to the service' : reasonOf(event.code)}
        </h2>
      </div>
      <ol className="path-steps" aria-label="The gateway's checks, in order">
        {pathStates(event).map((state, n) => (
          <li key={stages[n].id} className={`path-step dp-${state === 'skipped' ? 'unreached' : state}`}>
            <span className="path-bar" aria-hidden="true" />
            <span className="path-name">
              <Explain term={stages[n].name} plain={stages[n].plain} technical={stages[n].technical} />
            </span>
            <span className="path-word">{stepWords[state]}</span>
          </li>
        ))}
      </ol>
      {beforeRouting(event) ? (
        <p className="hint">Decided before endpoint matching; earlier checks did not run.</p>
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
          <dt>Endpoint</dt>
          <dd>
            {event.operation ? (
              <XRef view="policy" focus={{ select: `operation:${event.operation}` }}>
                <code>{event.operation}</code>
              </XRef>
            ) : (
              'none matched'
            )}
          </dd>
        </div>
        <div className="fact">
          <dt>Account</dt>
          <dd>
            {event.subject ? <code>{event.subject}</code> : 'not signed in'}
            {event.roles.length ? <> as {event.roles.join(', ')}</> : null}
          </dd>
        </div>
        <div className="fact">
          <dt>Client Address</dt>
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
          <dt>Active Policy (Generation)</dt>
          <dd>
            <code>{event.generation}</code>
          </dd>
        </div>
      </dl>
      <p className="hint">Tokens, passwords and request bodies are never logged.</p>
    </section>
  )
}

// ApisView shows one block per API: its operations, who may call them, their
// limits and how the API is answering now.
// limitShort states a rate limit briefly, such as "120/min". The counting key
// is said once above the table; a limit that counts the other way says so.
function limitShort(o: Policy['operations'][number]): string {
  if (!o.rate) return 'None'
  const seconds = o.rate.window_seconds
  const span = seconds === 1 ? 's' : seconds === 60 ? 'min' : seconds === 3600 ? 'h' : `${seconds} s`
  const usual = o.auth === 'none' ? 'client_address' : 'subject'
  const unusual = o.rate.key !== usual ? (o.rate.key === 'client_address' ? ' per address' : ' per account') : ''
  return `${o.rate.count}/${span}${unusual}`
}

function bytesShort(bytes: number): string {
  return bytes >= 1024 ? `${(bytes / 1024).toLocaleString(undefined, { maximumFractionDigits: 1 })} KB` : `${bytes} B`
}

export function ApisView({ admin, can }: ViewProps) {
  const state = useGatewayState()
  const [query, setQuery] = useState('')
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const traffic = useResource<Traffic>((signal) => admin.get('/admin/traffic?window=300', signal), [])
  usePoll(traffic.refresh, liveMs, state.critical)
  // The last figures read stay shown when a refresh fails; the problem
  // notice above says the latest read failed.
  const health = traffic.last?.body.by_api ?? []
  // A link from another page names one API or one endpoint: it is brought
  // into view and highlighted.
  const linkedApi = useFocus('api')
  const linkedOperation = useFocus('operation')
  useTarget(policy.reply?.kind === 'ok', linkedOperation ?? linkedApi)
  return (
    <>
      <PageHeader
        title="APIs"
        lede="Each protected API with its endpoints, who may call them, limits and traffic."
      >
        <ReadStamp
          okAt={policy.last?.at ?? null} failed={policy.failed}
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
          if (!doc) return <Empty title="No policy is active yet." />
          const found = (o: (typeof doc.operations)[number]) =>
            matches(
              query,
              o.api,
              apiName(o.api),
              o.id,
              `${o.method} ${o.target}`,
              ...doc.permits.filter((p) => p.operation === o.id).map((p) => p.role),
            )
          const shown = doc.operations.filter(found).length
          return (
            <>
              <ListToolbar
                label="Filter APIs and endpoints"
                query={query}
                onQuery={setQuery}
                shown={shown}
                total={doc.operations.length}
                noun={['endpoint', 'endpoints']}
              >
                {can('manage_policy') ? (
                  <a className="btn btn-primary" href="#/policy">
                    Edit Endpoints in Policy
                  </a>
                ) : null}
              </ListToolbar>
              {shown === 0 ? <p className="hint">No API or endpoint matches the filter.</p> : null}
              {doc.apis.map((api) => {
                const all = doc.operations.filter((o) => o.api === api.id)
                const ops = all.filter(found)
                if (ops.length === 0 && all.length > 0) return null
                const h = health.find((x) => x.api === api.id)
                const upstream = doc.upstreams.find((u) => u.id === api.upstream)
                return (
                  <section
                    key={api.id}
                    className="section card"
                    aria-labelledby={`api-${api.id}`}
                    data-target={!linkedOperation && linkedApi === api.id ? 'true' : undefined}
                  >
                    <div className="card-head card-head-pad api-head">
                      <div>
                        <h2 id={`api-${api.id}`}>{apiName(api.id)}</h2>
                        <p className="hint">
                          {all.length} endpoints · service address <code>{upstream?.dial ?? api.upstream}</code> · limits count per client
                          address for public endpoints, per account otherwise
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
                      <TableScroll label={`Endpoints of ${api.id}`}>
                        <table className="ops-table">
                          <thead>
                            <tr>
                              <th scope="col">Endpoint</th>
                              <th scope="col">Who May Call</th>
                              <th scope="col">Limit</th>
                              <th scope="col">Body</th>
                            </tr>
                          </thead>
                          <tbody>
                            {ops.map((o) => {
                              const roles = doc.permits.filter((p) => p.operation === o.id).map((p) => p.role)
                              return (
                                <tr key={o.id} data-target={linkedOperation === o.id ? 'true' : undefined}>
                                  <td>
                                    <code className="ops-route">
                                      <span className="method">{o.method}</span> {o.target}
                                    </code>
                                    <span className="cell-sub">
                                      <XRef view="policy" focus={{ select: `operation:${o.id}` }}>
                                        <code>{o.id}</code>
                                      </XRef>
                                    </span>
                                  </td>
                                  <td>
                                    {o.access !== 'allow' ? (
                                      <Badge tone="refused">Closed</Badge>
                                    ) : o.auth !== 'required' ? (
                                      <span className="chip">Public</span>
                                    ) : roles.length ? (
                                      <span className="chips">
                                        {roles.map((role) => (
                                          <XRef key={role} view="versions" focus={{ role }}>
                                            <span className="chip">{role}</span>
                                          </XRef>
                                        ))}
                                      </span>
                                    ) : (
                                      <span className="muted">No Role Yet</span>
                                    )}
                                  </td>
                                  <td className="nowrap">{limitShort(o)}</td>
                                  <td className="nowrap">
                                    {o.body ? `JSON ≤ ${bytesShort(o.body.max_bytes)}` : <span className="muted">None</span>}
                                  </td>
                                </tr>
                              )
                            })}
                          </tbody>
                        </table>
                      </TableScroll>
                      {ops.some((o) => o.body || o.headers) ? (
                        <details className="api-rules">
                          <summary>Request Rules</summary>
                          {ops
                            .filter((o) => o.body || o.headers)
                            .map((o) => (
                              <div key={o.id}>
                                <RequestRules operation={o} />
                                <MoreDetails
                                  facts={[
                                    ['Body Schema (JSON)', o.body ? JSON.stringify(o.body.schema) : undefined],
                                    ['Header Schema (JSON)', o.headers ? JSON.stringify(o.headers) : undefined],
                                  ]}
                                />
                              </div>
                            ))}
                        </details>
                      ) : null}
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
  const [query, setQuery] = useState('')
  const [show, setShow] = useState<'all' | 'blocked' | 'seen'>('all')
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?limit=100', signal), [])
  const refresh = () => {
    list.refresh()
    recent.refresh()
  }
  const block = useMutation<BlockedAccount>(admin, list.refresh)
  const unblock = useMutation<{ subject: string; unblocked: boolean }>(admin, list.refresh)
  const [blocking, setBlocking] = useState(false)
  const [subject, setSubject] = useState('')
  const [reason, setReason] = useState('')
  const [confirming, setConfirming] = useState<string | null>(null)
  const manage = can('manage_subjects')
  const blockedList = list.reply?.kind === 'ok' ? list.reply.body.items : []
  const blocked = new Map(blockedList.map((b) => [b.subject, b]))
  const openBlock = (account = '') => {
    setSubject(account)
    setReason('')
    setBlocking(true)
  }
  const submit = () => {
    if (!subject.trim() || !reason.trim()) return
    setBlocking(false)
    void block.run({ method: 'PUT', path: `/admin/blocked-accounts/${encodeURIComponent(subject.trim())}`, body: { reason: reason.trim() }, key: newKey() })
  }

  // Every account the gateway knows about in one list: the blocked ones
  // first, newest block first, then the accounts seen in the latest
  // requests, newest first, with the roles and outcome of their last request.
  const seen = new Map<string, RequestEvent>()
  if (recent.reply?.kind === 'ok') for (const e of recent.reply.body.items) if (e.subject && !seen.has(e.subject)) seen.set(e.subject, e)
  const accounts = [
    ...[...blockedList].sort((a, b) => b.blocked_at.localeCompare(a.blocked_at)).map((b) => b.subject),
    ...[...seen.keys()].filter((account) => !blocked.has(account)),
  ]
  const counts = { all: accounts.length, blocked: blocked.size, seen: seen.size }
  const shown = accounts.filter((account) => {
    if (show === 'blocked' && !blocked.has(account)) return false
    if (show === 'seen' && !seen.has(account)) return false
    const b = blocked.get(account)
    const e = seen.get(account)
    return matches(query, account, b?.reason, b?.blocked_by, ...(e?.roles ?? []), e ? `${e.method} ${e.path}` : undefined)
  })
  const filters: ['all' | 'blocked' | 'seen', string][] = [
    ['all', 'All'],
    ['blocked', 'Blocked'],
    ['seen', 'Recently Seen'],
  ]

  return (
    <>
      <PageHeader title="Accounts" lede="Block or unblock accounts; a block applies at every endpoint at once.">
        <ReadStamp okAt={list.last?.at ?? null} failed={list.failed} loading={list.loading} onRefresh={refresh} />
        {manage ? (
          <button type="button" className="btn btn-primary btn-small" onClick={() => openBlock()}>
            Block an Account
          </button>
        ) : null}
      </PageHeader>
      {list.reply && list.reply.kind !== 'ok' ? <Problem reply={list.reply} /> : null}
      {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
      <MutationResult
        mutation={block}
        onRefresh={list.refresh}
        success={(b) => (
          <p className="notice-title">
            <code>{b.subject}</code> is blocked: its requests are refused from now on
          </p>
        )}
      />
      <MutationResult
        mutation={unblock}
        onRefresh={list.refresh}
        success={(u) => (
          <>
            <p className="notice-title">
              <code>{u.subject}</code> is unblocked and can use Fetch again
            </p>
            <p>Access is again limited to what its token roles and the policy allow.</p>
          </>
        )}
      />
      <section className="section card" aria-labelledby={`${id}-list`}>
        <h2 id={`${id}-list`} className="visually-hidden">
          Accounts
        </h2>
        <div className="card-head card-head-pad accounts-head">
          <div className="segmented" role="group" aria-label="Show">
            {filters.map(([value, label]) => (
              <button key={value} type="button" className="btn" aria-pressed={show === value} onClick={() => setShow(value)}>
                {label} <span className="segmented-count">{counts[value]}</span>
              </button>
            ))}
          </div>
          <ListToolbar label="Filter accounts" query={query} onQuery={setQuery} shown={shown.length} total={accounts.length} noun={['account', 'accounts']} />
        </div>
        <div className="card-body">
          {list.reply?.kind === 'ok' && recent.reply?.kind === 'ok' && accounts.length === 0 ? (
            <p className="hint card-empty">No account is blocked, and no signed-in request was seen recently.</p>
          ) : accounts.length > 0 && shown.length === 0 ? (
            <p className="hint card-empty">No account matches.</p>
          ) : null}
          {shown.length > 0 ? (
            <TableScroll label="Account List">
              <table className="accounts-table">
                <thead>
                  <tr>
                    <th scope="col">Account</th>
                    <th scope="col">Status</th>
                    <th scope="col">Roles</th>
                    <th scope="col">Last Request</th>
                    <th scope="col">Seen</th>
                    {manage ? (
                      <th scope="col">
                        <span className="visually-hidden">Actions</span>
                      </th>
                    ) : null}
                  </tr>
                </thead>
                <tbody>
                  {shown.map((account) => {
                    const b = blocked.get(account)
                    const e = seen.get(account)
                    return (
                      <tr key={account}>
                        <td>
                          <AccountLink account={account}>
                            <code>{account}</code>
                          </AccountLink>
                          {b ? (
                            <span className="cell-sub">
                              {b.reason} · by {b.blocked_by} · <TimeOfDay value={b.blocked_at} />
                            </span>
                          ) : null}
                        </td>
                        <td>{b ? <Badge tone="refused">Blocked</Badge> : <Badge tone="ok">Active</Badge>}</td>
                        <td>
                          {e ? (
                            e.roles.length ? (
                              <span className="chips">
                                {e.roles.map((role) => (
                                  <XRef key={role} view="versions" focus={{ role }}>
                                    <span className="chip">{role}</span>
                                  </XRef>
                                ))}
                              </span>
                            ) : (
                              <span className="muted">None the policy defines</span>
                            )
                          ) : (
                            <span className="muted">—</span>
                          )}
                        </td>
                        <td>
                          {e ? (
                            <>
                              <code className="soft">
                                <span className="method">{e.method}</span> {e.path}
                              </code>
                              <span className={`cell-sub ${e.decision === 'allowed' ? '' : 'refused-word'}`}>
                                {e.decision === 'allowed' ? 'Allowed' : reasonOf(e.code)}
                              </span>
                            </>
                          ) : (
                            <span className="muted">Not in the latest 100 requests</span>
                          )}
                        </td>
                        <td className="nowrap muted">{e ? <TimeOfDay value={e.time} seconds /> : '—'}</td>
                        {manage ? (
                          <td className="row-actions">
                            {b ? (
                              <button type="button" className="btn btn-small" onClick={() => setConfirming(account)} disabled={unblock.pending}>
                                Unblock
                                <span className="visually-hidden"> {account}</span>
                              </button>
                            ) : (
                              <button type="button" className="btn btn-small" onClick={() => openBlock(account)}>
                                Block…
                                <span className="visually-hidden"> {account}</span>
                              </button>
                            )}
                          </td>
                        ) : null}
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </TableScroll>
          ) : null}
        </div>
      </section>
      {manage ? (
        <Confirm
          open={blocking}
          title="Block an Account"
          confirmLabel="Block Now"
          danger
          disabled={block.pending || !subject.trim() || !reason.trim()}
          onCancel={() => setBlocking(false)}
          onConfirm={submit}
        >
          <p>Every request from this account is refused at once, at every endpoint, until it is unblocked.</p>
          <Field id={`${id}-subject`} label="Account ID" hint="As shown in requests, for example cust-002.">
            <input
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
            <input
              id={`${id}-reason`}
              className="input"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              maxLength={200}
              aria-describedby={`${id}-reason-hint`}
            />
          </Field>
        </Confirm>
      ) : null}
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
  // trustedBefore is the key list read before the latest reload was sent,
  // so the result can say which key IDs it added and removed.
  const [trustedBefore, setTrustedBefore] = useState<SigningKey[] | null>(null)
  return (
    <>
      <PageHeader title="Team" lede="Administrators, their permissions and the keys trusted to sign tokens.">
        <ReadStamp
          okAt={admins.last?.at ?? null} failed={admins.failed}
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
          <h2 id="keys-title">Token Signing Keys</h2>
          {can('manage_policy') ? (
            <button
              type="button"
              className="btn btn-small"
              disabled={reload.pending}
              onClick={() => {
                setTrustedBefore(keys.last?.body.items ?? null)
                void reload.run({ method: 'POST', path: '/admin/keys/reload', key: newKey() })
              }}
            >
              {reload.pending ? 'Reloading…' : 'Reload Keys from Settings'}
            </button>
          ) : null}
        </div>
        <div className="card-body">
          <ReplyView reply={keys.reply}>
            {(k) => (
              <TableScroll label="Signing Keys">
                <table>
                  <thead>
                    <tr>
                      <th scope="col">Key ID</th>
                      <th scope="col">Fingerprint</th>
                      <th scope="col">Loaded</th>
                      <th scope="col">Use Since the Gateway Started</th>
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
                        <td>
                          {x.last_verified_at ? (
                            <>
                              <Badge tone="ok">In Use</Badge> <span className="num">{x.verified.toLocaleString()}</span> tokens, last <Time value={x.last_verified_at} />
                            </>
                          ) : (
                            <span className="muted">Not used since the gateway started</span>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableScroll>
            )}
          </ReplyView>
          <div className="card-pad-x">
            <MutationResult mutation={reload} onRefresh={keys.refresh} success={(r) => <KeyChange before={trustedBefore} after={r.items} />} />
          </div>
        </div>
      </section>
      <p className="hint section">
        Signing keys are read from the gateway host, not the policy, so a policy rollback never restores a retired key. Before retiring a key, check it is no longer in use: tokens it signed are refused once it is retired.
      </p>
    </>
  )
}

// KeyChange says what a key reload changed, by key ID and fingerprint only:
// the Dashboard never sees key material.
function KeyChange({ before, after }: { before: SigningKey[] | null; after: SigningKey[] }) {
  const trusted = `${after.length} key${after.length === 1 ? '' : 's'} trusted`
  if (!before) return <p className="notice-title">Reloaded: {trusted}.</p>
  const known = (list: SigningKey[], key: SigningKey) => list.some((k) => k.kid === key.kid && k.fingerprint === key.fingerprint)
  const added = after.filter((key) => !known(before, key))
  const removed = before.filter((key) => !known(after, key))
  return (
    <>
      <p className="notice-title">Reloaded: {trusted}.</p>
      <p>
        {added.length === 0 && removed.length === 0 ? 'The same keys as before: nothing changed.' : null}
        {added.length > 0 ? `Now also trusted: ${added.map((k) => k.kid).join(', ')}. ` : null}
        {removed.length > 0 ? `No longer trusted: ${removed.map((k) => k.kid).join(', ')}; tokens signed with them are refused from the next request.` : null}
      </p>
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
        <h3 id={`impact-${candidate}`}>Impact on Recent Traffic</h3>
        <button type="button" className="btn btn-small" disabled={preview.pending} onClick={() => void preview.run({ method: 'POST', path: `/admin/candidates/${candidate}/impact` })}>
          {preview.pending ? 'Replaying…' : result ? 'Preview Again' : 'Preview Impact'}
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
                      <th scope="col">Endpoint</th>
                      <th scope="col">Account</th>
                      <th scope="col">Now</th>
                      <th scope="col">With This Change</th>
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
        <p className="hint">Replays recent requests against this change. Read-only.</p>
      )}
    </section>
  )
}
