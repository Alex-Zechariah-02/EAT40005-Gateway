import type { ReactNode } from 'react'
import type { Resource } from '../shared/hooks'
import { AccountLink, XRef } from '../shared/links'
import { TimeOfDay } from '../shared/time'
import { reasonOf } from './requests'
import { TrafficChart } from './chart'

// Traffic summary cards: figures, refusal reasons and busiest endpoints for a window.

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
export function TrafficCards(props: {
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
