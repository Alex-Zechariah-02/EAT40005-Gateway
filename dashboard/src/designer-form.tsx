import { useEffect, useId, useState } from 'react'
import type { Operation, Policy } from './api'
import { capital, copy, deleteReason, kindNames, rename, sections, splitKey, type BoxKey, type BoxKind, type Issue } from './designer-model'
import { RequestRules } from './policy'
import { Badge, Confirm, Field, TableScroll, describedBy } from './ui'

interface FormProps {
  policy: Policy
  editable: boolean
  onChange: (policy: Policy) => void
  onRename: (before: BoxKey, after: BoxKey, policy: Policy) => void
  onDelete: (key: BoxKey) => void
  onBufferIssue: (key: string, issue: string | null) => void
}

export function Issues({ issues, onSelect }: { issues: Issue[]; onSelect: (key: BoxKey) => void }) {
  return (
    <section className="designer-issues" aria-label="Issues">
      <h2>
        Issues <Badge tone={issues.length ? 'pending' : 'neutral'}>{issues.length}</Badge>
      </h2>
      <p className="hint">Checks in this page. The gateway checks the whole document when you save it as a draft change.</p>
      {issues.length ? (
        <ul>
          {issues.map((issue, index) => (
            <li key={`${issue.field}:${index}`}>
              <code>
                {issue.id} {issue.field}
              </code>
              : {issue.message}{' '}
              {issue.box ? (
                <button className="btn btn-small" type="button" onClick={() => onSelect(issue.box!)}>
                  Select box
                </button>
              ) : null}
            </li>
          ))}
        </ul>
      ) : (
        <p className="hint">No drawing issue. Make a change, then save it as a draft change for the gateway to check.</p>
      )}
    </section>
  )
}

function SchemaEditor({
  label,
  value,
  editable,
  onChange,
  onIssue,
}: {
  label: string
  value: unknown
  editable: boolean
  onChange: (value: unknown) => void
  onIssue: (message: string | null) => void
}) {
  const id = useId()
  const serialised = JSON.stringify(value, null, 2)
  const [text, setText] = useState(serialised)
  const [error, setError] = useState('')
  useEffect(() => {
    setText(serialised)
    setError('')
    onIssue(null)
  }, [serialised])
  return (
    <Field id={id} label={label} hint="The gateway checks the schema subset when you save this as a draft change." error={error || undefined} wide>
      <textarea
        id={id}
        className="input code-input"
        value={text}
        readOnly={!editable}
        spellCheck={false}
        aria-invalid={!!error}
        aria-describedby={describedBy(id, 'hint', error)}
        onChange={(event) => {
          const next = event.target.value
          setText(next)
          try {
            const parsed: unknown = JSON.parse(next)
            if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Use a JSON object.')
            setError('')
            onIssue(null)
            onChange(parsed)
          } catch (caught) {
            const message = caught instanceof Error ? caught.message : 'Enter a JSON object.'
            setError(message)
            onIssue(message)
          }
        }}
      />
    </Field>
  )
}

// In the side panel the object is the panel's top heading (h2); in the Form
// view it sits under its section's h2.
export function ObjectForm({
  selection,
  section,
  inPanel = false,
  ...props
}: FormProps & { selection: BoxKey; section?: string; inPanel?: boolean }) {
  const Title = inPanel ? 'h2' : 'h3'
  const Subtitle = inPanel ? 'h3' : 'h4'
  const { policy, editable, onChange, onRename, onDelete, onBufferIssue } = props
  const [kind, id] = splitKey(selection)
  const item = policy[sections[kind]].find((entry) => entry.id === id)
  const prefix = useId()
  const [makingPublic, setMakingPublic] = useState(false)
  // The typed ID is applied only while it names no other object of its kind:
  // renaming onto an existing ID would merge the two objects' references.
  const [idText, setIdText] = useState(id)
  useEffect(() => {
    setIdText(id)
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
              ? 'Changing a role ID changes its permissions. Tokens that still carry the old name lose them.'
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
          <>
            {select('method', 'Method', operation.method, ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'], (method) =>
              patch({ method }),
            )}
            {input(
              'target',
              'Target',
              operation.target,
              (target) => patch({ target }),
              'Exact path or prefix ending /*; starts with /; no empty, . or .. segments, encoded characters or backslashes. The gateway checks the complete syntax.',
            )}
            {select(
              'api',
              'API',
              operation.api,
              policy.apis.map((api) => api.id),
              (api) => patch({ api }),
            )}
            <fieldset id={fieldId('Access')} className="designer-choice">
              <legend>Endpoint access</legend>
              {['allow', 'deny'].map((access) => (
                <label key={access}>
                  <input
                    type="radio"
                    name={fieldId('access')}
                    value={access}
                    checked={operation.access === access}
                    disabled={!editable}
                    onChange={() => patch({ access })}
                  />{' '}
                  {access}
                </label>
              ))}
            </fieldset>
            <fieldset id={fieldId('Sign-in')} className="designer-choice">
              <legend>Token</legend>
              {[
                { value: 'required', label: 'Required' },
                { value: 'none', label: 'Public (no token)' },
              ].map((auth) => (
                <label key={auth.value}>
                  <input
                    type="radio"
                    name={fieldId('auth')}
                    checked={operation.auth === auth.value}
                    disabled={!editable}
                    onChange={() => {
                      const rateChanges = operation.rate && operation.rate.key !== 'client_address'
                      if (auth.value === 'none' && (rateChanges || policy.permits.some((permit) => permit.operation === id)))
                        setMakingPublic(true)
                      else patch({ auth: auth.value })
                    }}
                  />{' '}
                  {auth.label}
                </label>
              ))}
            </fieldset>
            <div className="field field-wide" id={fieldId('Rate')}>
              <label className="designer-option">
                <input
                  type="checkbox"
                  checked={!!operation.rate}
                  disabled={!editable}
                  onChange={(event) => {
                    const next = copy(policy)
                    const target = next.operations.find((entry) => entry.id === id)!
                    const key = operation.auth === 'none' ? 'client_address' : 'subject'
                    if (event.target.checked) target.rate = { algorithm: 'sliding-window', key, count: 100, window_seconds: 60 }
                    else delete target.rate
                    onChange(next)
                  }}
                />{' '}
                Rate limit
              </label>
              <p className="hint">Sliding window, per endpoint. Counters survive policy changes.</p>
            </div>
            {operation.rate ? (
              <>
                <fieldset id={fieldId('rate-key')} className="designer-choice">
                  <legend>Count requests</legend>
                  {[
                    { value: 'subject', label: 'Per account' },
                    { value: 'client_address', label: 'Per client address' },
                  ].map((key) => (
                    <label key={key.value}>
                      <input
                        type="radio"
                        name={fieldId('rate-key')}
                        checked={operation.rate!.key === key.value}
                        disabled={!editable || (key.value === 'subject' && operation.auth === 'none')}
                        onChange={() => patch({ rate: { ...operation.rate!, key: key.value } })}
                      />{' '}
                      {key.label}
                      {key.value === 'subject' && operation.auth === 'none' ? <span className="hint"> (needs a token)</span> : null}
                    </label>
                  ))}
                </fieldset>
                {input(
                  'rate-count',
                  'Request count',
                  operation.rate.count,
                  (value) => patch({ rate: { ...operation.rate!, count: Number(value) } }),
                  undefined,
                  'number',
                )}
                {input(
                  'rate-window',
                  'Window (seconds)',
                  operation.rate.window_seconds,
                  (value) => patch({ rate: { ...operation.rate!, window_seconds: Number(value) } }),
                  undefined,
                  'number',
                )}
              </>
            ) : null}
            <div className="field field-wide" id={fieldId('Body')}>
              <label className="designer-option">
                <input
                  type="checkbox"
                  checked={!!operation.body}
                  disabled={!editable}
                  onChange={(event) => {
                    const next = copy(policy)
                    const target = next.operations.find((entry) => entry.id === id)!
                    if (event.target.checked)
                      target.body = { max_bytes: 1024, schema: { type: 'object', properties: {}, additionalProperties: false } }
                    else delete target.body
                    onChange(next)
                  }}
                />{' '}
                Body rule
              </label>
            </div>
            {operation.body ? (
              <>
                {input(
                  'body-max',
                  'Maximum body bytes',
                  operation.body.max_bytes,
                  (value) => patch({ body: { ...operation.body!, max_bytes: Number(value) } }),
                  undefined,
                  'number',
                )}
                <SchemaEditor
                  label="Body schema (JSON)"
                  value={operation.body.schema}
                  editable={editable}
                  onChange={(schema) => patch({ body: { ...operation.body!, schema } })}
                  onIssue={(issue) => onBufferIssue(`${selection}.body`, issue)}
                />
              </>
            ) : null}
            <div className="field field-wide">
              <label className="designer-option">
                <input
                  type="checkbox"
                  checked={operation.headers !== undefined}
                  disabled={!editable}
                  onChange={(event) => {
                    const next = copy(policy)
                    const target = next.operations.find((entry) => entry.id === id)!
                    if (event.target.checked) target.headers = { type: 'object', properties: {}, additionalProperties: true }
                    else delete target.headers
                    onChange(next)
                  }}
                />{' '}
                Header rule
              </label>
            </div>
            {operation.headers !== undefined ? (
              <SchemaEditor
                label="Header schema (JSON)"
                value={operation.headers}
                editable={editable}
                onChange={(headers) => patch({ headers })}
                onIssue={(issue) => onBufferIssue(`${selection}.headers`, issue)}
              />
            ) : null}
            {operation.body || operation.headers ? (
              <details className="api-rules">
                <summary>These rules in words</summary>
                <RequestRules operation={operation} />
              </details>
            ) : null}
          </>
        ) : null}
      </div>
      {kind === 'role' ? (
        <>
          <Subtitle className="designer-object-subtitle">Who holds it</Subtitle>
          <p className="hint">
            Each person's sign-in token, issued by Fetch Accounts, carries their roles. The policy decides what a role may call; an account is
            stopped on the Accounts page.
          </p>
        </>
      ) : null}
      {kind === 'role' || operation ? (
        <fieldset className="designer-permit-list" id={fieldId('Role')}>
          <legend>Permissions</legend>
          {operation?.auth === 'none' ? (
            <p className="hint">Public endpoints do not use role permissions.</p>
          ) : (
            (kind === 'role'
              ? policy.operations
                  .filter((entry) => entry.auth !== 'none')
                  .map((entry) => ({ role: id, operation: entry.id, label: `${entry.id}: ${entry.method} ${entry.target}` }))
              : policy.roles.map((role) => ({ role: role.id, operation: id, label: role.id }))
            ).map((permit) => (
              <label key={`${permit.role}:${permit.operation}`}>
                <input
                  type="checkbox"
                  checked={policy.permits.some((entry) => entry.role === permit.role && entry.operation === permit.operation)}
                  disabled={!editable}
                  onChange={(event) => togglePermit(permit.role, permit.operation, event.target.checked)}
                />{' '}
                <span>{permit.label}</span>
              </label>
            ))
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
      {section ? <p className="hint">Selected check: {section}. Its controls are shown above.</p> : null}
      {operation ? (
        <Confirm
          open={makingPublic}
          title="Make this endpoint public?"
          confirmLabel="Make public"
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

export function Permits(props: FormProps) {
  const operations = props.policy.operations.filter((operation) => operation.auth !== 'none')
  return (
    <TableScroll label="Permissions matrix">
      <table className="matrix-table">
        <thead>
          <tr>
            <th scope="col">Role</th>
            {operations.map((operation) => (
              <th scope="col" key={operation.id}>
                <code>{operation.id}</code>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {props.policy.roles.map((role) => (
            <tr key={role.id}>
              <th scope="row">
                <code>{role.id}</code>
              </th>
              {operations.map((operation) => (
                <td key={operation.id}>
                  <label className="matrix-cell">
                    <span className="visually-hidden">
                      {role.id} may call {operation.id}
                    </span>
                    <input
                      type="checkbox"
                      disabled={!props.editable}
                      checked={props.policy.permits.some((permit) => permit.role === role.id && permit.operation === operation.id)}
                      onChange={(event) => {
                        const next = copy(props.policy)
                        next.permits = next.permits.filter((permit) => permit.role !== role.id || permit.operation !== operation.id)
                        if (event.target.checked) next.permits.push({ role: role.id, operation: operation.id })
                        props.onChange(next)
                      }}
                    />
                  </label>
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </TableScroll>
  )
}

const formSections = [
  ['role', 'Roles'],
  ['operation', 'Endpoints'],
  ['api', 'APIs'],
  ['upstream', 'Service addresses'],
  ['permits', 'Permissions'],
] as const

export function FormView(props: FormProps & { onAdd: (kind: BoxKind) => void }) {
  return (
    <div className="designer-form">
      <nav className="designer-form-nav" aria-label="Form sections">
        {formSections.map(([key, label]) => (
          <button
            className="btn btn-small"
            type="button"
            key={key}
            onClick={() => document.getElementById(`designer-form-${key}`)?.focus()}
          >
            {label}
          </button>
        ))}
      </nav>
      {(['role', 'operation', 'api', 'upstream'] as BoxKind[]).map((kind) => (
        <section className="section" key={kind} id={`designer-form-${kind}`} tabIndex={-1}>
          <div className="section-head">
            <h2>{capital(kindNames[kind].many)}</h2>
            {props.editable ? (
              <button
                className="btn btn-small"
                type="button"
                disabled={(kind === 'operation' && !props.policy.apis.length) || (kind === 'api' && !props.policy.upstreams.length)}
                onClick={() => props.onAdd(kind)}
              >
                Add {kindNames[kind].one}
              </button>
            ) : null}
          </div>
          {props.policy[sections[kind]].map((item, index) => (
            <div className="panel" key={`${kind}:${index}`}>
              <ObjectForm {...props} selection={`${kind}:${item.id}`} />
            </div>
          ))}
          {!props.policy[sections[kind]].length ? (
            <p className="hint">
              No {kindNames[kind].many}.{' '}
              {props.editable ? `Use Add ${kindNames[kind].one} to create one.` : 'Inspect another view or policy mode.'}
            </p>
          ) : null}
          {kind === 'operation' && !props.policy.apis.length ? <p className="hint">Add an API before adding an endpoint.</p> : null}
          {kind === 'api' && !props.policy.upstreams.length ? <p className="hint">Add a service address before adding an API.</p> : null}
        </section>
      ))}
      <section className="section" id="designer-form-permits" tabIndex={-1}>
        <h2>Permissions</h2>
        <Permits {...props} />
      </section>
    </div>
  )
}
