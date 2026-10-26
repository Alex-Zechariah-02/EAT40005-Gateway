import { Dialog } from '@base-ui/react/dialog'
import type { ReactNode } from 'react'
import { Button } from './button'

// ConfirmDialog asks before an action the person cannot take back. It is a
// Base UI dialog: focus moves in, Escape and the cancel button close it, and
// focus returns to the button that opened it.
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  body,
  confirmLabel,
  cancelLabel = 'Keep it',
  onConfirm,
  danger = false,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  body: ReactNode
  confirmLabel: string
  cancelLabel?: string
  onConfirm: () => void
  danger?: boolean
}) {
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Backdrop className="fixed inset-0 z-40 bg-black/40 transition-opacity duration-200 data-[ending-style]:opacity-0 data-[starting-style]:opacity-0" />
        <Dialog.Popup className="fixed top-1/2 left-1/2 z-50 w-[min(440px,calc(100vw-32px))] -translate-x-1/2 -translate-y-1/2 rounded-3xl bg-surface p-6 text-ink shadow-raised transition-[opacity,scale] duration-200 ease-out-quint data-[ending-style]:scale-95 data-[ending-style]:opacity-0 data-[starting-style]:scale-95 data-[starting-style]:opacity-0">
          <Dialog.Title className="text-lg font-extrabold tracking-[-0.015em]">{title}</Dialog.Title>
          <Dialog.Description render={<div />} className="mt-2 text-sm text-ink-2">{body}</Dialog.Description>
          <div className="mt-6 flex flex-wrap justify-end gap-2">
            <Dialog.Close render={<Button variant="secondary" />}>{cancelLabel}</Dialog.Close>
            <Button variant={danger ? 'danger' : 'primary'} onClick={onConfirm}>
              {confirmLabel}
            </Button>
          </div>
        </Dialog.Popup>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
