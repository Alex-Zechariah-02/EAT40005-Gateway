import { Check, Clock, X, QuestionMark, Minus, Plus, ArrowRight, ArrowsClockwise, Info, MagnifyingGlass } from '@phosphor-icons/react'
import { Fragment, useContext, useEffect, useId, useRef, useState, type ReactNode } from 'react'
import { committedAnyway, committedMessage, message, type Outcome, type Reply } from './api'
import { plainChange, type Change } from './diff'
import { Navigate, ViewMemory, type Mutation } from './hooks'

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
        {props.search === false ? null : <GlobalSearch />}
      </header>
      {props.lede ? (
        <p id={aboutId} className="page-about" hidden={!about}>
          {props.lede}
        </p>
      ) : null}
    </>
  )
}

// Each search hands Request lookup a new address, so a repeated search starts
// again; the counter carries no search term.
let searches = 0

// GlobalSearch finds what an administrator is given: a request's full Ref,
// an account ID, an endpoint ID, a draft change ("change 12"), a temporary
// rule ("rule 3") or a version ("version 4"). An account or endpoint goes to
// Request lookup through the page's memory, so it never enters the address
// or the browser history. It only opens pages; each page reads the gateway.
function GlobalSearch() {
  const [text, setText] = useState('')
  const [hint, setHint] = useState('')
  const memory = useContext(ViewMemory)
  const navigate = useContext(Navigate)
  const lookup = (field: 'subject' | 'operation', value: string) => {
    memory?.set('lookup', {
      form: { ref: '', subject: '', operation: '', decision: '', [field]: value },
      query: `${field}=${encodeURIComponent(value)}`,
    })
    searches += 1
    window.location.hash = `#/lookup?search=${searches}`
  }
  const find = (value: string) => {
    let match: RegExpExecArray | null
    if (/^[0-9a-f]{32}$/i.test(value)) window.location.hash = `#/lookup?ref=${encodeURIComponent(value.toLowerCase())}`
    else if (/^[0-9a-f]{4}-[0-9a-f]{4}$/i.test(value))
      return 'That is a short Ref. Ask for the full reference: the copy button beside a Ref copies all 32 characters.'
    else if ((match = /^(?:draft\s+)?change\s*#?(\d+)$/i.exec(value))) navigate?.('candidates', { candidate: Number(match[1]) })
    else if (/^(?:temporary\s+)?rule\s*#?\d+$/i.test(value)) navigate?.('overrides')
    else if (/^version\s*#?\d+$/i.test(value)) navigate?.('policy')
    else if (/^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+)+$/i.test(value)) lookup('operation', value)
    else if (/^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,63}$/.test(value)) lookup('subject', value)
    else return 'Search finds a full Ref, an account ID, an endpoint ID, “change 12”, “rule 3” or “version 4”.'
    return ''
  }
  return (
    <form
      className="ref-search"
      role="search"
      aria-label="Search the gateway"
      onSubmit={(event) => {
        event.preventDefault()
        const value = text.trim()
        if (value) setHint(find(value))
      }}
    >
      <MagnifyingGlass size={16} aria-hidden="true" />
      <input
        type="search"
        aria-label="Search"
        aria-describedby={hint ? 'search-hint' : undefined}
        placeholder="Ref, account, endpoint, change 12, rule 3"
        value={text}
        onChange={(event) => {
          setText(event.target.value)
          setHint('')
        }}
        maxLength={64}
        spellCheck={false}
        autoComplete="off"
      />
      {hint ? (
        <p id="search-hint" role="status" className="search-hint">
          {hint}
        </p>
      ) : null}
    </form>
  )
}

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
export function useNow(): number {
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

// CopyButton copies one exact value, such as a code or an ID, and says so.
export function CopyButton(props: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      className="btn btn-small btn-copy"
      onClick={() =>
        navigator.clipboard.writeText(props.value).then(
          () => setCopied(true),
          () => setCopied(false),
        )
      }
    >
      {copied ? 'Copied' : 'Copy'}
      <span className="visually-hidden"> {props.label}</span>
    </button>
  )
}

// MoreDetails holds the exact facts behind a plain message, collapsed until
// asked for, each value copyable.
export function MoreDetails(props: { facts: [string, string | undefined][] }) {
  const facts = props.facts.filter((fact): fact is [string, string] => fact[1] !== undefined && fact[1] !== '')
  if (facts.length === 0) return null
  return (
    <details className="more-details">
      <summary>More details</summary>
      <dl className="facts facts-compact">
        {facts.map(([label, value]) => (
          <div key={label} className="fact">
            <dt>{label}</dt>
            <dd>
              <code>{value}</code> <CopyButton value={value} label={label} />
            </dd>
          </div>
        ))}
      </dl>
    </details>
  )
}

// Problem describes a refused or unanswered call: what happened in plain
// words, what to do, and the exact code, status and field under More
// details. Refused, applied with a problem and no answer keep their own
// titles and tones, because each means something different for a change.
export function Problem<T>(props: { reply: Reply<T> }) {
  const r = props.reply
  if (r.kind === 'unknown') {
    return (
      <div role="alert" className="notice tone-unknown">
        <p className="notice-title">
          <Glyph name="unknown" />
          No answer from the gateway
        </p>
        <p>Check that the gateway is running, then refresh. If you were making a change, check whether it was applied before sending it again.</p>
        <MoreDetails facts={[['Reason', r.message]]} />
      </div>
    )
  }
  if (r.kind !== 'refused') return null
  const code = r.outcome?.reason ?? r.error
  const field = r.outcome?.field ?? r.field
  // A committed change whose audit record or publication failed is
  // reported with an error status, but it was applied: never "Refused".
  const committed = committedAnyway(r)
  const words = committed ? committedMessage(code) : message(code)
  return (
    <div role="alert" className={`notice ${committed ? 'tone-pending' : 'tone-refused'}`}>
      <p className="notice-title">
        <Glyph name={committed ? 'pending' : 'refused'} />
        <span>{words.title}</span>
      </p>
      <p>{words.action}</p>
      <MoreDetails
        facts={[
          ['Error code', code],
          ['HTTP status', String(r.status)],
          ['Field', field],
          ['Transition ID', committed ? r.outcome?.transition_id : undefined],
        ]}
      />
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
  ['disposition', 'Result (kept or undone)'],
  ['reason', 'Reason code'],
  ['field', 'Field'],
  ['transition_id', 'Transition'],
  ['revision', 'Version now in force'],
  ['generation', 'Generation in force'],
  ['candidate', 'Draft change'],
  ['provisional', 'Version on trial'],
  ['rollback_target', 'Version restored if undone'],
  ['deadline', 'Trial deadline'],
  ['override', 'Temporary rule'],
  ['target', 'Target'],
  ['expires_at', 'Expires'],
]

// Receipt shows the controller's record of a mutation exactly as returned.
export function Receipt(props: { outcome: Outcome }) {
  return (
    <MoreDetails facts={receiptFields.map(([key, label]) => [label, props.outcome[key] === undefined ? undefined : String(props.outcome[key])])} />
  )
}

// actorName shows the gateway's own automatic actions as the gateway's.
export function actorName(actor: string | undefined): string {
  return actor === 'controller' ? 'the gateway' : (actor ?? '')
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
                No answer from the gateway: outcome unknown
              </p>
              <p>
                The request was sent but no answer arrived. The gateway may or may not have applied it. Refresh to see the current state before doing
                anything else.
              </p>
              {phase.request.key !== undefined ? (
                <p>
                  Sending the identical request again is safe: it carries the same request key, so the gateway returns its recorded result
                  or applies it once. Until its outcome is known it stays listed at the top of every page.
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
              <MoreDetails
                facts={[
                  ['Reason', phase.reply.message],
                  ['Request key', phase.request.key],
                ]}
              />
            </div>
          ))
        : null}
    </div>
  )
}

// Confirm asks for explicit confirmation of a policy-changing action in a
// native modal dialog, which keeps keyboard focus inside until it closes.
// Focus starts on Cancel, so pressing Enter by habit changes nothing.
// TypedConfirmation asks for a word to be typed before the riskiest actions;
// the dialog's confirm button stays held back until it matches.
export function TypedConfirmation(props: { word: string; value: string; onChange: (value: string) => void }) {
  const id = useId()
  return (
    <div className="field">
      <label htmlFor={id}>
        Type <strong>{props.word}</strong> to confirm
      </label>
      <input
        id={id}
        className="input mono"
        value={props.value}
        onChange={(event) => props.onChange(event.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  )
}

export function Confirm(props: {
  open: boolean
  title: string
  confirmLabel: string
  danger?: boolean
  // disabled holds the confirm button back, for example until the facts it
  // depends on have been read again.
  disabled?: boolean
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
        <button type="button" className={`btn ${props.danger ? 'btn-danger' : 'btn-primary'}`} onClick={props.onConfirm} disabled={props.disabled}>
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
  const words = message(code)
  return `${words.title}. ${words.action} (${code})`
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

const changeKinds = [
  ['added', 'Added'],
  ['changed', 'Changed'],
  ['removed', 'Removed'],
] as const
// The review shows this many sentences per kind; the page lists them all.
const reviewLength = 8

// ChangeSet is the review of an edit: what it adds, changes and removes, in
// plain sentences, from the same differences the change lists show.
export function ChangeSet({ changes }: { changes: Change[] }) {
  return (
    <div className="change-set">
      {changeKinds.map(([kind, label]) => {
        const list = changes.filter((change) => change.kind === kind)
        if (list.length === 0) return null
        return (
          <section key={kind} aria-label={`${label} (${list.length})`}>
            <h3>
              {label} ({list.length})
            </h3>
            <ul>
              {list.slice(0, reviewLength).map((change) => (
                <li key={change.text}>{plainChange(change)}</li>
              ))}
            </ul>
            {list.length > reviewLength ? <p className="hint">And {list.length - reviewLength} more, listed on the page.</p> : null}
          </section>
        )
      })}
    </div>
  )
}

// matches says whether any of the values contains the filter text, ignoring
// case. An empty filter matches everything.
export function matches(query: string, ...values: (string | number | undefined)[]): boolean {
  const wanted = query.trim().toLowerCase()
  return !wanted || values.some((value) => value !== undefined && String(value).toLowerCase().includes(wanted))
}

// ListToolbar heads a list: a filter over the rows already read, how many
// rows it leaves, and the list's main action on the right. The filter only
// narrows what is shown; it asks the gateway for nothing.
export function ListToolbar(props: {
  label: string
  query: string
  onQuery: (query: string) => void
  shown: number
  total: number
  noun: [one: string, many: string]
  // more says further rows exist that have not been read yet.
  more?: boolean
  children?: ReactNode
}) {
  const id = useId()
  const [one, many] = props.noun
  const loaded = props.more ? ' loaded' : ''
  const count = props.query.trim()
    ? `${props.shown} of ${props.total} ${props.total === 1 ? one : many}${loaded}`
    : `${props.total} ${props.total === 1 ? one : many}${loaded}`
  return (
    <div className="list-toolbar">
      <div className="list-filter">
        <MagnifyingGlass size={16} aria-hidden="true" />
        <input
          id={id}
          type="search"
          aria-label={props.label}
          placeholder="Filter"
          value={props.query}
          onChange={(event) => props.onQuery(event.target.value)}
          maxLength={64}
          spellCheck={false}
          autoComplete="off"
        />
      </div>
      <p className="list-count" role="status">
        {count}
      </p>
      {props.children ? <div className="list-actions">{props.children}</div> : null}
    </div>
  )
}

export interface WizardStep {
  title: string
  body: ReactNode
  // problem holds Next back, and says why, until the step is complete.
  problem?: string
}

// Wizard walks through one task a step at a time in a modal dialog and ends on
// a review of its effect. Finishing only hands back to the caller, which makes
// the usual draft edit or asks the usual confirmation. Each new step's heading
// takes focus, so a screen reader hears where it is.
export function Wizard(props: { title: string; steps: WizardStep[]; finishLabel: string; onFinish: () => void; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)
  const titleId = useId()
  const problemId = useId()
  const [index, setIndex] = useState(0)
  const moved = useRef(false)
  useEffect(() => {
    const dialog = ref.current
    if (dialog && !dialog.open) dialog.showModal()
    return () => dialog?.close()
  }, [])
  useEffect(() => {
    if (moved.current) heading.current?.focus()
  }, [index])
  const go = (next: number) => {
    moved.current = true
    setIndex(next)
  }
  const step = props.steps[index]
  const last = index === props.steps.length - 1
  return (
    <dialog
      ref={ref}
      className="wizard"
      aria-labelledby={titleId}
      onCancel={(event) => {
        event.preventDefault()
        props.onClose()
      }}
    >
      <h2 id={titleId}>{props.title}</h2>
      <ol className="wizard-steps">
        {props.steps.map((item, position) => (
          <li key={item.title} aria-current={position === index ? 'step' : undefined} className={position < index ? 'wizard-done' : undefined}>
            <span className="wizard-number">{position + 1}</span> {item.title}
          </li>
        ))}
      </ol>
      <div className="dialog-body">
        <h3 ref={heading} tabIndex={-1}>
          Step {index + 1} of {props.steps.length}: {step.title}
        </h3>
        {step.body}
        {step.problem ? (
          <p id={problemId} className="hint">
            {step.problem}
          </p>
        ) : null}
      </div>
      <div className="actions dialog-actions">
        <button type="button" className="btn" onClick={props.onClose}>
          Cancel
        </button>
        {index > 0 ? (
          <button type="button" className="btn" onClick={() => go(index - 1)}>
            Back
          </button>
        ) : null}
        <button
          type="button"
          className="btn btn-primary"
          disabled={Boolean(step.problem)}
          aria-describedby={step.problem ? problemId : undefined}
          onClick={last ? props.onFinish : () => go(index + 1)}
        >
          {last ? props.finishLabel : 'Next'}
        </button>
      </div>
    </dialog>
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
