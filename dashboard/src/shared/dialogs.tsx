import { X } from '@phosphor-icons/react'
import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'

// Modal dialogs: confirmations and step-by-step wizards.

const focusable = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]:not([tabindex="-1"])'

// keepTabInside makes Tab and Shift+Tab wrap within a modal dialog, so
// keyboard focus never leaves it for the browser's own controls.
export function keepTabInside(event: KeyboardEvent<HTMLElement>) {
  if (event.key !== 'Tab') return
  const items = [...event.currentTarget.querySelectorAll<HTMLElement>(focusable)].filter((item) => item.getClientRects().length > 0)
  const first = items[0]
  const last = items.at(-1)
  if (!first || !last) return
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault()
    first.focus()
  }
}

// Confirm asks for explicit confirmation of a policy-changing action in a
// native modal dialog, which keeps keyboard focus inside until it closes.
// Focus starts on Cancel, so pressing Enter by habit changes nothing.
// TypedConfirmation asks for a word to be typed before the riskiest actions;
// the dialog's confirm button stays held back until it matches.
export function TypedConfirmation(props: { word: string; value: string; onChange: (value: string) => void }) {
  const id = useId()
  return (
    <div className="field">
      <label htmlFor={id}>
        Type <strong>{props.word}</strong> to confirm
      </label>
      <input
        id={id}
        className="input mono"
        value={props.value}
        onChange={(event) => props.onChange(event.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  )
}

export function Confirm(props: {
  open: boolean
  title: string
  confirmLabel: string
  danger?: boolean
  // disabled holds the confirm button back, for example until the facts it
  // depends on have been read again.
  disabled?: boolean
  onConfirm: () => void
  onCancel: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDialogElement>(null)
  const titleId = useId()
  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (props.open && !dialog.open) dialog.showModal()
    if (!props.open && dialog.open) dialog.close()
  }, [props.open])

  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      onCancel={(event) => {
        event.preventDefault()
        props.onCancel()
      }}
    >
      <h2 id={titleId}>{props.title}</h2>
      <div className="dialog-body">{props.children}</div>
      <div className="actions dialog-actions">
        <button type="button" className="btn" autoFocus onClick={props.onCancel}>
          Cancel
        </button>
        <button type="button" className={`btn ${props.danger ? 'btn-danger' : 'btn-primary'}`} onClick={props.onConfirm} disabled={props.disabled}>
          {props.confirmLabel}
        </button>
      </div>
    </dialog>
  )
}

interface WizardStep {
  title: string
  body: ReactNode
  // problem holds Next back, and says why, until the step is complete.
  problem?: string
  // plain says what the step decides; technical names the policy field it writes.
  plain?: string
  technical?: ReactNode
  // reveal, when given, keeps Next available: pressing it early marks the
  // step's fields and states the problem instead of moving on.
  reveal?: () => void
  // skip, when given, offers to pass over an optional step after clearing it.
  skip?: () => void
}

// Wizard walks through one task a step at a time in a modal dialog and ends on
// a review of its effect. Finishing only hands back to the caller, which makes
// the usual draft edit or asks the usual confirmation. Each new step's heading
// takes focus, so a screen reader hears where it is; closing returns focus to
// the control that opened it.
// A caller that jumps between steps itself, as a quick choice that fills every
// step does, passes step and onStep. drawer shows it along the window's right
// edge with a Close button; unsaved asks before closing discards input.
export function Wizard(props: {
  title: string
  steps: WizardStep[]
  finishLabel: string
  finishDisabled?: boolean
  onFinish: () => void
  onClose: () => void
  step?: number
  onStep?: (step: number) => void
  drawer?: boolean
  unsaved?: boolean
}) {
  const ref = useRef<HTMLDialogElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)
  const titleId = useId()
  const problemId = useId()
  const [ownIndex, setOwnIndex] = useState(0)
  const [tried, setTried] = useState(-1)
  const [asking, setAsking] = useState(false)
  const index = props.step ?? ownIndex
  const setIndex = props.onStep ?? setOwnIndex
  const moved = useRef(false)
  useEffect(() => {
    const dialog = ref.current
    const opener = document.activeElement
    if (dialog && !dialog.open) dialog.showModal()
    return () => {
      dialog?.close()
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus()
    }
  }, [])
  useEffect(() => {
    // A centred wizard's first step keeps the dialog's own focus; each later
    // step, and every step of a drawer, moves it to the step's heading.
    if (moved.current || props.drawer) heading.current?.focus()
    moved.current = true
  }, [index])
  const go = (next: number) => setIndex(next)
  const close = () => (props.unsaved ? setAsking(true) : props.onClose())
  const step = props.steps[index]
  const last = index === props.steps.length - 1
  const stated = step.problem && (!step.reveal || tried === index) ? step.problem : undefined
  const advance = () => {
    if (step.problem && step.reveal) {
      setTried(index)
      step.reveal()
      requestAnimationFrame(() => ref.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus())
      return
    }
    if (last) props.onFinish()
    else go(index + 1)
  }
  return (
    <>
      <dialog
        ref={ref}
        className={props.drawer ? 'wizard drawer' : 'wizard'}
        aria-labelledby={titleId}
        onKeyDown={keepTabInside}
        onCancel={(event) => {
          event.preventDefault()
          close()
        }}
      >
        {props.drawer ? (
          <div className="drawer-head">
            <h2 id={titleId}>{props.title}</h2>
            <button type="button" className="btn btn-small" onClick={close}>
              <X size={14} aria-hidden="true" /> Close
            </button>
          </div>
        ) : (
          <h2 id={titleId}>{props.title}</h2>
        )}
        <ol className="wizard-steps">
          {props.steps.map((item, position) => (
            <li key={item.title} aria-current={position === index ? 'step' : undefined} className={position < index ? 'wizard-done' : undefined}>
              <span className="wizard-number">{position + 1}</span> {item.title}
            </li>
          ))}
        </ol>
        <div className="dialog-body">
          <h3 ref={heading} tabIndex={-1}>
            Step {index + 1} of {props.steps.length}: {step.title}
          </h3>
          {step.plain ? <p className="wizard-plain">{step.plain}</p> : null}
          {step.technical ? <p className="wizard-technical">{step.technical}</p> : null}
          {step.body}
          {stated ? (
            <p id={problemId} className="hint" role={step.reveal ? 'alert' : undefined}>
              {stated}
            </p>
          ) : null}
        </div>
        <div className="actions dialog-actions">
          <button type="button" className="btn" onClick={close}>
            Cancel
          </button>
          {index > 0 ? (
            <button type="button" className="btn" onClick={() => go(index - 1)}>
              Back
            </button>
          ) : null}
          {step.skip ? (
            <button
              type="button"
              className="btn"
              onClick={() => {
                step.skip?.()
                go(index + 1)
              }}
            >
              Skip This Step
            </button>
          ) : null}
          <button
            type="button"
            className="btn btn-primary"
            disabled={(Boolean(step.problem) && !step.reveal) || (last && props.finishDisabled)}
            aria-describedby={stated ? problemId : undefined}
            onClick={advance}
          >
            {last ? props.finishLabel : 'Next'}
          </button>
        </div>
      </dialog>
      <Confirm open={asking} title="Discard This Input?" confirmLabel="Discard Input" danger onConfirm={props.onClose} onCancel={() => setAsking(false)}>
        <p>Nothing you entered in {props.title} is added to the draft.</p>
      </Confirm>
    </>
  )
}
