import { useId, useState, type FormEvent } from 'react'
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
} from './ui'
import type { ViewProps } from './views'

type Kind = 'permit-exception' | 'permit-containment' | 'rate-exception' | 'rate-containment' | 'operation-containment'

// The supported override actions. Nothing else can be expressed: no other
// control, no free-form patch and no priority.
const kinds: { id: Kind; label: string }[] = [
  { id: 'permit-exception', label: 'Allow temporarily: let a role call an endpoint' },
  { id: 'permit-containment', label: 'Restrict temporarily: stop a role calling an endpoint' },
  { id: 'rate-exception', label: 'Allow temporarily: raise an endpoint’s rate limit' },
  { id: 'rate-containment', label: 'Restrict temporarily: lower an endpoint’s rate limit' },
  { id: 'operation-containment', label: 'Restrict temporarily: block an endpoint for everyone' },
]

// An exception widens what the base allows and a containment narrows it; the
// Dashboard calls them allowing and restricting temporarily.
export function OverrideKind({ type }: { type: string }) {
  if (type === 'exception') {
    return (
      <Badge tone="pending" glyph="plus">
        Allow temporarily
      </Badge>
    )
  }
  if (type === 'containment') {
    return (
      <Badge tone="refused" glyph="minus">
        Restrict temporarily
      </Badge>
    )
  }
  return <Badge tone="neutral">{type}</Badge>
}

// overrideChange says in words what an override does to the base policy.
export function overrideChange(o: Pick<Override, 'type' | 'control' | 'operation' | 'role' | 'count'>, policy?: Policy): string {
  if (o.control === 'rate') {
    const base = policy?.operations.find((op) => op.id === o.operation)?.rate?.count
    return `${o.operation} limited to ${o.count} requests per window${base !== undefined ? ` (policy in force: ${base})` : ''}`
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
    label: 'Freeze ordering',
    detail: () => 'No new orders from anyone for 10 minutes',
    kind: 'operation-containment',
    operation: 'orders.create',
    seconds: 600,
  },
  {
    label: 'Slow down sign-in attempts',
    detail: (window) => `3 attempts per address every ${window ?? 60} s, for 15 minutes`,
    kind: 'rate-containment',
    operation: 'accounts.login',
    count: 3,
    seconds: 900,
  },
  {
    label: 'Raise the order limit',
    detail: (window) => `20 orders per customer every ${window ?? 60} s, for 30 minutes`,
    kind: 'rate-exception',
    operation: 'orders.create',
    count: 20,
    seconds: 1800,
  },
  {
    label: 'Stop riders taking jobs',
    detail: () => 'Riders cannot open the job board for 10 minutes',
    kind: 'permit-containment',
    operation: 'orders.jobs',
    role: 'RIDER',
    seconds: 600,
  },
]

const statusWords: Record<string, string> = { active: 'In force', revoked: 'Revoked', expired: 'Ended', quarantined: 'Set aside after a restore' }

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
        title="Temporary rules"
        lede="A temporary rule changes what the gateway enforces for a fixed time, then ends by itself: allowing temporarily lets more through, restricting temporarily lets less through. The policy in force never changes, and a rule can be revoked early."
      >
        <ReadStamp okAt={active.last?.at ?? null} failed={active.failed} loading={active.loading || policy.loading} onRefresh={refresh} />
      </PageHeader>

      <section className="section card" aria-labelledby="overrides-title">
        <div className="card-head card-head-pad">
          <div>
            <h2 id="overrides-title">In force</h2>
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
                label="Filter temporary rules in force"
                query={query}
                onQuery={setQuery}
                shown={shown.length}
                total={view.overrides.length}
                noun={['rule in force', 'rules in force']}
              >
                {manage && document ? (
                  <button type="button" className="btn btn-primary" onClick={() => setGuided(true)} disabled={view.provisional !== undefined}>
                    Add a temporary rule
                  </button>
                ) : null}
              </ListToolbar>
              {view.overrides.length === 0 ? (
                <p className="hint card-empty">No temporary rule is in force. The policy applies exactly as kept.</p>
              ) : shown.length === 0 ? (
                <p className="hint card-empty">No rule in force matches the filter.</p>
              ) : (
                <TableScroll label="Temporary rules in force">
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
                        <tr key={o.id}>
                          <td className="num">{o.id}</td>
                          <td>
                            <OverrideKind type={o.type} />
                          </td>
                          <td>
                            {overrideChange(o, document)}
                            <span className="cell-sub">
                              <code>{o.target}</code>
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
                  <p className="notice-title">Rule {o.override} revoked. The policy in force no longer includes it.</p>
                  <Receipt outcome={o} />
                </>
              )}
            />
          </div>
        </div>
      </section>

      {manage && document && view ? (
        <CreateForm
          admin={admin}
          policy={document}
          etag={etag}
          provisional={view.provisional !== undefined}
          confirmed={view.confirmed}
          created={refresh}
          guided={guided}
          onGuided={setGuided}
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
          <code>{revoking?.target}</code> returns to the policy in force from the next request.
        </p>
      </Confirm>
    </>
  )
}

function CreateForm(props: {
  admin: ViewProps['admin']
  policy: Policy
  etag: string | null
  provisional: boolean
  confirmed: number
  created: () => void
  // guided opens the same rule as a step-by-step wizard.
  guided: boolean
  onGuided: (open: boolean) => void
}) {
  const id = useId()
  const [kind, setKind] = useState<Kind>('permit-exception')
  const [operation, setOperation] = useState(props.policy.operations[0]?.id ?? '')
  const [role, setRole] = useState(props.policy.roles[0]?.id ?? '')
  const [count, setCount] = useState('1')
  const [seconds, setSeconds] = useState('300')
  const [check, setCheck] = useState<Reply<Outcome> | null>(null)
  const [checking, setChecking] = useState(false)
  const [confirming, setConfirming] = useState(false)
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

  function submit(event: FormEvent) {
    event.preventDefault()
    setConfirming(true)
  }

  // A quick action fills the form and asks for confirmation at once; the
  // gateway checks it like any other rule.
  function applyPreset(preset: Preset) {
    setKind(preset.kind)
    setOperation(preset.operation)
    if (preset.role) setRole(preset.role)
    if (preset.count) setCount(String(preset.count))
    setSeconds(String(preset.seconds))
    setCheck(null)
    setConfirming(true)
  }

  const base = props.policy.operations.find((op) => op.id === operation)
  const lifetime = Number(seconds)
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
  // A wizard edit forgets an earlier check, as an edit of the form does.
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

  return (
    <section className="section card card-pad" aria-labelledby={`${id}-title`}>
      <h2 id={`${id}-title`}>Add a temporary rule</h2>
      {props.provisional ? (
        <p className="notice tone-pending">A change is on trial: the gateway refuses new temporary rules until it is kept or undone.</p>
      ) : null}
      {available.length ? (
        <div className="quick-actions" role="group" aria-label="Incident quick actions">
          {available.map((preset) => (
            <button
              key={preset.label}
              type="button"
              className="quick-action"
              onClick={() => applyPreset(preset)}
              disabled={mutation.pending || props.provisional}
            >
              <span className="quick-action-name">{preset.label}</span>
              <span className="quick-action-detail">
                {preset.detail(props.policy.operations.find((op) => op.id === preset.operation)?.rate?.window_seconds)}
              </span>
            </button>
          ))}
        </div>
      ) : null}
      <h3 className="form-subhead">Or choose exactly</h3>
      <form onSubmit={submit} onChange={() => setCheck(null)}>
        <div className="form-grid">
          <Field id={`${id}-kind`} label="Action" wide>
            <select id={`${id}-kind`} className="input" value={kind} onChange={(e) => setKind(e.target.value as Kind)}>
              {kinds.map((k) => (
                <option key={k.id} value={k.id}>
                  {k.label}
                </option>
              ))}
            </select>
          </Field>
          <Field id={`${id}-operation`} label="Endpoint" error={errors.operation} wide>
            <select
              id={`${id}-operation`}
              className="input"
              value={operation}
              aria-invalid={errors.operation ? true : undefined}
              aria-describedby={describedBy(`${id}-operation`, undefined, errors.operation)}
              onChange={(e) => setOperation(e.target.value)}
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
                onChange={(e) => setRole(e.target.value)}
              >
                {props.policy.roles.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.id}
                  </option>
                ))}
              </select>
            </Field>
          ) : null}
          {control === 'rate' ? (
            <Field
              id={`${id}-count`}
              label="Temporary request count per window"
              hint={
                base?.rate ? `The policy in force allows ${base.rate.count} per ${base.rate.window_seconds} s.` : 'This endpoint has no rate limit.'
              }
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
                required
                value={count}
                aria-invalid={errors.count ? true : undefined}
                aria-describedby={describedBy(`${id}-count`, 'hint', errors.count)}
                onChange={(e) => setCount(e.target.value)}
              />
            </Field>
          ) : null}
          <Field id={`${id}-seconds`} label="Lifetime (seconds)" hint="1 to 86400. It cannot be extended." error={errors.seconds}>
            <input
              id={`${id}-seconds`}
              className="input input-number"
              type="number"
              inputMode="numeric"
              min={1}
              max={86400}
              step={1}
              required
              value={seconds}
              aria-invalid={errors.seconds ? true : undefined}
              aria-describedby={describedBy(`${id}-seconds`, 'hint', errors.seconds)}
              onChange={(e) => setSeconds(e.target.value)}
            />
          </Field>
        </div>
        <p className="preview">
          <span className="muted">This rule:</span> {sentence}, for{' '}
          {Number.isInteger(lifetime) && lifetime > 0 ? duration(lifetime) : `${seconds} seconds`}.
        </p>
        <div className="actions">
          <button type="button" className="btn" onClick={validate} disabled={checking || mutation.pending}>
            {checking ? 'Checking…' : 'Check with the gateway'}
          </button>
          <button type="submit" className="btn btn-primary" disabled={mutation.pending}>
            Add rule
          </button>
        </div>
      </form>
      <div aria-live="polite">{props.guided ? null : checkResult}</div>
      <MutationResult<Outcome>
        mutation={mutation}
        onRefresh={props.created}
        success={(o) => (
          <>
            <p className="notice-title">
              Rule {o.override} is in force until {time(o.expires_at)}.
            </p>
            <p>The policy in force is unchanged; the rule ends by itself.</p>
            <Receipt outcome={o} />
          </>
        )}
      />
      {props.guided ? (
        <Wizard
          title="Add a temporary rule"
          finishLabel="Add rule"
          onClose={() => props.onGuided(false)}
          onFinish={() => {
            props.onGuided(false)
            setConfirming(true)
          }}
          steps={[
            {
              title: 'What to do',
              body: (
                <fieldset className="wizard-choices">
                  <legend>Action</legend>
                  {kinds.map((k) => (
                    <label key={k.id} className="check">
                      <input type="radio" name={`${id}-wizard-kind`} checked={kind === k.id} onChange={() => edit(setKind)(k.id)} />
                      <span>{k.label}</span>
                    </label>
                  ))}
                </fieldset>
              ),
            },
            {
              title: control === 'permit' ? 'Endpoint and role' : 'Endpoint',
              problem: !props.policy.operations.length
                ? 'This policy has no endpoints.'
                : control === 'rate' && !(Number.isInteger(Number(count)) && Number(count) >= 1)
                  ? 'Enter a whole number of requests, at least 1.'
                  : undefined,
              body: (
                <div className="form-grid">
                  <Field id={`${id}-wizard-operation`} label="Endpoint" wide>
                    <select id={`${id}-wizard-operation`} className="input" value={operation} onChange={(e) => edit(setOperation)(e.target.value)}>
                      {props.policy.operations.map((op) => (
                        <option key={op.id} value={op.id}>
                          {op.id} ({op.method} {op.target})
                        </option>
                      ))}
                    </select>
                  </Field>
                  {control === 'permit' ? (
                    <Field id={`${id}-wizard-role`} label="Role">
                      <select id={`${id}-wizard-role`} className="input" value={role} onChange={(e) => edit(setRole)(e.target.value)}>
                        {props.policy.roles.map((r) => (
                          <option key={r.id} value={r.id}>
                            {r.id}
                          </option>
                        ))}
                      </select>
                    </Field>
                  ) : null}
                  {control === 'rate' ? (
                    <Field
                      id={`${id}-wizard-count`}
                      label="Temporary request count per window"
                      hint={
                        base?.rate
                          ? `The policy in force allows ${base.rate.count} per ${base.rate.window_seconds} s.`
                          : 'This endpoint has no rate limit.'
                      }
                    >
                      <input
                        id={`${id}-wizard-count`}
                        className="input input-number"
                        type="number"
                        inputMode="numeric"
                        min={1}
                        step={1}
                        value={count}
                        aria-describedby={describedBy(`${id}-wizard-count`, 'hint')}
                        onChange={(e) => edit(setCount)(e.target.value)}
                      />
                    </Field>
                  ) : null}
                </div>
              ),
            },
            {
              title: 'How long',
              problem:
                Number.isInteger(lifetime) && lifetime >= 1 && lifetime <= 86400 ? undefined : 'Enter a whole number of seconds from 1 to 86400.',
              body: (
                <Field id={`${id}-wizard-seconds`} label="Lifetime (seconds)" hint="1 to 86400. It cannot be extended; it can be revoked early.">
                  <input
                    id={`${id}-wizard-seconds`}
                    className="input input-number"
                    type="number"
                    inputMode="numeric"
                    min={1}
                    max={86400}
                    step={1}
                    value={seconds}
                    aria-describedby={describedBy(`${id}-wizard-seconds`, 'hint')}
                    onChange={(e) => edit(setSeconds)(e.target.value)}
                  />
                </Field>
              ),
            },
            {
              title: 'Review',
              body: (
                <>
                  <p>
                    <strong>{sentence}</strong>, for {Number.isInteger(lifetime) && lifetime > 0 ? duration(lifetime) : `${seconds} seconds`}.
                  </p>
                  <p className="hint">The policy in force, version {props.confirmed}, does not change. Add rule asks you to confirm once more.</p>
                  <div className="actions">
                    <button type="button" className="btn" onClick={validate} disabled={checking || mutation.pending}>
                      {checking ? 'Checking…' : 'Check with the gateway'}
                    </button>
                  </div>
                  <div aria-live="polite">{checkResult}</div>
                </>
              ),
            },
          ]}
        />
      ) : null}
      <Confirm
        open={confirming}
        title="Add this temporary rule?"
        confirmLabel="Add rule"
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setConfirming(false)
          mutation.run({ method: 'POST', path: '/admin/overrides', body: body(), ifMatch: props.etag ?? undefined, key: newKey() })
        }}
      >
        <p>
          <strong>{sentence}</strong>, from the next request, for{' '}
          {Number.isInteger(lifetime) && lifetime > 0 ? duration(lifetime) : `${seconds} seconds`}. It cannot be extended.
        </p>
        <p className="hint">The policy in force, version {props.confirmed}, does not change. The rule can be revoked before it ends.</p>
      </Confirm>
    </section>
  )
}

function OverrideHistory({ admin, reads }: { admin: ViewProps['admin']; reads: number }) {
  const history = usePaged<Override>(admin, '/admin/overrides/history', [reads])
  return (
    <section className="section">
      <h2>Past rules</h2>
      {history.reply && history.reply.kind !== 'ok' ? <Problem reply={history.reply} /> : null}
      {history.reply?.kind === 'ok' && history.items.length === 0 ? <Empty title="No temporary rule has been added yet." /> : null}
      {history.items.length > 0 ? (
        <TableScroll label="Past rules">
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
                <tr key={o.id}>
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
