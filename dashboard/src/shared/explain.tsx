// Explain: a term that opens a short explanation on hover, focus or click.
import { useId, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

const openDelay = 350
const closeDelay = 200

// Explain shows its term as a button. Pointer rest opens the note after a
// short delay; a click or Enter keeps it open until Escape or a click outside.
export function Explain(props: { term: string; plain: string; technical?: string; children?: ReactNode }) {
  const id = useId()
  const button = useRef<HTMLButtonElement>(null)
  const note = useRef<HTMLDivElement>(null)
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  const pinned = useRef(false)

  function place() {
    const anchor = button.current?.getBoundingClientRect()
    const element = note.current
    if (!anchor || !element) return
    const width = element.offsetWidth
    const left = Math.max(8, Math.min(anchor.left, window.innerWidth - width - 8))
    const below = anchor.bottom + 8 + element.offsetHeight <= window.innerHeight
    element.style.left = `${left}px`
    element.style.top = below ? `${anchor.bottom + 8}px` : `${anchor.top - element.offsetHeight - 8}px`
  }
  function show() {
    const element = note.current
    if (!element || element.matches(':popover-open')) return
    element.showPopover()
    place()
  }
  function hide() {
    if (note.current?.matches(':popover-open')) note.current.hidePopover()
  }
  function later(action: () => void, delay: number) {
    clearTimeout(timer.current)
    timer.current = setTimeout(action, delay)
  }
  function toggle() {
    clearTimeout(timer.current)
    pinned.current = !note.current?.matches(':popover-open') || !pinned.current
    if (pinned.current) show()
    else hide()
  }
  function leave() {
    if (!pinned.current) later(hide, closeDelay)
  }

  return (
    <>
      <button
        ref={button}
        type="button"
        className="explain-term"
        aria-describedby={id}
        onClick={toggle}
        onPointerEnter={(event) => event.pointerType === 'mouse' && later(show, openDelay)}
        onPointerLeave={leave}
      >
        {props.children ?? props.term}
      </button>
      {createPortal(
        <div
          ref={note}
          id={id}
          popover="auto"
          role="tooltip"
          className="explain-note"
          onPointerEnter={() => clearTimeout(timer.current)}
          onPointerLeave={leave}
          onToggle={(event) => {
            if (event.newState === 'closed') pinned.current = false
          }}
        >
          <strong className="explain-title">{props.term}</strong>
          <p>{props.plain}</p>
          {props.technical ? <p className="explain-technical">{props.technical}</p> : null}
        </div>,
        document.body,
      )}
    </>
  )
}
