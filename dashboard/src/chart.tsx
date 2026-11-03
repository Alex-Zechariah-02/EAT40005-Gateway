import { useId, useMemo, useState } from 'react'
import { Empty, TableScroll } from './ui'
import { TimeOfDay } from './time'
import type { Traffic } from './summary'

// The traffic chart: requests per interval with response times under the bars.

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
