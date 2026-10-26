import { useId, useState } from 'react'
import { cx } from './cx'

// A small bar chart drawn in SVG (presentation attributes, not style
// attributes, so the content policy holds). Thin bars with rounded data ends
// on a shared baseline, a recessive grid, a legend for two series, the value
// of the bar under the pointer or focus, and a table view of the same data.

export interface Series {
  name: string
  values: number[]
  // fill and swatch are utility classes from the chart tokens.
  fill: string
  swatch: string
}

const width = 720
const height = 220
const top = 12
const bottom = 28
const left = 56

function niceMax(value: number): number {
  if (value <= 0) return 1
  const power = 10 ** Math.floor(Math.log10(value))
  const step = [1, 2, 2.5, 5, 10].find((s) => s * power >= value / 4)! * power
  return Math.ceil(value / step) * step
}

// bar draws a column whose top corners are rounded and whose base sits flat
// on the baseline.
function bar(x: number, y: number, w: number, h: number): string {
  const r = Math.min(3, w / 2, h)
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`
}

export function BarChart({ label, categories, series, format, every = 1 }: { label: string; categories: string[]; series: Series[]; format: (value: number) => string; every?: number }) {
  const id = useId()
  const [active, setActive] = useState<number | null>(null)
  const max = niceMax(Math.max(...series.flatMap((s) => s.values)))
  const plotW = width - left - 8
  const plotH = height - top - bottom
  const band = plotW / categories.length
  const gap = 2
  const barW = Math.max(2, Math.min(22, (band * 0.66 - gap * (series.length - 1)) / series.length))
  const groupW = barW * series.length + gap * (series.length - 1)
  const y = (v: number) => top + plotH - (v / max) * plotH
  const ticks = [0, max / 2, max]
  const shown = active ?? null

  return (
    <figure className="m-0">
      <div className="mb-2 flex min-h-6 flex-wrap items-center justify-between gap-3">
        {series.length > 1 ? (
          <ul className="flex gap-4 text-xs font-semibold text-ink-2" aria-label="Legend">
            {series.map((s) => <li key={s.name} className="flex items-center gap-1.5"><span className={cx('size-2.5 rounded-sm', s.swatch)} aria-hidden="true" />{s.name}</li>)}
          </ul>
        ) : <span />}
        <p className="text-xs text-ink-2 tabular-nums" aria-live="polite">
          {shown !== null ? <><span className="font-bold text-ink">{categories[shown]}</span>{series.map((s) => ` · ${series.length > 1 ? `${s.name} ` : ''}${format(s.values[shown])}`).join('')}</> : <span className="text-ink-3">Point at a bar for its value</span>}
        </p>
      </div>
      <svg viewBox={`0 0 ${width} ${height}`} className="h-auto w-full overflow-visible" role="img" aria-labelledby={`${id}-title`} onMouseLeave={() => setActive(null)}>
        <title id={`${id}-title`}>{label}</title>
        {ticks.map((t) => (
          <g key={t}>
            <line x1={left} x2={width - 8} y1={y(t)} y2={y(t)} className="stroke-line" strokeWidth={1} />
            <text x={left - 8} y={y(t) + 4} textAnchor="end" className="fill-ink-3 text-[11px] tabular-nums">{format(t)}</text>
          </g>
        ))}
        {categories.map((category, i) => {
          const x0 = left + band * i + (band - groupW) / 2
          return (
            // Bars answer the pointer only: the SVG is one image to assistive
            // technology, and the table below gives every value by keyboard.
            <g key={category} onMouseEnter={() => setActive(i)}>
              <rect x={left + band * i} y={top} width={band} height={plotH} className={cx(active === i ? 'fill-sunken' : 'fill-transparent')} />
              {series.map((s, k) => {
                const v = s.values[i]
                const h = Math.max(v > 0 ? 1.5 : 0, (v / max) * plotH)
                return <path key={s.name} d={bar(x0 + k * (barW + gap), top + plotH - h, barW, h)} className={s.fill} />
              })}
              {i % every === 0 && <text x={left + band * i + band / 2} y={height - 8} textAnchor="middle" className="fill-ink-3 text-[11px]">{category}</text>}
            </g>
          )
        })}
      </svg>
      <details className="mt-2 text-sm">
        <summary className="cursor-pointer rounded font-semibold text-brand">Show as table</summary>
        <div className="mt-2 max-h-72 overflow-auto rounded-xl ring-1 ring-line">
          <table className="w-full text-left text-sm tabular-nums">
            <thead className="sticky top-0 bg-sunken text-xs text-ink-3"><tr><th className="px-3 py-2 font-semibold">Period</th>{series.map((s) => <th key={s.name} className="px-3 py-2 text-right font-semibold">{s.name}</th>)}</tr></thead>
            <tbody className="divide-y divide-line">
              {categories.map((c, i) => <tr key={c}><td className="px-3 py-1.5">{c}</td>{series.map((s) => <td key={s.name} className="px-3 py-1.5 text-right">{format(s.values[i])}</td>)}</tr>)}
            </tbody>
          </table>
        </div>
      </details>
    </figure>
  )
}
