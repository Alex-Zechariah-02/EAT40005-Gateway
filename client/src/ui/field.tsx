import { CaretDown } from '@phosphor-icons/react'
import { useId, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes, type TextareaHTMLAttributes } from 'react'
import { cx } from './cx'

// Fields keep a visible label, an optional hint and an error, all tied to the
// control for assistive technology (aria-describedby, aria-invalid).

const control =
  'w-full rounded-xl bg-sunken px-3.5 text-[15px] text-ink ring-1 ring-line transition-[box-shadow,background-color] duration-150 outline-none ' +
  'hover:ring-line-strong focus:bg-surface focus:ring-2 focus:ring-brand aria-invalid:ring-2 aria-invalid:ring-bad disabled:text-ink-3'

interface Shared {
  label: string
  hint?: ReactNode
  error?: string | null
  optional?: boolean
  className?: string
}

function Frame({ id, label, hint, error, optional, className, children }: Shared & { id: string; children: ReactNode }) {
  return (
    <div className={cx('flex flex-col gap-1.5', className)}>
      <label htmlFor={id} className="text-sm font-semibold text-ink">
        {label}
        {optional && <span className="ml-1.5 font-normal text-ink-3">(optional)</span>}
      </label>
      {children}
      {hint && !error && <p id={`${id}-hint`} className="text-xs text-ink-3">{hint}</p>}
      {error && <p id={`${id}-error`} className="text-xs font-semibold text-bad">{error}</p>}
    </div>
  )
}

function describedBy(id: string, hint: unknown, error: unknown): string | undefined {
  if (error) return `${id}-error`
  if (hint) return `${id}-hint`
  return undefined
}

export function TextField({ label, hint, error, optional, className, ...input }: Shared & InputHTMLAttributes<HTMLInputElement>) {
  const id = useId()
  return (
    <Frame id={id} label={label} hint={hint} error={error} optional={optional} className={className}>
      <input id={id} aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, hint, error)} className={cx(control, 'h-11')} {...input} />
    </Frame>
  )
}

export function TextArea({ label, hint, error, optional, className, ...input }: Shared & TextareaHTMLAttributes<HTMLTextAreaElement>) {
  const id = useId()
  return (
    <Frame id={id} label={label} hint={hint} error={error} optional={optional} className={className}>
      <textarea id={id} aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, hint, error)} className={cx(control, 'min-h-24 resize-y py-2.5')} {...input} />
    </Frame>
  )
}

export function SelectField({ label, hint, error, optional, className, children, ...select }: Shared & SelectHTMLAttributes<HTMLSelectElement>) {
  const id = useId()
  return (
    <Frame id={id} label={label} hint={hint} error={error} optional={optional} className={className}>
      <div className="relative">
        <select id={id} aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, hint, error)} className={cx(control, 'h-11 cursor-pointer appearance-none pr-10')} {...select}>
          {children}
        </select>
        <CaretDown size={16} className="pointer-events-none absolute top-1/2 right-3.5 -translate-y-1/2 text-ink-3" aria-hidden="true" />
      </div>
    </Frame>
  )
}
