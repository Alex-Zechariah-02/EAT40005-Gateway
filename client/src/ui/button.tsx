import type { ButtonHTMLAttributes, ReactNode } from 'react'
import { Link } from '../lib/router'
import { cx } from './cx'

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger'
type Size = 'sm' | 'md' | 'lg'

const variants: Record<Variant, string> = {
  primary: 'bg-brand text-on-brand hover:bg-brand-strong disabled:bg-line-strong disabled:text-ink-3',
  secondary: 'bg-surface text-ink ring-1 ring-line-strong hover:bg-sunken disabled:text-ink-3',
  ghost: 'text-ink-2 hover:bg-sunken hover:text-ink disabled:text-ink-3',
  danger: 'bg-surface text-bad ring-1 ring-line-strong hover:bg-bad-soft disabled:text-ink-3',
}

const sizes: Record<Size, string> = {
  sm: 'h-9 px-3.5 text-sm gap-1.5 rounded-lg',
  md: 'h-11 px-5 text-[15px] gap-2 rounded-xl',
  lg: 'h-12 px-6 text-[15px] gap-2 rounded-xl',
}

export function buttonClass(variant: Variant = 'primary', size: Size = 'md', block = false): string {
  return cx(
    'inline-flex shrink-0 items-center justify-center font-bold whitespace-nowrap transition-[background-color,color,scale] duration-150 ease-out-quint select-none',
    'active:scale-[0.97] disabled:cursor-not-allowed disabled:active:scale-100',
    variants[variant],
    sizes[size],
    block && 'w-full',
  )
}

export function Button({
  variant,
  size,
  block,
  busy,
  busyLabel,
  disabled,
  className,
  children,
  type = 'button',
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: Size; block?: boolean; busy?: boolean; busyLabel?: ReactNode }) {
  return (
    <button type={type} className={cx(buttonClass(variant, size, block), className)} disabled={busy || disabled} aria-busy={busy || undefined} {...rest}>
      {busy && <span className="size-4 animate-spin rounded-full border-2 border-current border-r-transparent" aria-hidden="true" />}
      {busy && busyLabel ? busyLabel : children}
    </button>
  )
}

export function ButtonLink({ to, variant, size, block, className, children }: { to: string; variant?: Variant; size?: Size; block?: boolean; className?: string; children: ReactNode }) {
  return (
    <Link to={to} className={cx(buttonClass(variant, size, block), className)}>
      {children}
    </Link>
  )
}
