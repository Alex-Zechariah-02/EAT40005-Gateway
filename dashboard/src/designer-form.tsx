import { useEffect, useId, useRef, useState } from 'react'
import type { Operation, Policy } from './api'
import { capital, copy, deleteReason, kindNames, rename, sections, splitKey, type BoxKey, type BoxKind, type GroupAction, type Issue } from './designer-model'
import { RequestRules } from './policy'
import { CaretDown, CaretRight } from '@phosphor-icons/react'
import { ActionMenu, Confirm, Field, TableScroll, describedBy } from './ui'

interface FormProps {
  policy: Policy
  editable: boolean
  onChange: (policy: Policy) => void
  onRename: (before: BoxKey, after: BoxKey, policy: Policy) => void
  onDelete: (key: BoxKey) => void
  onBufferIssue: (key: string, issue: string | null) => void
}

// Issues lists what this page can already tell is incomplete, above the
// work and only when there is something to fix. The gateway still checks the
// whole document when it is saved.
export function Issues({ issues, onSelect }: { issues: Issue[]; onSelect: (key: BoxKey) => void }) {
  if (!issues.length) return null
  return (
    <section className="notice tone-pending designer-issues" aria-label="Issues">
      <p className="notice-title">
        {issues.length} {issues.length === 1 ? 'issue' : 'issues'} to fix before saving
      </p>
      <ul>
        {issues.map((issue, index) => (
          <li key={`${issue.field}:${index}`}>
            {issue.message} <code className="soft">{issue.field}</code>{' '}
            {issue.box ? (
              <button className="link-button" type="button" onClick={() => onSelect(issue.box!)}>
                Show
              </button>
            ) : null}
          </li>
        ))}
      </ul>
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
    <Field id={id} label={label} hint="The gateway validates the schema on save." error={error || undefined} wide>
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
                <span className="designer-check-value">{check.value}</span>
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
          <>
            {select('method', 'Method', operation.method, ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'], (method) =>
              patch({ method }),
            )}
            {input(
              'target',
              'Target',
              operation.target,
              (target) => patch({ target }),
              'Exact path, or prefix ending /*. Starts with /; no empty, . or .. segments, encoded characters or backslashes.',
            )}
            {select(
              'api',
              'API',
              operation.api,
              policy.apis.map((api) => api.id),
              (api) => patch({ api }),
            )}
            <fieldset id={fieldId('Access')} className="designer-choice">
              <legend>Endpoint Access</legend>
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
                { value: 'none', label: 'Public (No Token)' },
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
                  <legend>Count Requests</legend>
                  {[
                    { value: 'subject', label: 'Per Account' },
                    { value: 'client_address', label: 'Per Client Address' },
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
                  label="Body Schema (JSON)"
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
                label="Header Schema (JSON)"
                value={operation.headers}
                editable={editable}
                onChange={(headers) => patch({ headers })}
                onIssue={(issue) => onBufferIssue(`${selection}.headers`, issue)}
              />
            ) : null}
            {operation.body || operation.headers ? (
              <details className="api-rules">
                <summary>These Rules in Words</summary>
                <RequestRules operation={operation} />
              </details>
            ) : null}
          </>
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

export function Permits(props: FormProps) {
  const operations = props.policy.operations.filter((operation) => operation.auth !== 'none')
  const apis = [...new Set(operations.map((operation) => operation.api))]
  const short = (operation: Operation) => (operation.id.startsWith(`${operation.api}.`) ? operation.id.slice(operation.api.length + 1) : operation.id)
  return (
    <TableScroll label="Permissions Matrix">
      <table className="matrix-table">
        <thead>
          <tr>
            <th scope="col" rowSpan={2} className="matrix-corner">
              Role
            </th>
            {apis.map((api) => (
              <th scope="colgroup" key={api} colSpan={operations.filter((operation) => operation.api === api).length} className="matrix-api">
                {api}
              </th>
            ))}
          </tr>
          <tr>
            {operations.map((operation) => (
              <th scope="col" key={operation.id} className="matrix-endpoint">
                <span className="visually-hidden">{operation.id}, </span>
                <span aria-hidden="true">{short(operation)}</span>
                <code className="matrix-route">
                  <span className="method">{operation.method}</span> {operation.target}
                </code>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {props.policy.roles.map((role) => (
            <tr key={role.id}>
              <th scope="row" className="matrix-role">
                <code>{role.id}</code>
                <span className="matrix-role-count">
                  {props.policy.permits.filter((permit) => permit.role === role.id).length} of {operations.length}
                </span>
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

// The Form list names its groups as headings, in title case.
const groupTitles: Record<BoxKind, string> = { role: 'Roles', operation: 'Endpoints', api: 'APIs', upstream: 'Service Addresses' }

// listed is a group's boxes in list order. Endpoints are grouped by their
// API, as on the canvas; api is set on the first endpoint of each API.
function listed(policy: Policy, kind: BoxKind): { item: Policy[(typeof sections)[BoxKind]][number]; index: number; api?: string }[] {
  const items = policy[sections[kind]].map((item, index) => ({ item, index }))
  if (kind !== 'operation') return items
  const order = (api: string) => {
    const position = policy.apis.findIndex((entry) => entry.id === api)
    return position < 0 ? policy.apis.length : position
  }
  const sorted = [...items].sort((a, b) => order((a.item as Operation).api) - order((b.item as Operation).api) || a.index - b.index)
  return sorted.map((entry, position) => {
    const api = (entry.item as Operation).api
    const first = position === 0 || (sorted[position - 1].item as Operation).api !== api
    return { ...entry, api: first ? (policy.apis.some((known) => known.id === api) ? api : '') : undefined }
  })
}

// FormView lists every box by kind beside one editor, so a policy of any
// size is a short list to scan instead of one long page of forms.
// It shares the page's selection with the canvas, so the box edited here is
// the one shown selected after switching view, and the other way round.
export function FormView(
  props: FormProps & { actions: (kind: BoxKind) => GroupAction[]; selection: BoxKey | null; onSelect: (key: BoxKey) => void },
) {
  const kinds: BoxKind[] = ['role', 'operation', 'api', 'upstream']
  const first = kinds.flatMap((kind) => props.policy[sections[kind]].map((item) => `${kind}:${item.id}` as BoxKey))[0]
  const [picked, setPicked] = useState<BoxKey | 'permits' | undefined>(() =>
    props.selection && exists(props.policy, props.selection) ? props.selection : first,
  )
  function pick(key: BoxKey) {
    setPicked(key)
    props.onSelect(key)
  }
  const added = useRef<BoxKind | null>(null)
  useEffect(() => {
    const kind = added.current
    if (kind) {
      added.current = null
      const items = props.policy[sections[kind]]
      if (items.length) pick(`${kind}:${items[items.length - 1].id}`)
      return
    }
    if (picked === 'permits' || (picked && exists(props.policy, picked))) return
    setPicked(first ?? 'permits')
  }, [props.policy])
  const shown = picked === 'permits' || (picked && exists(props.policy, picked)) ? picked : first
  // Groups start closed, except the one holding the box being edited.
  const [open, setOpen] = useState<Set<BoxKind>>(() => new Set(first ? [splitKey(first)[0]] : []))
  const openKind = shown && shown !== 'permits' ? splitKey(shown)[0] : undefined
  useEffect(() => {
    if (openKind && !open.has(openKind)) setOpen((previous) => new Set(previous).add(openKind))
  }, [openKind])
  return (
    <div className="designer-form designer-form-split">
      <nav className="designer-form-list" aria-label="Policy objects">
        {kinds.map((kind) => (
          <section key={kind} className={`designer-form-group designer-kind-${kind}`} aria-label={groupTitles[kind]}>
            <div className="designer-form-group-head">
              <h2>
                <button
                  type="button"
                  className="designer-form-group-toggle"
                  aria-label={`${groupTitles[kind]}, ${props.policy[sections[kind]].length}`}
                  aria-expanded={open.has(kind)}
                  aria-controls={`designer-form-list-${kind}`}
                  onClick={() =>
                    setOpen((previous) => {
                      const next = new Set(previous)
                      if (next.has(kind)) next.delete(kind)
                      else next.add(kind)
                      return next
                    })
                  }
                >
                  {open.has(kind) ? <CaretDown size={14} aria-hidden="true" /> : <CaretRight size={14} aria-hidden="true" />}
                  <span className="designer-form-group-title">{groupTitles[kind]}</span>
                  <span className="designer-card-count">{props.policy[sections[kind]].length}</span>
                </button>
              </h2>
              {props.editable ? (
                <ActionMenu
                  className="designer-form-add"
                  label={`Add to ${groupTitles[kind]}`}
                  tip={`Add to ${groupTitles[kind]}`}
                  text={open.has(kind) ? 'Add' : undefined}
                  items={props.actions(kind).map((action) => ({
                    ...action,
                    run: () => {
                      if (action.adds) added.current = action.adds
                      action.run()
                    },
                  }))}
                />
              ) : null}
            </div>
            <ul id={`designer-form-list-${kind}`} hidden={!open.has(kind)}>
              {listed(props.policy, kind).map(({ item, index, api }) => {
                const key: BoxKey = `${kind}:${item.id}`
                const operation = kind === 'operation' ? (item as Operation) : undefined
                return (
                  <li key={`${kind}:${index}`}>
                    {api !== undefined ? <p className="designer-form-subhead">{api ? `${capital(api)} API` : 'No API'}</p> : null}
                    <button
                      type="button"
                      className="designer-form-item"
                      aria-current={shown === key ? 'true' : undefined}
                      aria-label={`${capital(kindNames[kind].one)} ${item.id || '(empty ID)'}`}
                      onClick={() => pick(key)}
                    >
                      {operation ? (
                        <code>
                          <span className="method">{operation.method}</span> {operation.target}
                        </code>
                      ) : (
                        <span>{item.id || '(empty ID)'}</span>
                      )}
                      {operation ? <span className="designer-form-item-id">{operation.id}</span> : null}
                    </button>
                  </li>
                )
              })}
            </ul>
            {kind === 'operation' && !props.policy.apis.length ? <p className="hint">Add an API first.</p> : null}
            {kind === 'api' && !props.policy.upstreams.length ? <p className="hint">Add a service address first.</p> : null}
          </section>
        ))}
        <button type="button" className="designer-form-item designer-form-matrix" aria-current={shown === 'permits' ? 'true' : undefined} onClick={() => setPicked('permits')}>
          Permissions Matrix
        </button>
      </nav>
      <div className="designer-form-detail">
        {shown === 'permits' ? (
          <section aria-labelledby="designer-form-permits">
            <h2 id="designer-form-permits">Permissions Matrix</h2>
            <p className="hint">Tick where a role may call an endpoint. Public endpoints need no role and are not listed.</p>
            <Permits {...props} />
          </section>
        ) : shown ? (
          <ObjectForm
            {...props}
            selection={shown}
            onRename={(before, after, policy) => {
              setPicked(after)
              props.onRename(before, after, policy)
            }}
          />
        ) : (
          <p className="hint">This draft is empty. Add a service address, an API, an endpoint and a role.</p>
        )}
      </div>
    </div>
  )
}

function exists(policy: Policy, key: BoxKey): boolean {
  const [kind, id] = splitKey(key)
  return policy[sections[kind]].some((item) => item.id === id)
}

// checks lists what the gateway checks on an endpoint, in its fixed order.
export function checks(policy: Policy, operation: Operation): { name: string; value: string; tone: 'neutral' | 'refused' | 'pending' | null }[] {
  const api = policy.apis.find((item) => item.id === operation.api)
  return [
    { name: 'Route', value: `${operation.method} ${operation.target}`, tone: 'neutral' },
    { name: 'Access', value: operation.access, tone: operation.access === 'deny' ? 'refused' : 'neutral' },
    { name: 'Sign-In', value: operation.auth === 'none' ? 'public' : 'required', tone: operation.auth === 'none' ? 'pending' : 'neutral' },
    {
      name: 'Role',
      value: operation.auth === 'none' ? 'n/a' : String(policy.permits.filter((permit) => permit.operation === operation.id).length),
      tone: 'neutral',
    },
    {
      name: 'Rate',
      value: operation.rate ? `${operation.rate.count}/${operation.rate.window_seconds}s` : 'none',
      tone: operation.rate ? 'neutral' : null,
    },
    {
      name: 'Body',
      value: operation.body ? `≤ ${operation.body.max_bytes} B${operation.headers ? ' + headers' : ''}` : operation.headers ? 'headers' : 'none',
      tone: operation.body || operation.headers ? 'neutral' : null,
    },
    { name: 'Record', value: 'always', tone: 'neutral' },
    { name: 'Forward', value: `${operation.api} → ${api?.upstream ?? 'missing service address'}`, tone: 'neutral' },
  ]
}
