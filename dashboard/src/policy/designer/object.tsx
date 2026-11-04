import { useEffect, useId, useState } from 'react'
import type { Operation, Policy } from '../../shared/api'
import { capital, copy, deleteReason, kindNames, rename, sections, splitKey, type BoxKey } from './model'
import { Confirm } from '../../shared/dialogs'
import { Field, describedBy } from '../../shared/ui'
import { checks, OperationFields } from './fields'

// The editor for one box of the draft: its ID, its fields, its permissions and its deletion.

export interface FormProps {
  policy: Policy
  editable: boolean
  onChange: (policy: Policy) => void
  onRename: (before: BoxKey, after: BoxKey, policy: Policy) => void
  onDelete: (key: BoxKey) => void
  onBufferIssue: (key: string, issue: string | null) => void
}

// In the side panel the object is the panel's top heading (h2); in the Form
// view it sits under its section's h2.
export function ObjectForm({ selection, inPanel = false, ...props }: FormProps & { selection: BoxKey; inPanel?: boolean }) {
  const Title = inPanel ? 'h2' : 'h3'
  const Subtitle = inPanel ? 'h3' : 'h4'
  const { policy, editable, onChange, onRename, onDelete, onBufferIssue } = props
  const [kind, id] = splitKey(selection)
  const item = policy[sections[kind]].find((entry) => entry.id === id)
  const prefix = useId()
  const [makingPublic, setMakingPublic] = useState(false)
  // section is the check last chosen in the endpoint's check list; its
  // control takes the focus.
  const [section, setSection] = useState<string>()
  // The typed ID is applied only while it names no other object of its kind:
  // renaming onto an existing ID would merge the two objects' references.
  const [idText, setIdText] = useState(id)
  useEffect(() => {
    setIdText(id)
    setSection(undefined)
    return () => onBufferIssue(`${selection}.id`, null)
  }, [selection])
  useEffect(() => {
    if (!section) return
    const field = section === 'Route' ? 'target' : section === 'Forward' ? 'api' : section
    const target = document.getElementById(`${prefix}-${field}`)
    const control = target?.matches('input,select,textarea,button')
      ? target
      : target?.querySelector<HTMLElement>('input,select,textarea,button')
    control?.focus()
  }, [selection, section, prefix])
  if (!item) return <p className="hint">Select a box to see and change it.</p>
  const operation = kind === 'operation' ? (item as Operation) : null
  // Endpoints a role can be granted: public ones take no role permission.
  const signedIn = policy.operations.filter((entry) => entry.auth !== 'none')
  const blockedDelete = deleteReason(policy, selection)
  function idProblem(value: string): string | null {
    if (!value) return 'Enter an ID.'
    if (value !== id && policy[sections[kind]].some((entry) => entry.id === value)) return `Another ${kindNames[kind].one} already uses the ID ${value}.`
    return null
  }

  function patch(update: Record<string, unknown>) {
    const next = copy(policy)
    Object.assign(next[sections[kind]].find((entry) => entry.id === id)!, update)
    onChange(next)
  }
  function togglePermit(role: string, operationId: string, checked: boolean) {
    const next = copy(policy)
    next.permits = next.permits.filter((permit) => permit.role !== role || permit.operation !== operationId)
    if (checked) next.permits.push({ role, operation: operationId })
    onChange(next)
  }
  const fieldId = (name: string) => `${prefix}-${name}`
  function input(name: string, label: string, value: string | number, update: (value: string) => void, hint?: string, type = 'text') {
    const controlId = fieldId(name)
    return (
      <Field key={name} id={controlId} label={label} hint={hint}>
        <input
          id={controlId}
          className="input"
          type={type}
          value={value}
          readOnly={!editable}
          spellCheck={false}
          aria-describedby={describedBy(controlId, hint)}
          onChange={(event) => update(event.target.value)}
        />
      </Field>
    )
  }
  function select(name: string, label: string, value: string, values: string[], update: (value: string) => void) {
    const controlId = fieldId(name)
    return (
      <Field key={name} id={controlId} label={label}>
        <select id={controlId} className="input" value={value} disabled={!editable} onChange={(event) => update(event.target.value)}>
          {!values.includes(value) ? <option value={value}>{value || 'Choose one'}</option> : null}
          {values.map((choice) => (
            <option key={choice} value={choice}>
              {choice}
            </option>
          ))}
        </select>
      </Field>
    )
  }

  return (
    <section className="designer-object" data-box={selection} aria-label={`${capital(kindNames[kind].one)} ${id}`}>
      <Title className="designer-object-title">
        {capital(kindNames[kind].one)} <code>{id || '(empty ID)'}</code>
      </Title>
      {operation ? (
        <ol className="designer-checks" aria-label={`Checks for ${operation.id}, in the gateway's order`}>
          {checks(policy, operation).map((check, index) => (
            <li key={check.name}>
              <button
                type="button"
                className={`designer-check${check.tone ? ` tone-${check.tone}` : ' is-none'}`}
                aria-pressed={section === check.name}
                onClick={() => setSection(check.name)}
              >
                <span className="designer-check-step" aria-hidden="true">
                  {index + 1}
                </span>
                <span className="designer-check-name">{check.name}</span>{' '}
                <span className="designer-check-value">{check.code ? <code>{check.value}</code> : check.value}</span>
              </button>
            </li>
          ))}
        </ol>
      ) : null}
      <div className="form-grid">
        {input(
          'id',
          `${capital(kindNames[kind].one)} ID`,
          idText,
          (value) => {
            setIdText(value)
            const problem = idProblem(value)
            onBufferIssue(`${selection}.id`, problem)
            if (problem) return
            const next = rename(policy, selection, value)
            onChange(next)
            onRename(selection, `${kind}:${value}`, next)
          },
          idText !== id && idProblem(idText)
            ? `${idProblem(idText)} The draft keeps the ID ${id || '(empty)'} until this is changed.`
            : kind === 'role'
              ? 'Renaming moves its permissions. Tokens carrying the old name lose them.'
              : 'Changing an ID also changes its references in this draft.',
        )}
        {kind === 'upstream'
          ? input(
              'dial',
              'Address',
              (item as Policy['upstreams'][number]).dial,
              (dial) => patch({ dial }),
              'A service address on this computer, for example 127.0.0.1:9504.',
            )
          : null}
        {kind === 'api'
          ? select(
              'upstream',
              'Service address',
              (item as Policy['apis'][number]).upstream,
              policy.upstreams.map((upstream) => upstream.id),
              (upstream) => patch({ upstream }),
            )
          : null}
        {operation ? (
          <OperationFields
            operation={operation}
            policy={policy}
            editable={editable}
            id={id}
            selection={selection}
            fieldId={fieldId}
            input={input}
            select={select}
            patch={patch}
            onChange={onChange}
            onBufferIssue={onBufferIssue}
            setMakingPublic={setMakingPublic}
          />
        ) : null}
      </div>
      {kind === 'role' ? (
        <fieldset className="designer-permit-list" id={fieldId('Role')}>
          <legend>
            Permissions{' '}
            <span className="muted">
              {policy.permits.filter((permit) => permit.role === id).length} of {signedIn.length}
            </span>
          </legend>
          <p className="hint">Granted to every account whose sign-in token carries this role.</p>
          {[...new Set(signedIn.map((entry) => entry.api))].map((api) => (
            <div className="designer-permit-group" key={api}>
              <Subtitle className="designer-permit-api">{api}</Subtitle>
              <div className="designer-permit-grid">
                {signedIn
                  .filter((entry) => entry.api === api)
                  .map((entry) => (
                    <label key={entry.id} title={entry.id}>
                      <input
                        type="checkbox"
                        checked={policy.permits.some((permit) => permit.role === id && permit.operation === entry.id)}
                        disabled={!editable}
                        aria-label={`${entry.method} ${entry.target} (${entry.id})`}
                        onChange={(event) => togglePermit(id, entry.id, event.target.checked)}
                      />
                      <code>
                        <span className="method">{entry.method}</span> {entry.target}
                      </code>
                    </label>
                  ))}
              </div>
            </div>
          ))}
        </fieldset>
      ) : null}
      {operation ? (
        <fieldset className="designer-permit-list" id={fieldId('Role')}>
          <legend>Permissions</legend>
          {operation.auth === 'none' ? (
            <p className="hint">Public endpoints do not use role permissions.</p>
          ) : (
            <div className="designer-permit-grid">
              {policy.roles.map((role) => (
                <label key={role.id}>
                  <input
                    type="checkbox"
                    checked={policy.permits.some((permit) => permit.role === role.id && permit.operation === id)}
                    disabled={!editable}
                    onChange={(event) => togglePermit(role.id, id, event.target.checked)}
                  />
                  <span>{role.id}</span>
                </label>
              ))}
            </div>
          )}
        </fieldset>
      ) : null}
      {editable ? (
        <div className="designer-delete">
          <button type="button" className="btn btn-danger-quiet" disabled={!!blockedDelete} onClick={() => onDelete(selection)}>
            Delete {kindNames[kind].one}
          </button>
          {blockedDelete ? <p className="hint">{blockedDelete}</p> : null}
        </div>
      ) : null}
      {operation ? (
        <Confirm
          open={makingPublic}
          title="Make this endpoint public?"
          confirmLabel="Make Public"
          onCancel={() => setMakingPublic(false)}
          onConfirm={() => {
            const next = copy(policy)
            const target = next.operations.find((entry) => entry.id === id)!
            target.auth = 'none'
            // A public operation keeps its limit, counted per client address.
            if (target.rate) target.rate.key = 'client_address'
            next.permits = next.permits.filter((permit) => permit.operation !== id)
            onChange(next)
            setMakingPublic(false)
          }}
        >
          <p>
            Making it public removes its {policy.permits.filter((permit) => permit.operation === id).length} permissions
            {operation?.rate ? ' and counts its rate limit per client address' : ''}. Continue?
          </p>
        </Confirm>
      ) : null}
    </section>
  )
}
