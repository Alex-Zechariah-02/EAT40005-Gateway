import { useId, useState, type ReactNode } from 'react'
import type { Operation, Policy } from './api'
import { grantAccess, setRate } from './designer-edits'
import { differences } from './diff'
import { ChangeSet } from './review'
import { Wizard } from './dialogs'
import { EndpointWizard } from './designer-endpoint'

// The Designer's guided tasks: give access and set a rate limit, step by step.

export type GuidedTask = 'access' | 'rate' | 'endpoint'
// api, when given, is the API whose group the wizard was opened from: a new
// endpoint starts there, and a rate limit is chosen among its endpoints.
export type WizardProps = { policy: Policy; api?: string; apply: (document: Policy, summary: string) => void; close: () => void }

export const methods = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS']
export const withBody = new Set(['POST', 'PUT', 'PATCH'])

// GuidedChange runs one common edit as a short wizard, opened from a group's
// + menu. Each ends on a review of what the draft gains or loses and only
// edits the unsaved draft: saving, the gateway's check, the trial and keeping
// follow as for any other change.
export function GuidedChange(props: WizardProps & { task: GuidedTask }) {
  if (props.task === 'access') return <AccessWizard {...props} />
  if (props.task === 'rate') return <RateWizard {...props} />
  return <EndpointWizard {...props} />
}

export function Review(props: { before: Policy; after: Policy; children?: ReactNode }) {
  const changes = differences(props.before, props.after)
  return (
    <>
      {changes.length > 0 ? <ChangeSet changes={changes} /> : <p>This makes no difference to the draft.</p>}
      {props.children}
      <p className="hint">
        Applying changes only this draft. Nothing is saved until you choose Save draft change; the gateway then checks it, and it goes live only
        through a trial you keep.
      </p>
    </>
  )
}

export function Choice(props: { id: string; label: string; value: string; options: [string, string][]; onChange: (value: string) => void }) {
  return (
    <div className="field">
      <label htmlFor={props.id}>{props.label}</label>
      <select id={props.id} className="input" value={props.value} onChange={(event) => props.onChange(event.target.value)}>
        {props.options.map(([value, label]) => (
          <option key={value} value={value}>
            {label}
          </option>
        ))}
      </select>
    </div>
  )
}

export function Whole(props: { id: string; label: string; value: string; onChange: (value: string) => void; hint?: string }) {
  return (
    <div className="field">
      <label htmlFor={props.id}>{props.label}</label>
      <input
        id={props.id}
        className="input input-number"
        type="number"
        inputMode="numeric"
        min={1}
        step={1}
        value={props.value}
        aria-describedby={props.hint ? `${props.id}-hint` : undefined}
        onChange={(event) => props.onChange(event.target.value)}
      />
      {props.hint ? (
        <p id={`${props.id}-hint`} className="hint">
          {props.hint}
        </p>
      ) : null}
    </div>
  )
}

export function wholeNumber(text: string): number | null {
  const value = Number(text)
  return text.trim() !== '' && Number.isInteger(value) && value >= 1 ? value : null
}

function endpointLabel(operation: Operation): string {
  return `${operation.id} (${operation.method} ${operation.target})`
}

function rateText(operation: Operation | undefined): string {
  if (!operation?.rate) return 'no rate limit'
  const per = operation.rate.key === 'client_address' ? 'client address' : 'account'
  return `${operation.rate.count} requests per ${operation.rate.window_seconds} s per ${per}`
}

function AccessWizard({ policy, apply, close }: WizardProps) {
  const id = useId()
  const [role, setRole] = useState(policy.roles[0]?.id ?? '')
  const [chosen, setChosen] = useState<string[]>([])
  const signedIn = policy.operations.filter((operation) => operation.auth !== 'none')
  const allowed = (operation: string) => policy.permits.some((permit) => permit.role === role && permit.operation === operation)
  const fresh = chosen.filter((operation) => !allowed(operation))
  const next = grantAccess(policy, role, fresh)
  const closed = signedIn.filter((operation) => fresh.includes(operation.id) && operation.access !== 'allow')
  return (
    <Wizard
      title="Give a Role Access"
      finishLabel="Apply to the Draft"
      onClose={close}
      onFinish={() => apply(next, `${role} can now call ${fresh.length} more ${fresh.length === 1 ? 'endpoint' : 'endpoints'}.`)}
      steps={[
        {
          title: 'Choose a Role',
          problem: policy.roles.length ? undefined : 'This draft has no roles. Add a role first.',
          body: (
            <>
              <p>Every account whose sign-in token carries the role gains the access.</p>
              <Choice id={`${id}-role`} label="Role" value={role} options={policy.roles.map((item) => [item.id, item.id])} onChange={setRole} />
            </>
          ),
        },
        {
          title: 'Choose Endpoints',
          problem: fresh.length ? undefined : `Choose at least one endpoint ${role} cannot call yet.`,
          body: (
            <fieldset className="wizard-choices">
              <legend>Endpoints That Need a Sign-In</legend>
              {signedIn.map((operation) => (
                <label key={operation.id} className="check">
                  <input
                    type="checkbox"
                    checked={allowed(operation.id) || chosen.includes(operation.id)}
                    disabled={allowed(operation.id)}
                    onChange={(event) => setChosen(event.target.checked ? [...chosen, operation.id] : chosen.filter((item) => item !== operation.id))}
                  />
                  <span>
                    <code>{endpointLabel(operation)}</code>
                    {allowed(operation.id) ? <span className="muted"> · already allowed</span> : null}
                  </span>
                </label>
              ))}
            </fieldset>
          ),
        },
        {
          title: 'Review',
          body: (
            <Review before={policy} after={next}>
              {closed.length ? (
                <p className="notice tone-pending">
                  {closed.map((operation) => operation.id).join(', ')} {closed.length === 1 ? 'is' : 'are'} set to deny every request, so the new
                  permission has no effect until the endpoint is set to allow.
                </p>
              ) : null}
            </Review>
          ),
        },
      ]}
    />
  )
}

function RateWizard({ policy, api, apply, close }: WizardProps) {
  const id = useId()
  // Opened from an API's group, it offers only that API's endpoints.
  const offered = api && policy.operations.some((item) => item.api === api) ? policy.operations.filter((item) => item.api === api) : policy.operations
  const [endpoint, setEndpoint] = useState(offered[0]?.id ?? '')
  const operation = offered.find((item) => item.id === endpoint)
  const [count, setCount] = useState(String(operation?.rate?.count ?? 100))
  const [seconds, setSeconds] = useState(String(operation?.rate?.window_seconds ?? 60))
  const requests = wholeNumber(count)
  const span = wholeNumber(seconds)
  const next = requests && span ? setRate(policy, endpoint, requests, span) : policy
  const unchanged = differences(policy, next).length === 0
  const per = operation?.rate?.key === 'client_address' || operation?.auth === 'none' ? 'client address' : 'account'
  return (
    <Wizard
      title="Change a Rate Limit"
      finishLabel="Apply to the Draft"
      onClose={close}
      onFinish={() => apply(next, `${endpoint} limited to ${requests} requests per ${span} s.`)}
      steps={[
        {
          title: 'Choose an Endpoint',
          problem: operation ? undefined : 'This draft has no endpoints. Add an endpoint first.',
          body: (
            <>
              <Choice
                id={`${id}-endpoint`}
                label="Endpoint"
                value={endpoint}
                options={offered.map((item) => [item.id, endpointLabel(item)])}
                onChange={(value) => {
                  const chosen = offered.find((item) => item.id === value)
                  setEndpoint(value)
                  setCount(String(chosen?.rate?.count ?? 100))
                  setSeconds(String(chosen?.rate?.window_seconds ?? 60))
                }}
              />
              <p className="hint">In this draft now: {rateText(operation)}.</p>
            </>
          ),
        },
        {
          title: 'Set the Limit',
          problem: requests && span ? undefined : 'Enter whole numbers of at least 1.',
          body: (
            <div className="form-grid">
              <Whole id={`${id}-count`} label="Requests" value={count} onChange={setCount} hint={`Counted per ${per}.`} />
              <Whole id={`${id}-seconds`} label="Per How Many Seconds" value={seconds} onChange={setSeconds} />
            </div>
          ),
        },
        {
          title: 'Review',
          problem: unchanged ? 'This is already the limit in the draft. Go back and choose another.' : undefined,
          body: (
            <Review before={policy} after={next}>
              <p className="hint">A request over the limit is refused until enough of the window has passed.</p>
            </Review>
          ),
        },
      ]}
    />
  )
}
