import { useEffect, useRef, useState } from 'react'
import type { Operation, Policy } from '../../shared/api'
import {
  capital,
  copy,
  kindNames,
  sections,
  splitKey,
  type BoxKey,
  type BoxKind,
  type GroupAction,
  type Issue,
} from './model'
import { CaretDown, CaretRight } from '@phosphor-icons/react'
import { ActionMenu } from '../../shared/overlays'
import { TableScroll } from '../../shared/ui'
import { type FormProps, ObjectForm } from './object'

// The Designer's Form view: every box listed by kind beside its editor, the issues list and the permissions matrix.

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

function Permits(props: FormProps) {
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
                  <span className="designer-form-group-title">{groupTitles[kind]}</span>{' '}
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
                      onClick={() => pick(key)}
                    >
                      <span className="visually-hidden">{capital(kindNames[kind].one)} </span>
                      {operation ? (
                        <code>
                          <span className="method">{operation.method}</span> {operation.target}
                        </code>
                      ) : (
                        <span>{item.id || '(empty ID)'}</span>
                      )}
                      {operation ? <span className="designer-form-item-id"> {operation.id}</span> : null}
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
