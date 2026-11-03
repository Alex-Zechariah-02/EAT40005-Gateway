import { useId, useState, type ReactNode } from 'react'
import { newKey, type Outcome, type Override, type OverridesView, type Policy, type Reply, type Revision } from './api'
import { useGatewayState, useMutation, usePaged, useRereadWhen, useResource } from './hooks'
import {
  actorName,
  Badge,
  Confirm,
  describedBy,
  duration,
  Empty,
  Field,
  fieldError,
  ListToolbar,
  matches,
  MoreDetails,
  MutationResult,
  PageHeader,
  Problem,
  ReadStamp,
  Receipt,
  Remaining,
  ShowMore,
  TableScroll,
  time,
  Time,
  Wizard,
  useFocus,
  useTarget,
  XRef,
} from './ui'
import type { ViewProps } from './views'

type Kind = 'permit-exception' | 'permit-containment' | 'rate-exception' | 'rate-containment' | 'operation-containment'

// The supported override actions. Nothing else can be expressed: no other
// control, no free-form patch and no priority.
const kinds: { id: Kind; label: string }[] = [
  { id: 'permit-exception', label: 'Allow Temporarily: Let a Role Call an Endpoint' },
  { id: 'permit-containment', label: 'Restrict Temporarily: Stop a Role Calling an Endpoint' },
  { id: 'rate-exception', label: 'Allow Temporarily: Raise an Endpoint’s Rate Limit' },
  { id: 'rate-containment', label: 'Restrict Temporarily: Lower an Endpoint’s Rate Limit' },
  { id: 'operation-containment', label: 'Restrict Temporarily: Block an Endpoint for Everyone' },
]

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

interface Preset {
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

const statusWords: Record<string, string> = { active: 'Active', revoked: 'Revoked', expired: 'Ended', quarantined: 'Set aside after a restore' }

const statusTones: Record<string, 'pending' | 'neutral' | 'refused'> = {
  active: 'pending',
  revoked: 'neutral',
  expired: 'neutral',
  quarantined: 'refused',
}

// Overrides lists the temporary overrides in force, creates one after the
// gateway has checked it, and revokes one. An override changes the effective
// policy only; the confirmed base stays as it is.
export function Overrides({ admin, can }: ViewProps) {
  const state = useGatewayState()
  const active = useResource<OverridesView>((signal) => admin.get('/admin/overrides', signal), [])
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const [reads, setReads] = useState(0)
  // A rule's end, revocation or a new rule made elsewhere changes the
  // gateway's status, which the shell rereads every 3 s while rules are
  // active; the rules in force, the policy and past rules are then read
  // again from the gateway, not from a local countdown. A reread this page
  // asks for goes the same way.
  useRereadWhen(state.version, () => {
    active.refresh()
    policy.refresh()
    setReads((n) => n + 1)
  })
  const refresh = state.refresh
  const mutation = useMutation(admin, refresh)
  const [revoking, setRevoking] = useState<Override | null>(null)
  // A link from another page, such as the history, names one rule: its row
  // is brought into view and highlighted.
  const linkedRule = useFocus('rule')
  useTarget(active.reply?.kind === 'ok', linkedRule)
  const [query, setQuery] = useState('')
  const [guided, setGuided] = useState(false)

  const view = active.reply?.kind === 'ok' ? active.reply.body : undefined
  const etag = active.reply?.kind === 'ok' ? active.reply.etag : null
  const document = policy.reply?.kind === 'ok' ? policy.reply.body.document : undefined
  const manage = can('manage_policy')
  const shown = (view?.overrides ?? []).filter((o) =>
    matches(query, o.id, `rule ${o.id}`, o.operation, o.role, o.target, overrideChange(o, document)),
  )

  return (
    <>
      <PageHeader
        title="Temporary Rules"
        lede="Time-limited rules that allow or restrict access on top of the active policy."
      >
        <ReadStamp okAt={active.last?.at ?? null} failed={active.failed} loading={active.loading || policy.loading} onRefresh={refresh} />
      </PageHeader>

      <section className="section card" aria-labelledby="overrides-title">
        <div className="card-head card-head-pad">
          <div>
            <h2 id="overrides-title">Active Rules</h2>
            {view ? (
              <p className="hint">
                On version {view.revision}
                {view.provisional ? ' (on trial)' : ''}
              </p>
            ) : null}
          </div>
        </div>
        <div className="card-body">
          {active.reply && active.reply.kind !== 'ok' ? <Problem reply={active.reply} /> : null}
          {view ? (
            <>
              <ListToolbar
                label="Filter active temporary rules"
                query={query}
                onQuery={setQuery}
                shown={shown.length}
                total={view.overrides.length}
                noun={['active rule', 'active rules']}
              >
                {manage && document ? (
                  <button type="button" className="btn btn-primary" onClick={() => setGuided(true)} disabled={view.provisional !== undefined}>
                    Add a Temporary Rule
                  </button>
                ) : null}
                {manage && view.provisional !== undefined ? (
                  <span className="hint">A change is on trial: the gateway refuses new temporary rules until it is kept or undone.</span>
                ) : null}
              </ListToolbar>
              {view.overrides.length === 0 ? (
                <p className="hint card-empty">No temporary rules are active.</p>
              ) : shown.length === 0 ? (
                <p className="hint card-empty">No active rule matches the filter.</p>
              ) : (
                <TableScroll label="Active Temporary Rules">
                  <table>
                    <thead>
                      <tr>
                        <th scope="col">Rule</th>
                        <th scope="col">Kind</th>
                        <th scope="col">Change</th>
                        <th scope="col">Added</th>
                        <th scope="col">Ends</th>
                        {manage ? (
                          <th scope="col">
                            <span className="visually-hidden">Action</span>
                          </th>
                        ) : null}
                      </tr>
                    </thead>
                    <tbody>
                      {shown.map((o) => (
                        <tr key={o.id} data-target={linkedRule === String(o.id) ? 'true' : undefined}>
                          <td className="num">{o.id}</td>
                          <td>
                            <OverrideKind type={o.type} />
                          </td>
                          <td>
                            {overrideChange(o, document)}
                            <span className="cell-sub">
                              {o.operation ? (
                                <XRef view="policy" focus={{ select: `operation:${o.operation}` }}>
                                  <code>{o.target}</code>
                                </XRef>
                              ) : (
                                <code>{o.target}</code>
                              )}
                            </span>
                          </td>
                          <td>
                            <Time value={o.created_at} /> <span className="muted">by {o.actor}</span>
                          </td>
                          <td>
                            <Time value={o.expires_at} />
                            <span className="cell-sub">
                              <Remaining key={o.expires_at} until={o.expires_at} onPassed={refresh} />
                            </span>
                          </td>
                          {manage ? (
                            <td className="row-actions">
                              <button
                                type="button"
                                className="btn btn-small btn-danger-quiet"
                                onClick={() => setRevoking(o)}
                                disabled={mutation.pending}
                              >
                                Revoke
                                <span className="visually-hidden"> rule {o.id}</span>
                              </button>
                            </td>
                          ) : null}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </TableScroll>
              )}
            </>
          ) : null}
          <div className="card-pad-x">
            <MutationResult<Outcome>
              mutation={mutation}
              onRefresh={refresh}
              success={(o) => (
                <>
                  <p className="notice-title">Rule {o.override} revoked. It no longer applies.</p>
                  <Receipt outcome={o} />
                </>
              )}
            />
          </div>
        </div>
      </section>

      {manage && document && view ? (
        <AddRule
          admin={admin}
          policy={document}
          etag={etag}
          confirmed={view.confirmed}
          created={refresh}
          open={guided}
          onClose={() => setGuided(false)}
        />
      ) : null}
      {can('read_history') ? <OverrideHistory admin={admin} reads={reads} /> : null}

      <Confirm
        open={revoking !== null}
        title={`Revoke rule ${revoking?.id ?? ''}?`}
        confirmLabel={`Revoke rule ${revoking?.id ?? ''}`}
        danger
        onCancel={() => setRevoking(null)}
        onConfirm={() => {
          const o = revoking
          setRevoking(null)
          if (o) mutation.run({ method: 'DELETE', path: `/admin/overrides/${o.id}`, key: newKey() })
        }}
      >
        {revoking ? <p>{overrideChange(revoking, document)} ends now.</p> : null}
        <p>
          <code>{revoking?.target}</code> returns to the policy from the next request.
        </p>
      </Confirm>
    </>
  )
}

const lifetimes = [
  { seconds: 300, label: '5 min' },
  { seconds: 900, label: '15 min' },
  { seconds: 3600, label: '1 hour' },
  { seconds: 86400, label: '1 day' },
]

// AddRule is the one way to add a temporary rule: a dialog that starts with
// the quick rules and otherwise asks step by step, then shows every choice on
// one review before the gateway is asked. The result stays on the page.
function AddRule(props: {
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
  const available = presets.filter(
    (p) =>
      props.policy.operations.some((op) => op.id === p.operation && (p.kind.startsWith('rate') ? op.rate : true)) &&
      (!p.role || props.policy.roles.some((r) => r.id === p.role)),
  )
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
              ),
            },
            {
              title: 'How Long',
              problem:
                Number.isInteger(lifetime) && lifetime >= 1 && lifetime <= 86400 ? undefined : 'Enter a whole number of seconds from 1 to 86400.',
              body: (
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

function OverrideHistory({ admin, reads }: { admin: ViewProps['admin']; reads: number }) {
  const linkedRule = useFocus('rule')
  const history = usePaged<Override>(admin, '/admin/overrides/history', [reads])
  return (
    <section className="section card card-pad" aria-labelledby="past-rules-title">
      <h2 id="past-rules-title">Past Rules</h2>
      {history.reply && history.reply.kind !== 'ok' ? <Problem reply={history.reply} /> : null}
      {history.reply?.kind === 'ok' && history.items.length === 0 ? <Empty title="No temporary rule has been added yet." /> : null}
      {history.items.length > 0 ? (
        <TableScroll label="Past Rules">
          <table>
            <thead>
              <tr>
                <th scope="col">Rule</th>
                <th scope="col">Kind</th>
                <th scope="col">Change</th>
                <th scope="col">Added</th>
                <th scope="col">Ends</th>
                <th scope="col">Status</th>
              </tr>
            </thead>
            <tbody>
              {history.items.map((o) => (
                <tr key={o.id} data-target={linkedRule === String(o.id) ? 'true' : undefined}>
                  <td className="num">{o.id}</td>
                  <td>
                    <OverrideKind type={o.type} />
                  </td>
                  <td>
                    {overrideChange(o)}
                    <br />
                    <code className="muted">{o.target}</code>
                  </td>
                  <td>
                    <Time value={o.created_at} /> <span className="muted">by {o.actor}</span>
                  </td>
                  <td>
                    <Time value={o.expires_at} />
                  </td>
                  <td>
                    <Badge tone={statusTones[o.status] ?? 'neutral'}>{statusWords[o.status] ?? o.status}</Badge>
                    {o.resolved_at ? (
                      <span className="cell-sub">
                        <Time value={o.resolved_at} /> by {actorName(o.resolved_by)}
                      </span>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      ) : null}
      <ShowMore list={history} />
    </section>
  )
}
