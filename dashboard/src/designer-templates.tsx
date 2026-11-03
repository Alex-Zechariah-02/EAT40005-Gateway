import { useEffect, useId, useRef, useState } from 'react'
import type { Policy } from './api'
import { limitSignIn, readOnlyAccess, removeApiAccess, tightenRate, type TemplateResult } from './designer-edits'

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
  // The open menu lies over the page, so a click elsewhere or Escape closes it.
  const [open, setOpen] = useState(false)
  useEffect(() => {
    if (!open) return
    const away = (event: PointerEvent) => {
      if (!menu.current?.contains(event.target as Node)) menu.current?.removeAttribute('open')
    }
    const escape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || !menu.current?.contains(document.activeElement)) return
      menu.current.removeAttribute('open')
      menu.current.querySelector('summary')?.focus()
    }
    document.addEventListener('pointerdown', away)
    document.addEventListener('keydown', escape)
    return () => {
      document.removeEventListener('pointerdown', away)
      document.removeEventListener('keydown', escape)
    }
  }, [open])
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
    <details ref={menu} className="designer-add designer-templates" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary className="btn">Templates</summary>
      {/* nowheel lets the wheel scroll this menu instead of moving the canvas. */}
      <div className="nowheel">
        <p className="hint">Templates edit this draft only.</p>
        <section aria-labelledby={`${id}-login`}>
          <h3 id={`${id}-login`}>Limit Sign-In Attempts</h3>
          <p className="hint">accounts.login: 5 requests per 60 s per client address.</p>
          <button type="button" className="btn btn-small" onClick={() => run(limitSignIn(props.policy))}>
            Apply: limit sign-in attempts
          </button>
        </section>
        <section aria-labelledby={`${id}-access`}>
          <h3 id={`${id}-access`}>A Role's Access to an API</h3>
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
          <h3 id={`${id}-rate`}>Tighten an Endpoint's Rate Limit</h3>
          {select('endpoint', 'Endpoint', chosenEndpoint, endpoints, setEndpoint)}
          <div className="field">
            <label htmlFor={`${id}-count`}>Requests per Window</label>
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
