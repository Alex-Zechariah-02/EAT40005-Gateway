import { useState, type ReactNode } from 'react'
import { Check, Copy, Question, ShieldWarning, WarningCircle } from '@phosphor-icons/react'
import type { Problem } from '../lib/messages'
import { Link } from '../lib/router'
import { cx } from './cx'

// RefTag shows the reference the gateway put on an answer, with a copy
// button, so a person can quote it to Fetch support.
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
    <span className="inline-flex max-w-full items-center gap-1 rounded-md bg-sunken py-0.5 pr-0.5 pl-2 text-xs text-ink-2 ring-1 ring-line">
      Ref <span className="min-w-0 font-semibold tracking-tight break-all text-ink tabular-nums">{value}</span>
      <button type="button" onClick={copy} className="relative flex size-6 items-center justify-center rounded text-ink-3 hover:bg-surface hover:text-ink" aria-label={copied ? 'Reference copied' : 'Copy reference'}>
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
        <div className="mt-2.5 flex flex-wrap items-center gap-x-3 gap-y-2 text-xs text-ink-3">
          {problem.by && <span>{problem.by === 'gateway' ? 'Answered by the Fetch gateway' : `Answered by ${problem.by}`}{problem.status ? ` · ${problem.status}` : ''}</span>}
          {problem.ref && <RefTag value={problem.ref} />}
          {report && problem.ref && (
            <Link to={`/help?ref=${encodeURIComponent(problem.ref)}`} className="font-semibold text-brand underline-offset-2 hover:underline">
              Report this problem
            </Link>
          )}
        </div>
        {action && <div className="mt-3 flex flex-wrap gap-2">{action}</div>}
      </div>
    </div>
  )
}

export function Skeleton({ className }: { className?: string }) {
  return <div className={cx('animate-pulse rounded-xl bg-sunken', className)} aria-hidden="true" />
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

export function Empty({ icon, title, body, action, className }: { icon: ReactNode; title: string; body?: ReactNode; action?: ReactNode; className?: string }) {
  return (
    <div className={cx('flex flex-col items-center rounded-3xl bg-surface px-6 py-14 text-center shadow-card', className)}>
      <span className="flex size-14 items-center justify-center rounded-full bg-brand-soft text-brand" aria-hidden="true">{icon}</span>
      <h2 className="mt-4 text-lg font-extrabold tracking-[-0.015em]">{title}</h2>
      {body && <p className="mt-1 max-w-[46ch] text-sm text-ink-2">{body}</p>}
      {action && <div className="mt-5 flex flex-wrap justify-center gap-2">{action}</div>}
    </div>
  )
}
