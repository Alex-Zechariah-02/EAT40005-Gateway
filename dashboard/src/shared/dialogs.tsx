import { useEffect, useId, useRef, useState, type ReactNode } from 'react'

// Modal dialogs: confirmations and step-by-step wizards.

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
}

// Wizard walks through one task a step at a time in a modal dialog and ends on
// a review of its effect. Finishing only hands back to the caller, which makes
// the usual draft edit or asks the usual confirmation. Each new step's heading
// takes focus, so a screen reader hears where it is.
// A caller that jumps between steps itself, as a quick choice that fills every
// step does, passes step and onStep.
export function Wizard(props: {
  title: string
  steps: WizardStep[]
  finishLabel: string
  finishDisabled?: boolean
  onFinish: () => void
  onClose: () => void
  step?: number
  onStep?: (step: number) => void
}) {
  const ref = useRef<HTMLDialogElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)
  const titleId = useId()
  const problemId = useId()
  const [ownIndex, setOwnIndex] = useState(0)
  const index = props.step ?? ownIndex
  const setIndex = props.onStep ?? setOwnIndex
  const moved = useRef(false)
  useEffect(() => {
    const dialog = ref.current
    if (dialog && !dialog.open) dialog.showModal()
    return () => dialog?.close()
  }, [])
  useEffect(() => {
    // The first step keeps the dialog's own focus; each later one moves it.
    if (moved.current) heading.current?.focus()
    moved.current = true
  }, [index])
  const go = (next: number) => setIndex(next)
  const step = props.steps[index]
  const last = index === props.steps.length - 1
  return (
    <dialog
      ref={ref}
      className="wizard"
      aria-labelledby={titleId}
      onCancel={(event) => {
        event.preventDefault()
        props.onClose()
      }}
    >
      <h2 id={titleId}>{props.title}</h2>
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
        {step.body}
        {step.problem ? (
          <p id={problemId} className="hint">
            {step.problem}
          </p>
        ) : null}
      </div>
      <div className="actions dialog-actions">
        <button type="button" className="btn" onClick={props.onClose}>
          Cancel
        </button>
        {index > 0 ? (
          <button type="button" className="btn" onClick={() => go(index - 1)}>
            Back
          </button>
        ) : null}
        <button
          type="button"
          className="btn btn-primary"
          disabled={Boolean(step.problem) || (last && props.finishDisabled)}
          aria-describedby={step.problem ? problemId : undefined}
          onClick={last ? props.onFinish : () => go(index + 1)}
        >
          {last ? props.finishLabel : 'Next'}
        </button>
      </div>
    </dialog>
  )
}
