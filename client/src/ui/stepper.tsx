import { Minus, Plus, Trash } from '@phosphor-icons/react'
import { cx } from './cx'

// QuantityStepper changes a basket quantity. Each button has a 40 px hit
// area even though it looks smaller; at one, the minus button removes.
export function QuantityStepper({ value, max, name, onChange, size = 'md' }: { value: number; max: number; name: string; onChange: (next: number) => void; size?: 'sm' | 'md' }) {
  const small = size === 'sm'
  const button = cx(
    'relative flex items-center justify-center rounded-full text-ink transition-colors hover:bg-surface disabled:text-ink-3 disabled:hover:bg-transparent',
    'before:absolute before:-inset-1.5 before:content-[""]',
    small ? 'size-7' : 'size-8',
  )
  return (
    <div className="inline-flex items-center gap-1 rounded-full bg-sunken p-1 ring-1 ring-line" role="group" aria-label={`Quantity of ${name}`}>
      <button type="button" className={button} onClick={() => onChange(value - 1)} aria-label={value === 1 ? `Remove ${name}` : `One fewer ${name}`}>
        {value === 1 ? <Trash size={small ? 13 : 15} weight="bold" /> : <Minus size={small ? 13 : 15} weight="bold" />}
      </button>
      <span className={cx('text-center font-bold tabular-nums', small ? 'w-5 text-sm' : 'w-6')} aria-live="polite">{value}</span>
      <button type="button" className={button} onClick={() => onChange(value + 1)} disabled={value >= max} aria-label={`One more ${name}`}>
        <Plus size={small ? 13 : 15} weight="bold" />
      </button>
    </div>
  )
}
