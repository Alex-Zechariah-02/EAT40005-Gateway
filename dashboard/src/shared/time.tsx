import { ArrowsClockwise } from '@phosphor-icons/react'
import { useEffect, useRef, useState } from 'react'

// Times on screen: read stamps, dates, times of day, durations and countdowns.

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
    props.okAt === null
      ? props.loading
        ? 'Reading…'
        : props.failed
          ? 'Not read'
          : ''
      : `Updated ${clock(props.okAt.toISOString(), true)}${old ? ` · ${age(props.okAt, now)}` : ''}${props.failed ? ' · latest read failed' : ''}`
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

const timeFormat: Intl.DateTimeFormatOptions = {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
}

// time formats a server instant in the browser's locale and time zone.
export function time(iso: string | undefined): string {
  if (!iso) return ''
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? iso : date.toLocaleString(undefined, timeFormat)
}

export function Time({ value }: { value: string | undefined }) {
  if (!value) return null
  return <time dateTime={value}>{time(value)}</time>
}

// clock gives the time of day of an instant, with the date only when it is
// not today: "13:02", "13:02:41", "27 Oct 13:02".
export function clock(iso: string, seconds = false): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  const hm = date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: seconds ? '2-digit' : undefined, hourCycle: 'h23' })
  return date.toDateString() === new Date().toDateString() ? hm : `${date.toLocaleDateString([], { day: 'numeric', month: 'short' })} ${hm}`
}

export function TimeOfDay(props: { value: string; seconds?: boolean }) {
  return <time dateTime={props.value}>{clock(props.value, props.seconds)}</time>
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
