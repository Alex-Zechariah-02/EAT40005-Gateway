import type { Dispatch, SetStateAction } from 'react'
import type { Candidate, Operation, Outcome, Policy, Revision } from '../../shared/api'
import { differences } from '../../shared/diff'
import type { Mutation } from '../../shared/hooks'
import { type FormProps, ObjectForm } from './object'
import { changes, copy, type BoxKey, type Issue, type DesignerDraft } from './model'
import { trace, traceExplanation } from './trace'
import { Badge, Field } from '../../shared/ui'
import { ChangeList } from '../../shared/review'
import type { ViewProps } from '../../app/views'
import { stages, stepWords } from '../../shared/checks'
import { Explain } from '../../shared/explain'

// Parts of the Policy page around the work area: the command row, the stale-draft notice and the side panel with its trace.

export type Mode = 'draft' | 'confirmed' | 'provisional'
export type Tab = 'canvas' | 'form' | 'json'
export interface Staged {
  receipt: Outcome
  candidate: Candidate
}

// What each view of the policy is for, said once under the view tabs.
const viewNotes: Record<Tab, string> = {
  canvas: 'Map of roles, endpoints, APIs and service addresses. Select a role to see what it may call.',
  form: 'Edit each role, endpoint, API and service address, or tick permissions in the matrix.',
  json: 'The complete policy document, to copy, download or load from a file.',
}

function TracePanel({
  policy,
  who,
  setWho,
  what,
  setWhat,
  steps,
}: {
  policy: Policy
  who: string
  setWho: (value: string) => void
  what: string
  setWhat: (value: string) => void
  steps: ReturnType<typeof trace>
}) {
  return (
    <section className="designer-trace">
      <h2>Trace</h2>
      <Field id="designer-trace-who" label="Who">
        <select className="input" id="designer-trace-who" value={who} onChange={(event) => setWho(event.target.value)}>
          <option value="none">no token</option>
          {policy.roles.map((role) => (
            <option value={`role:${role.id}`} key={`role:${role.id}`}>
              a token with role {role.id}
            </option>
          ))}
        </select>
      </Field>
      <Field id="designer-trace-what" label="What">
        <select className="input" id="designer-trace-what" value={what} onChange={(event) => setWhat(event.target.value)}>
          {policy.operations.map((operation) => (
            <option value={operation.id} key={operation.id}>
              {operation.id}: {operation.method} {operation.target}
            </option>
          ))}
        </select>
      </Field>
      {steps.length ? (
        <ol className="designer-trace-steps">
          {steps.map((step) => (
            <li key={step.name}>
              <strong>
                <CheckName name={step.name} />
              </strong>{' '}
              {step.state === 'unreached' ? (
                <span className="hint">{stepWords.unreached}</span>
              ) : (
                <Badge tone={step.state === 'passed' ? 'ok' : step.state === 'refused' ? 'refused' : 'neutral'}>{stepWords[step.state]}</Badge>
              )}
              {step.text ? <p>{step.text}</p> : null}
            </li>
          ))}
        </ol>
      ) : (
        <p className="hint">Add an endpoint to trace a request.</p>
      )}
      <p className="notice tone-neutral">{traceExplanation}</p>
    </section>
  )
}

// CheckName names one of the gateway's checks with its explanation.
function CheckName({ name }: { name: string }) {
  const check = stages.find((item) => item.name === name)
  return check ? <Explain term={check.name} plain={check.plain} technical={check.technical} /> : <>{name}</>
}
// DesignerBar is the command row: the views with what each is for, the version shown, and the draft's state and actions.
export function DesignerBar(props: {
  can: ViewProps['can']
  draft: DesignerDraft
  pendingId: number | undefined
  mode: Mode
  tab: Tab
  setStageConfirm: (value: boolean) => void
  setDiscardConfirm: (value: boolean) => void
  current: Revision
  dirty: boolean
  changed: ReturnType<typeof changes>
  issues: Issue[]
  mutation: Mutation<Staged>
  changeTab: (next: Tab) => void
  changeMode: (next: Mode) => void
  canStage: boolean
  canDiscard: boolean
}) {
  const { can, draft, pendingId, mode, tab, setStageConfirm, setDiscardConfirm, current, dirty, changed, issues, mutation, changeTab, changeMode, canStage, canDiscard } = props
  return (
    <div className="designer-bar">
      <div className="designer-views">
          <div className="section-tabs designer-view-tabs" role="tablist" aria-label="Policy views">
            {(['canvas', 'form', 'json'] as Tab[]).map((choice, index, tabs) => (
              <button
                id={`designer-tab-${choice}`}
                role="tab"
                type="button"
                key={choice}
                aria-selected={tab === choice}
                aria-controls="designer-view"
                tabIndex={tab === choice ? 0 : -1}
                onClick={() => changeTab(choice)}
                onKeyDown={(event) => {
                  let next: Tab | undefined
                  if (event.key === 'ArrowRight') next = tabs[(index + 1) % tabs.length]
                  if (event.key === 'ArrowLeft') next = tabs[(index + tabs.length - 1) % tabs.length]
                  if (event.key === 'Home') next = tabs[0]
                  if (event.key === 'End') next = tabs[tabs.length - 1]
                  if (next) {
                    event.preventDefault()
                    changeTab(next)
                  }
                }}
              >
                {choice === 'json' ? 'JSON' : choice[0].toUpperCase() + choice.slice(1)}
              </button>
            ))}
          </div>
        <p className="designer-view-note">{viewNotes[tab]}</p>
      </div>
      <div className="designer-bar-actions">
        <label className="designer-viewing">
          <span>Viewing</span>
          <select className="input" value={mode} onChange={(event) => changeMode(event.target.value as Mode)}>
            <option value="draft">Draft</option>
            <option value="confirmed">Active · Version {current.id}</option>
            {pendingId !== undefined ? <option value="provisional">On Trial · Version {pendingId}</option> : null}
          </select>
        </label>
        <Badge tone={issues.length ? 'pending' : 'ok'}>{issues.length ? `${issues.length} ${issues.length === 1 ? 'Issue' : 'Issues'}` : 'No Issues'}</Badge>
        {can('manage_policy') ? (
          <>
            <p className={`designer-draft-state${dirty ? ' is-dirty' : ''}`} role="status">
              {mode !== 'draft'
                ? 'Edits go to the draft.'
                : dirty
                  ? `${changed.length} unsaved ${changed.length === 1 ? 'change' : 'changes'}`
                  : `No changes to version ${draft.baseRevision}`}
            </p>
            <button className="btn btn-danger-quiet" type="button" disabled={!canDiscard || mutation.pending} onClick={() => setDiscardConfirm(true)}>
              Discard
            </button>
            <button className="btn btn-primary" type="button" disabled={!canStage} onClick={() => setStageConfirm(true)}>
              Save Changes
            </button>
          </>
        ) : null}
      </div>
    </div>
  )
}

// StaleNotice says the active policy moved on since the draft was started, and offers to rebase the draft.
export function StaleNotice(props: {
  draft: DesignerDraft
  setDraft: Dispatch<SetStateAction<DesignerDraft | null>>
  showChanges: boolean
  setShowChanges: Dispatch<SetStateAction<boolean>>
  current: Revision
  confirmed: Policy | undefined
  mutation: Mutation<Staged>
}) {
  const { draft, setDraft, showChanges, setShowChanges, current, confirmed, mutation } = props
  return (
    <section className="notice tone-pending">
      <p>
        The active policy changed to version {current.id} since your draft was started from version {draft.baseRevision}. Your draft keeps
        its own changes; saving it unchanged would be refused.
      </p>
      <div className="actions">
        <button className="btn" type="button" aria-expanded={showChanges} onClick={() => setShowChanges((value) => !value)}>
          Show What Changed
        </button>
        <button
          className="btn"
          type="button"
          onClick={() => {
            setDraft({ ...draft, base: copy(confirmed!), baseRevision: current.id })
            mutation.clear()
            setShowChanges(false)
          }}
        >
          Keep my draft and base it on r{current.id}
        </button>
      </div>
      {showChanges ? <ChangeList changes={differences(draft.base, confirmed!)} /> : null}
    </section>
  )
}

// SidePanel holds the trace or the selected box's editor beside the canvas.
export function SidePanel(props: {
  selection: BoxKey | null
  setSelection: (value: BoxKey | null) => void
  showTrace: boolean
  setShowTrace: Dispatch<SetStateAction<boolean>>
  who: string
  setWho: (value: string) => void
  setWhat: (value: string) => void
  document: Policy
  currentOperation: Operation | undefined
  steps: ReturnType<typeof trace>
  formProps: FormProps | null
}) {
  const { selection, setSelection, showTrace, setShowTrace, who, setWho, setWhat, document, currentOperation, steps, formProps } = props
  return (
    <aside className="designer-side-panel" aria-label={showTrace ? 'Trace policy document' : 'Selected policy box'}>
      <button
        type="button"
        className="btn btn-small designer-panel-close"
        onClick={() => (showTrace ? setShowTrace(false) : setSelection(null))}
      >
        Close
      </button>
      {showTrace ? (
        <TracePanel
          policy={document}
          who={who}
          setWho={setWho}
          what={currentOperation?.id ?? ''}
          setWhat={(operation) => {
            setWhat(operation)
            setSelection(`operation:${operation}`)
          }}
          steps={steps}
        />
      ) : selection && formProps ? (
        <ObjectForm {...formProps} selection={selection} inPanel />
      ) : null}
    </aside>
  )
}
