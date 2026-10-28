import { useState, type ReactNode } from 'react'
import { Check, Copy, Question, ShieldWarning, WarningCircle } from '@phosphor-icons/react'
import { describe, type Problem } from '../lib/messages'
import type { Query } from '../lib/query'
import { Link } from '../lib/router'
import { Button } from './button'
import { cx } from './cx'

// shortRef shows the gateway's 32-character reference as its first eight
// characters, grouped ("6F1C-A9E2"). Anything else is shown as sent.
export function shortRef(value: string): string {
  if (!/^[0-9a-f]{32}$/i.test(value)) return value
  const head = value.slice(0, 8).toUpperCase()
  return `${head.slice(0, 4)}-${head.slice(4)}`
}

// RefTag shows the reference the gateway put on an answer in a short,
// readable form; the full reference is its tooltip and what the copy button
// copies, so support can still look it up exactly.
export function RefTag({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard.writeText(value).then(
      () => {
        setCopied(true)
        setTimeout(() => setCopied(false), 1600)
      },
      () => setCopied(false),
    )
  }
  return (
    <span className="inline-flex max-w-full items-center gap-1 rounded-md bg-sunken py-0.5 pr-0.5 pl-2 text-xs text-ink-2 ring-1 ring-line" title={`Full reference ${value}`}>
      Ref <span className="min-w-0 font-semibold tracking-tight break-all text-ink tabular-nums">{shortRef(value)}</span>
      <button type="button" onClick={copy} className="relative flex size-6 items-center justify-center rounded text-ink-3 hover:bg-surface hover:text-ink" aria-label={copied ? 'Full reference copied' : 'Copy full reference'}>
        {copied ? <Check size={13} weight="bold" /> : <Copy size={13} />}
      </button>
    </span>
  )
}

// ProblemNotice tells a person what went wrong in plain words, then who
// answered (the gateway or a named service) and the reference.
export function ProblemNotice({ problem, action, report = false, className }: { problem: Problem; action?: ReactNode; report?: boolean; className?: string }) {
  const Icon = problem.unknown ? Question : problem.by === 'gateway' ? ShieldWarning : WarningCircle
  const tone = problem.unknown ? 'bg-warn-soft text-warn' : 'bg-bad-soft text-bad'
  return (
    <div role="alert" className={cx('flex gap-3 rounded-2xl bg-surface p-4 shadow-card', className)}>
      <span className={cx('flex size-9 shrink-0 items-center justify-center rounded-full', tone)} aria-hidden="true">
        <Icon size={19} weight="bold" />
      </span>
      <div className="min-w-0 flex-1">
        <p className="font-bold text-ink">{problem.title}</p>
        {problem.detail && <p className="mt-0.5 text-sm text-ink-2">{problem.detail}</p>}
        <AnswerLine problem={problem} report={report} className="mt-2.5" />
        {action && <div className="mt-3 flex flex-wrap gap-2">{action}</div>}
      </div>
    </div>
  )
}

// AnswerLine says who answered and gives the reference, with a link to report
// it to Fetch support when asked.
export function AnswerLine({ problem, report = false, className }: { problem: Problem; report?: boolean; className?: string }) {
  return (
    <div className={cx('flex flex-wrap items-center gap-x-3 gap-y-2 text-xs text-ink-3', className)}>
      {problem.by && <span>{problem.by === 'gateway' ? 'Answered by the Fetch gateway' : `Answered by ${problem.by}`}{problem.status ? ` · ${problem.status}` : ''}</span>}
      {problem.ref && <RefTag value={problem.ref} />}
      {report && problem.ref && (
        <Link to={`/help?ref=${encodeURIComponent(problem.ref)}`} className="font-semibold text-brand underline-offset-2 hover:underline">
          Report this problem
        </Link>
      )}
    </div>
  )
}

export function Skeleton({ className }: { className?: string }) {
  return <div className={cx('animate-pulse rounded-xl bg-sunken', className)} aria-hidden="true" />
}

// RefreshProblem says that the latest refresh of a page failed while the page
// keeps showing what it last loaded, so old data never passes for current.
export function RefreshProblem<T>({ query, report = false, className }: { query: Query<T>; report?: boolean; className?: string }) {
  const problem = query.data && query.answer ? describe(query.answer) : null
  if (!problem) return null
  const stale = { ...problem, detail: [problem.detail, 'What’s shown below is from the last successful check.'].filter(Boolean).join(' ') }
  return <ProblemNotice className={className} problem={stale} report={report} action={<Button size="sm" variant="secondary" onClick={query.reload}>Try again</Button>} />
}

// Loading announces a wait to assistive technology while skeletons show it visually.
export function Loading({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div aria-busy="true">
      <span className="sr-only" role="status">{label}</span>
      {children}
    </div>
  )
}

export function Empty({ icon, title, body, action, footer, className }: { icon: ReactNode; title: string; body?: ReactNode; action?: ReactNode; footer?: ReactNode; className?: string }) {
  return (
    <div className={cx('flex flex-col items-center rounded-3xl bg-surface px-6 py-14 text-center shadow-card', className)}>
      <span className="flex size-14 items-center justify-center rounded-full bg-brand-soft text-brand" aria-hidden="true">{icon}</span>
      <h2 className="mt-4 text-lg font-extrabold tracking-[-0.015em]">{title}</h2>
      {body && <p className="mt-1 max-w-[46ch] text-sm text-ink-2">{body}</p>}
      {action && <div className="mt-5 flex flex-wrap justify-center gap-2">{action}</div>}
      {footer}
    </div>
  )
}
