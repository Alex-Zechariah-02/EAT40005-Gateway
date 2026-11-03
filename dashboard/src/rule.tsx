import { useId, useState, type ReactNode } from 'react'
import { newKey } from './http'
import type { Outcome, Override, Policy, Reply } from './api'
import { useMutation } from './hooks'
import { MutationResult, Problem, Receipt } from './replies'
import { Badge, fieldError, MoreDetails } from './ui'
import { Wizard } from './dialogs'
import { duration, time } from './time'
import type { ViewProps } from './views'
import { kinds, type Kind, type Preset, RuleAction, RuleLifetime, RuleTarget } from './rule-steps'

// Adding a temporary rule: what each rule does, in words, and the step-by-step dialog.

// An exception widens what the base allows and a containment narrows it; the
// Dashboard calls them allowing and restricting temporarily.
export function OverrideKind({ type }: { type: string }) {
  if (type === 'exception') {
    return (
      <Badge tone="pending" glyph="plus">
        Allow Temporarily
      </Badge>
    )
  }
  if (type === 'containment') {
    return (
      <Badge tone="refused" glyph="minus">
        Restrict Temporarily
      </Badge>
    )
  }
  return <Badge tone="neutral">{type}</Badge>
}

// overrideChange says in words what an override does to the base policy.
export function overrideChange(o: Pick<Override, 'type' | 'control' | 'operation' | 'role' | 'count'>, policy?: Policy): string {
  if (o.control === 'rate') {
    const base = policy?.operations.find((op) => op.id === o.operation)?.rate?.count
    return `${o.operation} limited to ${o.count} requests per window${base !== undefined ? ` (policy: ${base})` : ''}`
  }
  if (o.control === 'permit') return `${o.role} ${o.type === 'exception' ? 'may' : 'may not'} call ${o.operation}`
  return `${o.operation} blocked for everyone`
}
// AddRule is the one way to add a temporary rule: a dialog that starts with
// the quick rules and otherwise asks step by step, then shows every choice on
// one review before the gateway is asked. The result stays on the page.
export function AddRule(props: {
  admin: ViewProps['admin']
  policy: Policy
  etag: string | null
  confirmed: number
  created: () => void
  open: boolean
  onClose: () => void
}) {
  const id = useId()
  const [kind, setKind] = useState<Kind>('permit-exception')
  const [operation, setOperation] = useState(props.policy.operations[0]?.id ?? '')
  const [role, setRole] = useState(props.policy.roles[0]?.id ?? '')
  const [count, setCount] = useState('1')
  const [seconds, setSeconds] = useState('300')
  const [step, setStep] = useState(0)
  const [check, setCheck] = useState<Reply<Outcome> | null>(null)
  const [checking, setChecking] = useState(false)
  const mutation = useMutation(props.admin, props.created)

  const [control, type] = kind.split('-') as [string, string]
  const lastReply = mutation.phase.state === 'replied' ? mutation.phase.reply : (check ?? undefined)
  const errors = {
    operation: fieldError(lastReply, 'operation'),
    role: fieldError(lastReply, 'role'),
    count: fieldError(lastReply, 'count'),
    seconds: fieldError(lastReply, 'expires_in_seconds'),
  }

  // The request names only the members its control takes; the server refuses
  // any other member rather than ignoring it.
  function body() {
    const request: Record<string, unknown> = { type, control, operation, expires_in_seconds: Number(seconds) }
    if (control === 'permit') request.role = role
    if (control === 'rate') request.count = Number(count)
    return request
  }

  async function validate() {
    setChecking(true)
    setCheck(await props.admin.send<Outcome>({ method: 'POST', path: '/admin/overrides/validate', body: body(), ifMatch: props.etag ?? undefined }))
    setChecking(false)
  }

  // A quick rule fills every step and goes straight to the review.
  function applyPreset(preset: Preset) {
    setKind(preset.kind)
    setOperation(preset.operation)
    if (preset.role) setRole(preset.role)
    if (preset.count) setCount(String(preset.count))
    setSeconds(String(preset.seconds))
    setCheck(null)
    setStep(3)
  }

  function close() {
    props.onClose()
    setStep(0)
    setCheck(null)
  }

  const base = props.policy.operations.find((op) => op.id === operation)
  const lifetime = Number(seconds)
  const lifetimeText = Number.isInteger(lifetime) && lifetime > 0 ? duration(lifetime) : `${seconds} seconds`
  // An allowance for a role that may already call the endpoint, or a
  // restriction for one that may not, changes nothing and the gateway refuses
  // it, so such a role is marked before it is chosen.
  const permitted = (roleId: string) => props.policy.permits.some((permit) => permit.role === roleId && permit.operation === operation)
  const roleNote = (roleId: string) => {
    if (control !== 'permit') return ''
    if (type === 'exception' && permitted(roleId)) return 'already allowed'
    if (type === 'containment' && !permitted(roleId)) return 'not allowed now'
    return ''
  }
  const checkResult =
    check?.kind === 'ok' ? (
      <div className="notice tone-ok">
        <p className="notice-title">The gateway would accept it now. Nothing has been created.</p>
        <p>If added now, it ends at {time(check.body.expires_at)}.</p>
        <MoreDetails
          facts={[
            ['Target', check.body.target],
            ['Expires', check.body.expires_at],
          ]}
        />
      </div>
    ) : check ? (
      <Problem reply={check} />
    ) : null
  // An edit forgets an earlier check.
  function edit<T>(set: (value: T) => void) {
    return (value: T) => {
      set(value)
      setCheck(null)
    }
  }
  const sentence = overrideChange({ type, control, operation, role, count: Number(count) })
  const review: [string, ReactNode][] = [
    ['Action', kinds.find((k) => k.id === kind)?.label],
    [
      'Endpoint',
      <>
        <code>{operation}</code>
        {base ? <span className="muted"> {`${base.method} ${base.target}`}</span> : null}
      </>,
    ],
  ]
  if (control === 'permit') review.push(['Role', <code>{role}</code>])
  if (control === 'rate')
    review.push(['Requests per window', `${count}${base?.rate ? ` per ${base.rate.window_seconds} s (the policy allows ${base.rate.count})` : ''}`])
  review.push(['Lasts', `${lifetimeText} from the next request. It cannot be extended; it can be revoked early.`])
  review.push(['Policy', `Version ${props.confirmed} stays as it is.`])

  return (
    <>
      <MutationResult<Outcome>
        mutation={mutation}
        onRefresh={props.created}
        success={(o) => (
          <>
            <p className="notice-title">
              Rule {o.override} added. It ends at {time(o.expires_at)} unless revoked earlier.
            </p>
            <p>The policy is unchanged. Active Rules shows whether the rule still applies.</p>
            <Receipt outcome={o} />
          </>
        )}
      />
      {props.open ? (
        <Wizard
          title="Add a Temporary Rule"
          finishLabel="Add Rule"
          finishDisabled={mutation.pending}
          step={step}
          onStep={setStep}
          onClose={close}
          onFinish={() => {
            close()
            mutation.run({ method: 'POST', path: '/admin/overrides', body: body(), ifMatch: props.etag ?? undefined, key: newKey() })
          }}
          steps={[
            {
              title: 'What to Do',
              body: (
                <RuleAction id={id} policy={props.policy} kind={kind} edit={edit} setKind={setKind} applyPreset={applyPreset} />
              ),
            },
            {
              title: control === 'permit' ? 'Endpoint and Role' : 'Endpoint',
              problem: !props.policy.operations.length
                ? 'This policy has no endpoints.'
                : control === 'permit' && roleNote(role)
                  ? `${role} is ${roleNote(role)} on ${operation}. Choose another role or endpoint.`
                  : control === 'rate' && !(Number.isInteger(Number(count)) && Number(count) >= 1)
                    ? 'Enter a whole number of requests, at least 1.'
                    : undefined,
              body: (
                <RuleTarget
                  id={id}
                  policy={props.policy}
                  errors={errors}
                  control={control}
                  base={base}
                  roleNote={roleNote}
                  operation={operation}
                  role={role}
                  count={count}
                  edit={edit}
                  setOperation={setOperation}
                  setRole={setRole}
                  setCount={setCount}
                />
              ),
            },
            {
              title: 'How Long',
              problem:
                Number.isInteger(lifetime) && lifetime >= 1 && lifetime <= 86400 ? undefined : 'Enter a whole number of seconds from 1 to 86400.',
              body: (
                <RuleLifetime id={id} errors={errors} lifetime={lifetime} seconds={seconds} edit={edit} setSeconds={setSeconds} />
              ),
            },
            {
              title: 'Review',
              body: (
                <>
                  <p className="review-summary">
                    <strong>{sentence}</strong>, for {lifetimeText}.
                  </p>
                  <dl className="facts review-facts">
                    {review.map(([term, value]) => (
                      <div className="fact" key={term}>
                        <dt>{term}</dt>
                        <dd>{value}</dd>
                      </div>
                    ))}
                  </dl>
                  <div className="actions">
                    <button type="button" className="btn" onClick={validate} disabled={checking || mutation.pending}>
                      {checking ? 'Checking…' : 'Check with the Gateway'}
                    </button>
                    <button type="button" className="link-button" onClick={() => setStep(0)}>
                      Change Choices
                    </button>
                  </div>
                  <div aria-live="polite">{checkResult}</div>
                </>
              ),
            },
          ]}
        />
      ) : null}
    </>
  )
}
