import type { Policy } from '../shared/api'
import { describedBy, Field } from '../shared/ui'

// The steps of the temporary-rule dialog and their choices: rule kinds, quick rules and lifetimes.

export type Kind = 'permit-exception' | 'permit-containment' | 'rate-exception' | 'rate-containment' | 'operation-containment'

// The supported override actions. Nothing else can be expressed: no other
// control, no free-form patch and no priority.
export const kinds: { id: Kind; label: string }[] = [
  { id: 'permit-exception', label: 'Allow Temporarily: Let a Role Call an Endpoint' },
  { id: 'permit-containment', label: 'Restrict Temporarily: Stop a Role Calling an Endpoint' },
  { id: 'rate-exception', label: 'Allow Temporarily: Raise an Endpoint’s Rate Limit' },
  { id: 'rate-containment', label: 'Restrict Temporarily: Lower an Endpoint’s Rate Limit' },
  { id: 'operation-containment', label: 'Restrict Temporarily: Block an Endpoint for Everyone' },
]

export interface Preset {
  label: string
  detail: (window?: number) => string
  kind: Kind
  operation: string
  role?: string
  count?: number
  seconds: number
}

// Incident quick actions for the operations of the Fetch baseline policy.
// Each is shown only when its operation (and role) is in the policy.
const presets: Preset[] = [
  {
    label: 'Freeze Ordering',
    detail: () => 'No new orders from anyone for 10 minutes',
    kind: 'operation-containment',
    operation: 'orders.create',
    seconds: 600,
  },
  {
    label: 'Slow Down Sign-In Attempts',
    detail: (window) => `3 attempts per address every ${window ?? 60} s, for 15 minutes`,
    kind: 'rate-containment',
    operation: 'accounts.login',
    count: 3,
    seconds: 900,
  },
  {
    label: 'Raise the Order Limit',
    detail: (window) => `20 orders per customer every ${window ?? 60} s, for 30 minutes`,
    kind: 'rate-exception',
    operation: 'orders.create',
    count: 20,
    seconds: 1800,
  },
  {
    label: 'Stop Riders Taking Jobs',
    detail: () => 'Riders cannot open the job board for 10 minutes',
    kind: 'permit-containment',
    operation: 'orders.jobs',
    role: 'RIDER',
    seconds: 600,
  },
]

const lifetimes = [
  { seconds: 300, label: '5 min' },
  { seconds: 900, label: '15 min' },
  { seconds: 3600, label: '1 hour' },
  { seconds: 86400, label: '1 day' },
]

// Edit wraps a setter so that a change forgets an earlier check.
type Edit = <T>(set: (value: T) => void) => (value: T) => void

// The fields whose errors the gateway can name.
type Errors = Record<'operation' | 'role' | 'count' | 'seconds', string | undefined>

// RuleAction is the first step: a quick rule, or the exact action.
export function RuleAction(props: {
  id: string
  policy: Policy
  kind: Kind
  edit: Edit
  setKind: (value: Kind) => void
  applyPreset: (preset: Preset) => void
}) {
  const { id, kind, edit, setKind, applyPreset } = props
  const available = presets.filter(
    (p) =>
      props.policy.operations.some((op) => op.id === p.operation && (p.kind.startsWith('rate') ? op.rate : true)) &&
      (!p.role || props.policy.roles.some((r) => r.id === p.role)),
  )
  return (
    <>
      {available.length ? (
        <div className="quick-actions" role="group" aria-label="Quick Rules">
          {available.map((preset) => (
            <button key={preset.label} type="button" className="quick-action" onClick={() => applyPreset(preset)}>
              <span className="quick-action-name">{preset.label}</span>
              <span className="quick-action-detail">
                {preset.detail(props.policy.operations.find((op) => op.id === preset.operation)?.rate?.window_seconds)}
              </span>
            </button>
          ))}
        </div>
      ) : null}
      <fieldset className="wizard-choices">
        <legend>{available.length ? 'Or Choose Exactly' : 'Action'}</legend>
        {kinds.map((k) => (
          <label key={k.id} className="check">
            <input type="radio" name={`${id}-kind`} checked={kind === k.id} onChange={() => edit(setKind)(k.id)} />
            <span>{k.label}</span>
          </label>
        ))}
      </fieldset>
    </>
  )
}

// RuleTarget is the second step: the endpoint, and the role or request count.
export function RuleTarget(props: {
  id: string
  policy: Policy
  errors: Errors
  control: string
  base: Policy['operations'][number] | undefined
  roleNote: (roleId: string) => string
  operation: string
  role: string
  count: string
  edit: Edit
  setOperation: (value: string) => void
  setRole: (value: string) => void
  setCount: (value: string) => void
}) {
  const { id, errors, control, base, roleNote, operation, role, count, edit, setOperation, setRole, setCount } = props
  return (
    <div className="form-grid">
      <Field id={`${id}-operation`} label="Endpoint" error={errors.operation} wide>
        <select
          id={`${id}-operation`}
          className="input"
          value={operation}
          aria-invalid={errors.operation ? true : undefined}
          aria-describedby={describedBy(`${id}-operation`, undefined, errors.operation)}
          onChange={(e) => edit(setOperation)(e.target.value)}
        >
          {props.policy.operations.map((op) => (
            <option key={op.id} value={op.id}>
              {op.id} ({op.method} {op.target})
            </option>
          ))}
        </select>
      </Field>
      {control === 'permit' ? (
        <Field id={`${id}-role`} label="Role" error={errors.role}>
          <select
            id={`${id}-role`}
            className="input"
            value={role}
            aria-invalid={errors.role ? true : undefined}
            aria-describedby={describedBy(`${id}-role`, undefined, errors.role)}
            onChange={(e) => edit(setRole)(e.target.value)}
          >
            {props.policy.roles.map((r) => (
              <option key={r.id} value={r.id}>
                {roleNote(r.id) ? `${r.id} (${roleNote(r.id)})` : r.id}
              </option>
            ))}
          </select>
        </Field>
      ) : null}
      {control === 'rate' ? (
        <Field
          id={`${id}-count`}
          label="Temporary Request Count per Window"
          hint={base?.rate ? `The policy allows ${base.rate.count} per ${base.rate.window_seconds} s.` : 'This endpoint has no rate limit.'}
          error={errors.count}
        >
          <input
            id={`${id}-count`}
            className="input input-number"
            type="number"
            inputMode="numeric"
            min={1}
            max={1000000}
            step={1}
            value={count}
            aria-invalid={errors.count ? true : undefined}
            aria-describedby={describedBy(`${id}-count`, 'hint', errors.count)}
            onChange={(e) => edit(setCount)(e.target.value)}
          />
        </Field>
      ) : null}
    </div>
  )
}

// RuleLifetime is the third step: how long the rule lasts.
export function RuleLifetime(props: {
  id: string
  errors: Errors
  lifetime: number
  seconds: string
  edit: Edit
  setSeconds: (value: string) => void
}) {
  const { id, errors, lifetime, seconds, edit, setSeconds } = props
  return (
    <>
      <div className="segmented lifetime-choices" role="group" aria-label="Common Lifetimes">
        {lifetimes.map((choice) => (
          <button key={choice.seconds} type="button" aria-pressed={lifetime === choice.seconds} onClick={() => edit(setSeconds)(String(choice.seconds))}>
            {choice.label}
          </button>
        ))}
      </div>
      <Field id={`${id}-seconds`} label="Lifetime (Seconds)" hint="1 to 86400. It cannot be extended; it can be revoked early." error={errors.seconds}>
        <input
          id={`${id}-seconds`}
          className="input input-number"
          type="number"
          inputMode="numeric"
          min={1}
          max={86400}
          step={1}
          value={seconds}
          aria-invalid={errors.seconds ? true : undefined}
          aria-describedby={describedBy(`${id}-seconds`, 'hint', errors.seconds)}
          onChange={(e) => edit(setSeconds)(e.target.value)}
        />
      </Field>
    </>
  )
}
