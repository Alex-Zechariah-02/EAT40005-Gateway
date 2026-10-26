import { Check, Clock, X, QuestionMark, Minus, Plus, ArrowRight, ArrowsClockwise, Info, MagnifyingGlass } from '@phosphor-icons/react'
import { Fragment, useEffect, useId, useRef, useState, type ReactNode } from 'react'
import { explain, type Outcome, type Reply } from './api'
import type { Change } from './diff'
import type { Mutation } from './hooks'

// Tone is the meaning of a status: in force or done, provisional or
// time-bound, refused or blocking, unknown outcome, or inactive. Every badge
// also carries a word and a shape, so colour is never the only signal.
export type Tone = 'ok' | 'pending' | 'refused' | 'unknown' | 'neutral'

const glyphs = { ok: Check, pending: Clock, refused: X, unknown: QuestionMark, neutral: Minus, plus: Plus, minus: Minus }

export function Glyph({ name }: { name: keyof typeof glyphs }) {
  const Component = glyphs[name]
  return <Component className="glyph" size={16} weight="regular" aria-hidden="true" focusable="false" />
}

export function Badge(props: { tone: Tone; glyph?: keyof typeof glyphs; children: ReactNode }) {
  return (
    <span className={`badge tone-${props.tone}`}>
      <Glyph name={props.glyph ?? props.tone} />
      {props.children}
    </span>
  )
}

// Tags shows names such as roles or operation ids, each its own element,
// separated by commas as in a sentence.
export function Tags(props: { items: string[]; code?: boolean }) {
  return (
    <span className="tags">
      {props.items.map((item, i) => (
        <Fragment key={item}>
          {i > 0 ? ', ' : null}
          {props.code ? <code className="tag">{item}</code> : <span className="tag">{item}</span>}
        </Fragment>
      ))}
    </span>
  )
}

// The library arrow has a text equivalent for copying and screen readers.
export function Arrowed({ text }: { text: string }) {
  const parts = text.split('→')
  return (
    <>
      {parts.map((part, i) => (
        <Fragment key={i}>
          {i > 0 ? (
            <>
              <ArrowRight className="becomes" size={16} weight="regular" aria-hidden="true" />
              <span className="visually-hidden">→</span>
            </>
          ) : null}
          {part}
        </Fragment>
      ))}
    </>
  )
}

// PageHeader is the bar at the top of every view: its one short title (h1),
// the explanation behind an info button rather than a subtitle, the view's
// actions, and the search for a request by its Ref.
export function PageHeader(props: { title: string; lede?: ReactNode; search?: boolean; children?: ReactNode }) {
  const [about, setAbout] = useState(false)
  const aboutId = useId()
  return (
    <>
      <header className="page-head">
        <div className="page-title">
          <h1 id="page-title" tabIndex={-1}>
            {props.title}
          </h1>
          {props.lede ? (
            <button type="button" className="icon-button" aria-expanded={about} aria-controls={aboutId} onClick={() => setAbout((open) => !open)}>
              <Info size={18} aria-hidden="true" />
              <span className="visually-hidden">About this page</span>
            </button>
          ) : null}
        </div>
        {props.children ? <div className="page-actions">{props.children}</div> : null}
        {props.search === false ? null : <RefSearch />}
      </header>
      {props.lede ? (
        <p id={aboutId} className="page-about" hidden={!about}>
          {props.lede}
        </p>
      ) : null}
    </>
  )
}

// RefSearch opens Request lookup for the reference a person quotes from an
// error message.
function RefSearch() {
  const [ref, setRef] = useState('')
  return (
    <form
      className="ref-search"
      role="search"
      aria-label="Find a request"
      onSubmit={(event) => {
        event.preventDefault()
        const value = ref.trim()
        if (value) window.location.hash = `#/lookup?ref=${encodeURIComponent(value)}`
      }}
    >
      <MagnifyingGlass size={16} aria-hidden="true" />
      <input
        type="search"
        aria-label="Request reference (Ref)"
        placeholder="Find a request by its Ref"
        value={ref}
        onChange={(event) => setRef(event.target.value)}
        maxLength={64}
        spellCheck={false}
        autoComplete="off"
      />
    </form>
  )
}

// ReadStamp shows when the data on the page was read and whether a newer read
// is under way, because what the dashboard shows is a copy of server state at
// that moment.
export function ReadStamp(props: { readAt: Date | null; loading: boolean; onRefresh: () => void }) {
  return (
    <p className="read-stamp" role="status">
      <span>{props.loading ? 'Reading…' : props.readAt ? `Read ${clock(props.readAt.toISOString(), true)}` : ''}</span>
      <button type="button" className="btn btn-small" onClick={props.onRefresh} disabled={props.loading}>
        <ArrowsClockwise size={14} aria-hidden="true" />
        Refresh
      </button>
    </p>
  )
}

// Problem describes a refused or unanswered call.
export function Problem<T>(props: { reply: Reply<T> }) {
  const r = props.reply
  if (r.kind === 'unknown') {
    return (
      <div role="alert" className="notice tone-unknown">
        <p className="notice-title">
          <Glyph name="unknown" />
          No answer from the gateway: {r.message}.
        </p>
      </div>
    )
  }
  if (r.kind !== 'refused') return null
  const code = r.outcome?.reason ?? r.error
  const field = r.outcome?.field ?? r.field
  const explanation = explain(code)
  return (
    <div role="alert" className="notice tone-refused">
      <p className="notice-title">
        <Glyph name="refused" />
        <span>
          Refused (HTTP {r.status}): <code>{code}</code>
        </span>
      </p>
      {explanation ? <p>{explanation}</p> : null}
      {field ? (
        <p>
          Field: <code>{field}</code>
        </p>
      ) : null}
    </div>
  )
}

// ShowMore reads the next page of a list, and says why when it cannot.
export function ShowMore(props: { list: { next?: string; paging: boolean; pageProblem: Reply<unknown> | null; more: () => void } }) {
  const { list } = props
  return (
    <>
      {list.pageProblem ? <Problem reply={list.pageProblem} /> : null}
      {list.next ? (
        <button type="button" className="btn show-more" onClick={list.more} disabled={list.paging}>
          {list.paging ? 'Reading…' : 'Show more'}
        </button>
      ) : null}
    </>
  )
}

// ReplyView renders a loaded resource or the reason it could not be read.
export function ReplyView<T>(props: { reply: Reply<T> | null; children: (body: T) => ReactNode }) {
  const r = props.reply
  if (r === null) return null
  if (r.kind === 'ok') return <>{props.children(r.body)}</>
  return <Problem reply={r} />
}

const receiptFields: [keyof Outcome, string][] = [
  ['outcome', 'Outcome'],
  ['disposition', 'Final disposition'],
  ['reason', 'Reason'],
  ['field', 'Field'],
  ['transition_id', 'Transition'],
  ['revision', 'Revision now in force'],
  ['generation', 'Effective generation'],
  ['candidate', 'Candidate'],
  ['provisional', 'Provisional revision'],
  ['rollback_target', 'Rollback target'],
  ['deadline', 'Confirmation deadline'],
  ['override', 'Override'],
  ['target', 'Target'],
  ['expires_at', 'Expires'],
]

// Receipt shows the controller's record of a mutation exactly as returned.
export function Receipt(props: { outcome: Outcome }) {
  return (
    <dl className="facts">
      {receiptFields
        .filter(([key]) => props.outcome[key] !== undefined && props.outcome[key] !== '')
        .map(([key, label]) => (
          <div key={key} className="fact">
            <dt>{label}</dt>
            <dd>
              <code>{String(props.outcome[key])}</code>
            </dd>
          </div>
        ))}
    </dl>
  )
}

// MutationResult reports a mutation's state in a live region.
export function MutationResult<T>(props: {
  mutation: Mutation<T>
  success: (body: T) => ReactNode
  onRefresh: () => void
  unknown?: ReactNode
}) {
  const { phase, run } = props.mutation
  return (
    <div aria-live="polite" className="mutation-result">
      {phase.state === 'pending' ? (
        <p className="notice tone-pending">
          <span className="notice-title">
            <Glyph name="pending" />
            Sent; waiting for the gateway's answer…
          </span>
        </p>
      ) : null}
      {phase.state === 'replied' && phase.reply.kind === 'ok' ? (
        <div className="notice tone-ok">{props.success(phase.reply.body)}</div>
      ) : null}
      {phase.state === 'replied' && phase.reply.kind === 'refused' ? <Problem reply={phase.reply} /> : null}
      {phase.state === 'replied' && phase.reply.kind === 'unknown'
        ? (props.unknown ?? (
            <div role="alert" className="notice tone-unknown">
              <p className="notice-title">
                <Glyph name="unknown" />
                Outcome unknown.
              </p>
              <p>
                The request was sent but no answer arrived ({phase.reply.message}). The gateway may or may not have applied it. Refresh to
                see the current state before doing anything else.
              </p>
              {phase.request.key !== undefined ? (
                <p>
                  Sending the identical request again is safe: it carries the same request key, so the gateway returns its recorded result
                  or applies it once.
                </p>
              ) : null}
              <div className="actions">
                <button type="button" className="btn" onClick={props.onRefresh}>
                  Refresh state
                </button>
                {phase.request.key !== undefined ? (
                  <button type="button" className="btn" onClick={() => run(phase.request)}>
                    Send the identical request again
                  </button>
                ) : null}
              </div>
            </div>
          ))
        : null}
    </div>
  )
}

// Confirm asks for explicit confirmation of a policy-changing action in a
// native modal dialog, which keeps keyboard focus inside until it closes.
// Focus starts on Cancel, so pressing Enter by habit changes nothing.
export function Confirm(props: {
  open: boolean
  title: string
  confirmLabel: string
  danger?: boolean
  onConfirm: () => void
  onCancel: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDialogElement>(null)
  const titleId = useId()
  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (props.open && !dialog.open) dialog.showModal()
    if (!props.open && dialog.open) dialog.close()
  }, [props.open])

  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      onCancel={(event) => {
        event.preventDefault()
        props.onCancel()
      }}
    >
      <h2 id={titleId}>{props.title}</h2>
      <div className="dialog-body">{props.children}</div>
      <div className="actions dialog-actions">
        <button type="button" className="btn" autoFocus onClick={props.onCancel}>
          Cancel
        </button>
        <button type="button" className={`btn ${props.danger ? 'btn-danger' : 'btn-primary'}`} onClick={props.onConfirm}>
          {props.confirmLabel}
        </button>
      </div>
    </dialog>
  )
}

// Field lays out one labelled input with its hint and error text; the input
// itself refers to them with describedBy.
export function Field(props: { id: string; label: string; hint?: string; error?: string; wide?: boolean; children: ReactNode }) {
  return (
    <div className={props.wide ? 'field field-wide' : 'field'}>
      <label htmlFor={props.id}>{props.label}</label>
      {props.children}
      {props.hint ? (
        <p className="hint" id={`${props.id}-hint`}>
          {props.hint}
        </p>
      ) : null}
      {props.error ? (
        <p className="field-error" id={`${props.id}-error`} role="alert">
          <Glyph name="refused" />
          {props.error}
        </p>
      ) : null}
    </div>
  )
}

export function describedBy(id: string, hint?: string, error?: string): string | undefined {
  const ids = [hint ? `${id}-hint` : '', error ? `${id}-error` : ''].filter(Boolean)
  return ids.length ? ids.join(' ') : undefined
}

// fieldError returns the server's error text for one form field, if the
// server named that field.
export function fieldError<T>(reply: Reply<T> | undefined, field: string): string | undefined {
  if (!reply || reply.kind !== 'refused') return undefined
  const named = reply.outcome?.field ?? reply.field
  if (named !== field) return undefined
  const code = reply.outcome?.reason ?? reply.error
  return `${code}: ${explain(code) || 'not accepted'}`
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
  if (minutes < 60) return `${minutes} min ${seconds % 60} s`
  return `${Math.floor(minutes / 60)} h ${minutes % 60} min`
}

// Remaining shows the time left until a server-provided instant. It is a
// display aid only: the gateway decides when a deadline or expiry has
// passed. When the instant passes it calls onPassed once, so the view rereads
// the server's state.
export function Remaining(props: { until: string; onPassed: () => void; format?: 'words' | 'clock' }) {
  const end = new Date(props.until).getTime()
  const [now, setNow] = useState(Date.now())
  const passed = useRef(false)
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  useEffect(() => {
    if (now >= end + 500 && !passed.current) {
      passed.current = true
      props.onPassed()
    }
  })
  const seconds = Math.max(0, Math.round((end - now) / 1000))
  if (props.format === 'clock') {
    const pad = (n: number) => String(n).padStart(2, '0')
    const text = seconds >= 3600 ? `${Math.floor(seconds / 3600)}:${pad(Math.floor(seconds / 60) % 60)}:${pad(seconds % 60)}` : `${pad(Math.floor(seconds / 60))}:${pad(seconds % 60)}`
    return <span className="countdown">{text}</span>
  }
  return <span className="remaining">{seconds > 0 ? `${duration(seconds)} left` : 'time reached; rereading the gateway'}</span>
}

// Empty is what a list or view shows when there is nothing in it, with the
// way forward where there is one.
export function Empty(props: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <p className="empty-title">{props.title}</p>
      {props.children}
    </div>
  )
}

// ChangeList shows policy differences, each item with the members that
// differ.
export function ChangeList({ changes }: { changes: Change[] }) {
  return (
    <ul className="changes changes-columns">
      {changes.map((change) => (
        <li key={change.text} className={`change change-${change.kind}`}>
          <span className="change-head">
            <Arrowed text={change.text} />
          </span>
          {change.details.length > 0 ? (
            <ul className="change-details">
              {change.details.map((detail) => (
                <li key={detail}>
                  <Arrowed text={detail} />
                </li>
              ))}
            </ul>
          ) : null}
        </li>
      ))}
    </ul>
  )
}

// TableScroll lets a wide table scroll sideways inside the page on a narrow
// screen. It can take keyboard focus, so it can be scrolled without a mouse.
export function TableScroll(props: { label: string; children: ReactNode }) {
  return (
    <div className="table-scroll" role="region" aria-label={props.label} tabIndex={0}>
      {props.children}
    </div>
  )
}

// CodeBlock shows text exactly as written, in a block that scrolls when the
// text is long. It can take keyboard focus, so it can be scrolled without a
// mouse.
export function CodeBlock(props: { label: string; className?: string; children: ReactNode }) {
  return (
    <pre className={props.className} role="region" aria-label={props.label} tabIndex={0}>
      {props.children}
    </pre>
  )
}
