import { useId, useRef, useState, type SyntheticEvent } from 'react'
import type { Policy } from './api'
import { limitSignIn, readOnlyAccess, removeApiAccess, tightenRate, type TemplateResult } from './designer-model'

// closeOtherMenus keeps one toolbar menu open at a time: opening one closes
// the others, which would otherwise lie over it.
export function closeOtherMenus(event: SyntheticEvent<HTMLDetailsElement>) {
  const menu = event.currentTarget
  if (!menu.open) return
  for (const other of menu.parentElement?.querySelectorAll('details[open]') ?? []) if (other !== menu) other.removeAttribute('open')
}

// Templates offers ready-made starting points for common changes. Applying one
// only edits the unsaved draft; the draft is then saved, checked by the
// gateway, put on trial and kept like any other change.
export function Templates(props: { policy: Policy; apply: (document: Policy, summary: string) => void }) {
  const id = useId()
  const menu = useRef<HTMLDetailsElement>(null)
  const roles = props.policy.roles.map((role) => role.id)
  const apis = props.policy.apis.map((api) => api.id)
  const endpoints = props.policy.operations.map((operation) => operation.id)
  const [role, setRole] = useState('')
  const [api, setApi] = useState('')
  const [endpoint, setEndpoint] = useState('')
  const [count, setCount] = useState('5')
  const [problem, setProblem] = useState('')
  // A choice that the draft no longer has falls back to the first one.
  const chosenRole = roles.includes(role) ? role : (roles[0] ?? '')
  const chosenApi = apis.includes(api) ? api : (apis[0] ?? '')
  const chosenEndpoint = endpoints.includes(endpoint) ? endpoint : (endpoints[0] ?? '')
  const run = (result: TemplateResult) => {
    if ('problem' in result) {
      setProblem(result.problem)
      return
    }
    setProblem('')
    menu.current?.removeAttribute('open')
    props.apply(result.document, result.summary)
  }
  const select = (key: string, label: string, value: string, options: string[], change: (value: string) => void) => (
    <div className="field">
      <label htmlFor={`${id}-${key}`}>{label}</label>
      <select id={`${id}-${key}`} className="input" value={value} onChange={(event) => change(event.target.value)}>
        {options.map((option) => (
          <option key={option} value={option}>
            {option}
          </option>
        ))}
      </select>
    </div>
  )
  return (
    <details ref={menu} className="designer-add designer-templates" onToggle={closeOtherMenus}>
      <summary className="btn">Templates</summary>
      <div>
        <p className="hint">Each template changes only this draft. Nothing is saved until you choose Save draft change.</p>
        <section aria-labelledby={`${id}-login`}>
          <h3 id={`${id}-login`}>Limit sign-in attempts</h3>
          <p className="hint">accounts.login: 5 requests per 60 s per client address.</p>
          <button type="button" className="btn btn-small" onClick={() => run(limitSignIn(props.policy))}>
            Apply: limit sign-in attempts
          </button>
        </section>
        <section aria-labelledby={`${id}-access`}>
          <h3 id={`${id}-access`}>A role's access to an API</h3>
          {select('role', 'Role', chosenRole, roles, setRole)}
          {select('api', 'API', chosenApi, apis, setApi)}
          <div className="actions">
            <button type="button" className="btn btn-small" onClick={() => run(readOnlyAccess(props.policy, chosenRole, chosenApi))}>
              Apply: read-only access
            </button>
            <button type="button" className="btn btn-small" onClick={() => run(removeApiAccess(props.policy, chosenRole, chosenApi))}>
              Apply: remove access
            </button>
          </div>
        </section>
        <section aria-labelledby={`${id}-rate`}>
          <h3 id={`${id}-rate`}>Tighten an endpoint's rate limit</h3>
          {select('endpoint', 'Endpoint', chosenEndpoint, endpoints, setEndpoint)}
          <div className="field">
            <label htmlFor={`${id}-count`}>Requests per window</label>
            <input id={`${id}-count`} className="input" type="number" min={1} value={count} onChange={(event) => setCount(event.target.value)} />
          </div>
          <button type="button" className="btn btn-small" onClick={() => run(tightenRate(props.policy, chosenEndpoint, Number(count)))}>
            Apply: tighten the limit
          </button>
        </section>
        {problem ? (
          <p role="status" className="hint">
            {problem}
          </p>
        ) : null}
      </div>
    </details>
  )
}
