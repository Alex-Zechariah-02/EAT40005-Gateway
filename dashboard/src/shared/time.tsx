import { ArrowsClockwise } from '@phosphor-icons/react'
import { useEffect, useRef, useState } from 'react'
import { activeZone, useZone } from './zone'

// Times on screen: one format for instants, read stamps, ages, durations
// and countdowns.

// staleAfterMs is when a reading counts as out of date on screen and before
// a trial is confirmed.
export const staleAfterMs = 15_000

// age says how long ago a reading arrived, in words.
export function age(at: Date, now = Date.now()): string {
  const seconds = Math.max(0, Math.round((now - at.getTime()) / 1000))
  return seconds < 60 ? `${seconds} s ago` : `${duration(seconds).replace(/ \d+ s$/, '')} ago`
}

// useNow is the browser's clock, updated every second, for ages and
// countdowns.
function useNow(): number {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  return now
}

// ReadStamp shows when the data on the page was last read successfully,
// whether the latest read failed and when it has become old, because what
// the dashboard shows is a copy of server state at that moment. It is not a
// live region: routine rereads are not announced; a failed read is, by the
// view's problem notice.
export function ReadStamp(props: { okAt: Date | null; loading: boolean; failed?: boolean; onRefresh: () => void }) {
  const now = useNow()
  const old = props.okAt !== null && now - props.okAt.getTime() > staleAfterMs
  const text =
    props.okAt === null ? (
      props.loading ? (
        'Reading…'
      ) : props.failed ? (
        'Not read'
      ) : (
        ''
      )
    ) : (
      <>
        Updated <Time value={props.okAt.toISOString()} />
        {old ? ` · ${age(props.okAt, now)}` : ''}
        {props.failed ? ' · latest read failed' : ''}
      </>
    )
  return (
    <p className={props.failed ? 'read-stamp read-stamp-failed' : 'read-stamp'}>
      <span>{text}</span>
      <button type="button" className="btn btn-small" onClick={props.onRefresh} disabled={props.loading}>
        <ArrowsClockwise size={14} aria-hidden="true" />
        Refresh
      </button>
    </p>
  )
}

const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const formats = new Map<string, Intl.DateTimeFormat>()

interface Parts {
  year: number
  month: number
  day: number
  hour: number
  minute: number
  second: number
  // offset is the zone's distance from UTC at that instant, in minutes.
  offset: number
}

// parts reads an instant's calendar and clock fields in one zone. Formats
// are cached per zone, as every table row asks on each refresh; a list of
// every zone passes keep = false so the cache holds only zones in use.
function parts(date: Date, zone: string, keep = true): Parts {
  let format = formats.get(zone)
  if (!format) {
    format = new Intl.DateTimeFormat('en-US', { timeZone: zone, hourCycle: 'h23', year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' })
    if (keep) formats.set(zone, format)
  }
  const field: Record<string, number> = {}
  for (const part of format.formatToParts(date)) field[part.type] = Number(part.value)
  const wall = Date.UTC(field.year, field.month - 1, field.day, field.hour, field.minute, field.second)
  const offset = Math.round((wall - Math.floor(date.getTime() / 1000) * 1000) / 60_000)
  return { year: field.year, month: field.month, day: field.day, hour: field.hour, minute: field.minute, second: field.second, offset }
}

const pad = (n: number) => String(n).padStart(2, '0')

function dateText(p: Parts): string {
  return `${p.day} ${months[p.month - 1]} ${p.year}`
}

function clockText(p: Parts, seconds = true): string {
  return seconds ? `${pad(p.hour)}:${pad(p.minute)}:${pad(p.second)}` : `${pad(p.hour)}:${pad(p.minute)}`
}

function offsetText(offset: number): string {
  const size = Math.abs(offset)
  return `UTC${offset < 0 ? '-' : '+'}${pad(Math.floor(size / 60))}:${pad(size % 60)}`
}

function valid(iso: string | undefined): Date | null {
  if (!iso) return null
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? null : date
}

// The one time format: 24-hour, day first, with the zone, for example
// "3 Nov 2026, 20:08:12 UTC+08:00". Text that is not a time is shown as it is.
export function time(iso: string | undefined, zone = activeZone()): string {
  const date = valid(iso)
  if (!date) return iso ?? ''
  const p = parts(date, zone)
  return `${dateText(p)}, ${clockText(p)} ${offsetText(p.offset)}`
}

// compact is the full form without the zone, and without the date when it
// is today, for dense tables whose column header names the zone once.
export function compact(iso: string, zone = activeZone()): string {
  const date = valid(iso)
  if (!date) return iso
  const p = parts(date, zone)
  return dateText(p) === dateText(parts(new Date(), zone)) ? clockText(p) : `${dateText(p)}, ${clockText(p)}`
}

// axis is only the clock, for chart axis labels; the chart names the zone.
export function axis(iso: string, seconds: boolean, zone = activeZone()): string {
  const date = valid(iso)
  return date ? clockText(parts(date, zone), seconds) : iso
}

// zoneLabel is a zone's distance from UTC now, as "UTC+08:00".
export function zoneLabel(zone = activeZone(), keep = true): string {
  return offsetText(parts(new Date(), zone, keep).offset)
}

// instant reads a wall time typed in a zone ("2026-11-03T10:00") as an
// RFC 3339 instant. Reading the offset twice settles a wall time near a
// clock change.
export function instant(local: string, zone = activeZone()): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(local)
  if (!match) return ''
  const [year, month, day, hour, minute, second] = match.slice(1).map((n) => Number(n ?? 0))
  const wall = Date.UTC(year, month - 1, day, hour, minute, second)
  const guess = parts(new Date(wall), zone).offset
  const settled = parts(new Date(wall - guess * 60_000), zone).offset
  return new Date(wall - settled * 60_000).toISOString()
}

// Time shows a server instant in the chosen zone. Pointing at it or
// focusing it shows the same moment in UTC.
export function Time(props: { value: string | undefined; compact?: boolean }) {
  const zone = useZone()
  if (!props.value) return null
  if (!valid(props.value)) return <>{props.value}</>
  return (
    <time dateTime={props.value} tabIndex={0} data-tip={time(props.value, 'UTC')} className={props.compact ? 'compact' : undefined}>
      {props.compact ? compact(props.value, zone) : time(props.value, zone)}
    </time>
  )
}

// Zone names the zone times are shown in, for a column header or a hint.
export function Zone() {
  return <>{zoneLabel(useZone())}</>
}

export function duration(seconds: number): string {
  if (seconds < 60) return `${seconds} s`
  const minutes = Math.floor(seconds / 60)
  // A zero part is left out: "10 min", not "10 min 0 s".
  if (minutes < 60) return seconds % 60 ? `${minutes} min ${seconds % 60} s` : `${minutes} min`
  return minutes % 60 ? `${Math.floor(minutes / 60)} h ${minutes % 60} min` : `${Math.floor(minutes / 60)} h`
}

// The rereads after a deadline shown here passes, in milliseconds after it:
// the gateway ends the trial or rule by its own timer, which may run a
// little after this computer's clock.
const rereadsAfterEnd = [500, 2_000, 5_000, 10_000, 20_000]

// Remaining shows the time left until a server-provided instant. It is a
// display aid only: the gateway decides when a deadline or expiry has
// passed. Once the instant passes it calls onPassed a few times, so the view
// rereads the server's state until the gateway reports the end (the view then
// no longer shows this countdown).
export function Remaining(props: { until: string; onPassed: () => void; format?: 'words' | 'clock' }) {
  const end = new Date(props.until).getTime()
  const now = useNow()
  const rereads = useRef(0)
  useEffect(() => {
    if (rereads.current < rereadsAfterEnd.length && now >= end + rereadsAfterEnd[rereads.current]) {
      rereads.current++
      props.onPassed()
    }
  })
  const seconds = Math.max(0, Math.round((end - now) / 1000))
  if (props.format === 'clock') {
    const pad = (n: number) => String(n).padStart(2, '0')
    const text = seconds >= 3600 ? `${Math.floor(seconds / 3600)}:${pad(Math.floor(seconds / 60) % 60)}:${pad(seconds % 60)}` : `${pad(Math.floor(seconds / 60))}:${pad(seconds % 60)}`
    return <span className="countdown">{text}</span>
  }
  if (seconds > 0) return <span className="remaining">{duration(seconds)} left</span>
  return (
    <span className="remaining">
      {rereads.current < rereadsAfterEnd.length ? 'time reached; waiting for the gateway to end it' : 'time reached, but the gateway has not reported it ended; refresh'}
    </span>
  )
}
