import { Basket, ForkKnife } from '@phosphor-icons/react'
import { photoUrl } from '../lib/photos'
import { cx } from './cx'

// Photo shows the bundled photo for a merchant or item ID, or a neutral
// placeholder for an ID without one. Photos are
// decorative: the name beside them carries the meaning.
export function Photo({ id, className, dim = false, section = 'food' }: { id: string; className?: string; dim?: boolean; section?: 'food' | 'mart' }) {
  const url = photoUrl(id)
  if (!url) {
    const Icon = section === 'mart' ? Basket : ForkKnife
    return (
      <div className={cx('flex items-center justify-center bg-sunken text-ink-3', className)} aria-hidden="true">
        <Icon size={28} />
      </div>
    )
  }
  return <img src={url} alt="" loading="lazy" decoding="async" className={cx('photo bg-sunken object-cover', dim && 'opacity-45 grayscale', className)} />
}
