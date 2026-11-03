import { useId, useState } from 'react'
import { addEndpoint } from './edits'
import { Wizard } from '../../shared/dialogs'
import { countProblem, idProblem, pathProblem, suggestedId } from './inputs'
import { Entry } from './routes'
import { Choice, methods, Review, Whole, wholeNumber, withBody, type WizardProps } from './wizards'

// The new-endpoint wizard: where it is, who may call it, its rate limit, then a review.

export function EndpointWizard({ policy, api: start, apply, close }: WizardProps) {
  const id = useId()
  const [api, setApi] = useState(start && policy.apis.some((item) => item.id === start) ? start : (policy.apis[0]?.id ?? ''))
  const [method, setMethod] = useState('GET')
  const [target, setTarget] = useState('')
  const [name, setName] = useState<string | null>(null)
  const [open, setOpen] = useState(false)
  const [roles, setRoles] = useState<string[]>([])
  const [count, setCount] = useState('100')
  const [seconds, setSeconds] = useState('60')
  // shown lists the steps whose Next was pressed early; their fields say what to fix.
  const [shown, setShown] = useState<number[]>([])
  const reveal = (step: number) => () => setShown((list) => (list.includes(step) ? list : [...list, step]))
  // Until an ID is typed, it follows the API and the path's last fixed part.
  const endpointId = name ?? suggestedId(api, target)
  const requests = wholeNumber(count)
  const span = wholeNumber(seconds)
  const pathError = pathProblem(target)
  const idError = idProblem(endpointId, policy.operations.some((operation) => operation.id === endpointId) ? 'This draft already has an endpoint with this ID.' : undefined)
  const where = !policy.apis.length ? 'Add an API first.' : pathError || idError ? 'Fix the marked fields to continue.' : undefined
  const next =
    !where && requests && span
      ? addEndpoint(policy, { id: endpointId, api, method, target, open, roles, count: requests, windowSeconds: span })
      : policy
  return (
    <Wizard
      title="Add an Endpoint"
      finishLabel="Apply to the Draft"
      onClose={close}
      onFinish={() => apply(next, `Endpoint ${endpointId} added (${method} ${target}).`)}
      steps={[
        {
          title: 'Where It Is',
          problem: where,
          reveal: reveal(0),
          body: (
            <div className="form-grid">
              <Choice id={`${id}-api`} label="API" value={api} options={policy.apis.map((item) => [item.id, item.id])} onChange={setApi} />
              <Choice id={`${id}-method`} label="Method" value={method} options={methods.map((item) => [item, item])} onChange={setMethod} />
              <Entry wide id={`${id}-target`} label="Path" value={target} placeholder="/reports/daily" onChange={setTarget} error={shown.includes(0) ? pathError : undefined} />
              <Entry wide id={`${id}-name`} label="Endpoint ID" value={endpointId} onChange={setName} error={shown.includes(0) ? idError : undefined} />
            </div>
          ),
        },
        {
          title: 'Who May Call It',
          body: (
            <>
              <fieldset className="wizard-choices">
                <legend>Sign-In</legend>
                <label className="check">
                  <input type="radio" name={`${id}-open`} checked={!open} onChange={() => setOpen(false)} />
                  <span>Signed-In Accounts with a Permission</span>
                </label>
                <label className="check">
                  <input type="radio" name={`${id}-open`} checked={open} onChange={() => setOpen(true)} />
                  <span>Anyone, Without Signing In</span>
                </label>
              </fieldset>
              {open ? null : (
                <fieldset className="wizard-choices">
                  <legend>Roles That May Call It</legend>
                  {policy.roles.map((role) => (
                    <label key={role.id} className="check">
                      <input
                        type="checkbox"
                        checked={roles.includes(role.id)}
                        onChange={(event) => setRoles(event.target.checked ? [...roles, role.id] : roles.filter((item) => item !== role.id))}
                      />
                      <span>{role.id}</span>
                    </label>
                  ))}
                  {roles.length === 0 ? <p className="hint">With no role chosen, no account can call it yet.</p> : null}
                </fieldset>
              )}
            </>
          ),
        },
        {
          title: 'Rate Limit',
          problem: requests && span ? undefined : 'Enter whole numbers of at least 1.',
          reveal: reveal(2),
          body: (
            <div className="form-grid">
              <Whole
                id={`${id}-count`}
                label="Requests"
                value={count}
                onChange={setCount}
                hint={`Counted per ${open ? 'client address' : 'account'}.`}
                error={shown.includes(2) ? countProblem(count) : undefined}
              />
              <Whole
                id={`${id}-seconds`}
                label="Per How Many Seconds"
                value={seconds}
                onChange={setSeconds}
                error={shown.includes(2) ? countProblem(seconds) : undefined}
              />
            </div>
          ),
        },
        {
          title: 'Review',
          body: (
            <Review before={policy} after={next}>
              {withBody.has(method) ? (
                <p className="hint">This endpoint has no request body rule yet. If it takes a body, select the endpoint on the canvas and add one.</p>
              ) : null}
            </Review>
          ),
        },
      ]}
    />
  )
}
