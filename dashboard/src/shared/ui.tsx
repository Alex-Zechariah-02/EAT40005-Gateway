import { Check, Clock, X, QuestionMark, Minus, Plus, ArrowRight, MagnifyingGlass } from '@phosphor-icons/react'
import { Fragment, useEffect, useId, useState, type ReactNode } from 'react'
import { message, type Reply } from './api'

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

// CopyButton copies one exact value, such as a code or an ID, and says so.
function CopyButton(props: { value: string; label: string }) {
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
      <summary>More Details</summary>
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

// usePageScrollbar shows the window's scrollbar only while the page scrolls
// or the pointer is near it, as scrolling lists show theirs on hover.
export function usePageScrollbar() {
  useEffect(() => {
    const root = document.documentElement
    let timer = 0
    const reveal = () => {
      root.classList.add('is-scrolling')
      clearTimeout(timer)
      timer = window.setTimeout(() => root.classList.remove('is-scrolling'), 900)
    }
    const near = (event: PointerEvent) => {
      if (event.clientX >= root.clientWidth - 16) reveal()
    }
    window.addEventListener('scroll', reveal, { passive: true })
    window.addEventListener('pointermove', near, { passive: true })
    return () => {
      clearTimeout(timer)
      window.removeEventListener('scroll', reveal)
      window.removeEventListener('pointermove', near)
    }
  }, [])
}
