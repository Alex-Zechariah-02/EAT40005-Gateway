import { Plus } from '@phosphor-icons/react'
import { useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react'
import { createPortal } from 'react-dom'

// Layers above the page: control tooltips and the + action menu.

// TooltipLayer draws the tip of every control marked with data-tip in one
// layer above the page. A tip drawn inside a scrolling list would be clipped
// by it and would widen it, giving the list a sideways scrollbar.
export function TooltipLayer() {
  const [tip, setTip] = useState<{ text: string; box: DOMRect; side: 'top' | 'right'; layer: Element } | null>(null)
  const drawn = useRef<HTMLDivElement>(null)
  useEffect(() => {
    let current: HTMLElement | null = null
    const show = (target: EventTarget | null, keyboard: boolean) => {
      const element = target instanceof Element ? target.closest<HTMLElement>('[data-tip]') : null
      if (element === current) return
      current = element
      // A labelled link in the wide sidebar already shows its words.
      const labelled = !!element?.closest('.shell:not(.is-rail)') && element.matches('.nav-group a, .sidebar-toggle')
      if (!element?.dataset.tip || labelled || (keyboard && !element.matches(':focus-visible'))) {
        setTip(null)
        return
      }
      // A tip inside an open modal dialog is drawn in it: the dialog's top layer covers the rest of the page.
      const layer = element.closest('dialog[open]') ?? document.body
      setTip({ text: element.dataset.tip, box: element.getBoundingClientRect(), side: element.closest('.sidebar') ? 'right' : 'top', layer })
    }
    const hide = () => {
      current = null
      setTip(null)
    }
    const over = (event: PointerEvent) => show(event.target, false)
    const focus = (event: FocusEvent) => show(event.target, true)
    document.addEventListener('pointerover', over)
    document.addEventListener('focusin', focus)
    document.addEventListener('focusout', hide)
    document.addEventListener('pointerdown', hide)
    window.addEventListener('scroll', hide, true)
    return () => {
      document.removeEventListener('pointerover', over)
      document.removeEventListener('focusin', focus)
      document.removeEventListener('focusout', hide)
      document.removeEventListener('pointerdown', hide)
      window.removeEventListener('scroll', hide, true)
    }
  }, [])
  // Keep the whole tip on screen near the window's edges.
  useLayoutEffect(() => {
    const element = drawn.current
    if (!element || !tip) return
    const width = element.offsetWidth
    const left = tip.side === 'right' ? tip.box.right + 10 : tip.box.left + tip.box.width / 2 - width / 2
    element.style.left = `${Math.max(8, Math.min(left, window.innerWidth - width - 8))}px`
    element.style.top = tip.side === 'right' ? `${tip.box.top + tip.box.height / 2 - element.offsetHeight / 2}px` : `${tip.box.top - element.offsetHeight - 6}px`
  }, [tip])
  // Drawn for sight only: each control's label or aria-label already names it.
  return tip
    ? createPortal(
        <div ref={drawn} className="tooltip" aria-hidden="true">
          {tip.text}
        </div>,
        tip.layer,
      )
    : null
}

export interface MenuAction {
  label: string
  run: () => void
  // unavailable says why the action cannot be taken yet.
  unavailable?: string
}

// ActionMenu is a + button that opens a short list of actions. The list is
// drawn above the page, so a card or a scrolling list never clips it; arrow
// keys move through it and Escape closes it, returning focus to the button.
export function ActionMenu(props: { label: string; tip?: string; items: MenuAction[]; className?: string; text?: string }) {
  const [open, setOpen] = useState(false)
  const [at, setAt] = useState<{ top: number; left: number }>({ top: 0, left: 0 })
  const trigger = useRef<HTMLButtonElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const id = useId()
  function close(refocus: boolean) {
    setOpen(false)
    if (refocus) trigger.current?.focus()
  }
  useLayoutEffect(() => {
    if (!open || !trigger.current || !list.current) return
    const box = trigger.current.getBoundingClientRect()
    const width = list.current.offsetWidth
    const height = list.current.offsetHeight
    const below = box.bottom + 6 + height <= window.innerHeight - 8
    setAt({
      top: below ? box.bottom + 6 : Math.max(8, box.top - 6 - height),
      left: Math.max(8, Math.min(box.right - width, window.innerWidth - width - 8)),
    })
    // Focus moves into the menu without scrolling the page, which would close it.
    list.current.querySelector<HTMLButtonElement>('[role="menuitem"]:not([aria-disabled="true"])')?.focus({ preventScroll: true })
  }, [open])
  useEffect(() => {
    if (!open) return
    const away = (event: PointerEvent) => {
      const target = event.target as Node
      if (!list.current?.contains(target) && !trigger.current?.contains(target)) close(false)
    }
    // The menu closes only when its button has moved, so it would no longer
    // sit beside it; a scroll elsewhere on the page leaves it open.
    const start = trigger.current?.getBoundingClientRect()
    const moved = () => {
      const now = trigger.current?.getBoundingClientRect()
      if (!start || !now || Math.abs(now.top - start.top) > 1 || Math.abs(now.left - start.left) > 1) close(false)
    }
    document.addEventListener('pointerdown', away)
    window.addEventListener('scroll', moved, true)
    window.addEventListener('resize', moved)
    return () => {
      document.removeEventListener('pointerdown', away)
      window.removeEventListener('scroll', moved, true)
      window.removeEventListener('resize', moved)
    }
  }, [open])
  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    const items = [...(list.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? [])]
    const index = items.indexOf(document.activeElement as HTMLButtonElement)
    let next = -1
    if (event.key === 'ArrowDown') next = (index + 1) % items.length
    if (event.key === 'ArrowUp') next = (index - 1 + items.length) % items.length
    if (event.key === 'Home') next = 0
    if (event.key === 'End') next = items.length - 1
    if (next >= 0) {
      event.preventDefault()
      items[next].focus()
    }
    if (event.key === 'Escape') {
      event.preventDefault()
      event.stopPropagation()
      close(true)
    }
    if (event.key === 'Tab') close(false)
  }
  return (
    <>
      <button
        ref={trigger}
        type="button"
        className={`${props.text ? 'btn btn-small' : 'icon-button is-icon'} action-menu-trigger${open ? ' is-open' : ''} ${props.className ?? ''}`}
        aria-label={props.label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        data-tip={open || props.text ? undefined : props.tip}
        onClick={() => (open ? close(false) : setOpen(true))}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' && !open) {
            event.preventDefault()
            setOpen(true)
          }
        }}
      >
        <Plus size={14} weight="bold" aria-hidden="true" />
        {props.text}
      </button>
      {open
        ? createPortal(
            <div ref={list} id={id} className="action-menu" role="menu" aria-label={props.label} style={at} onKeyDown={onKeyDown}>
              {props.items.map((item) => (
                <button
                  key={item.label}
                  type="button"
                  role="menuitem"
                  className="action-menu-item"
                  aria-disabled={item.unavailable ? true : undefined}
                  onClick={() => {
                    if (item.unavailable) return
                    close(false)
                    item.run()
                  }}
                >
                  <span>{item.label}</span>
                  {item.unavailable ? <span className="action-menu-why">{item.unavailable}</span> : null}
                </button>
              ))}
            </div>,
            document.body,
          )
        : null}
    </>
  )
}
