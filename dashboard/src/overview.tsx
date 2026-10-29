import type { Activation, OverridesView, Page, Revision, Status } from './api'
import { useGatewayState, useRemembered, useResource } from './hooks'
import { overrideChange } from './overrides'
import { OverviewLive } from './monitor'
import { TrialDecision } from './provisional'
import { admissionText, AdmissionBadge } from './state'
import { Badge, MoreDetails, PageHeader, Problem, ReadStamp, TimeOfDay } from './ui'
import type { ViewProps } from './views'

// Overview answers "is the gateway healthy and what is it enforcing?" at a
// glance: the active policy, any change on trial, temporary rules, then
// what traffic is doing. Every figure is the gateway's own.
export function Overview({ admin, can, open }: ViewProps) {
  const state = useGatewayState()
  // The last status read; the read stamp and problem notice say when a later
  // read failed.
  const status = state.facts
  const blocked = status && (status.admission !== 'ready' || status.audit_failed)
  return (
    <>
      <PageHeader title="Overview" lede="Gateway health, the active policy and recent request decisions.">
        <ReadStamp okAt={state.last?.at ?? null} failed={state.failed} loading={state.loading} onRefresh={state.refresh} />
      </PageHeader>
      {state.reply && state.reply.kind !== 'ok' ? <Problem reply={state.reply} /> : null}
      {status && blocked ? <Admission status={status} /> : null}
      {status && (status.admission === 'bootstrap_locked' || status.admission === 'recovery_required') ? (
        <FirstBase recovery={status.admission === 'recovery_required'} manage={can('manage_policy')} goChanges={() => open('candidates')} />
      ) : status ? (
        <GettingStarted admin={admin} status={status} open={open} />
      ) : null}
      {status ? <StatusStrip {...{ admin, can, status }} /> : null}
      <OverviewLive admin={admin} history={can('read_history')} />
    </>
  )
}

// StatusStrip is the three things an operator checks first: which policy is
// in force, whether a change is on trial, and which temporary rules apply.
function StatusStrip({ admin, can, status }: Pick<ViewProps, 'admin' | 'can'> & { status: Status }) {
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [status.confirmed])
  const overrides = useResource<OverridesView>((signal) => admin.get('/admin/overrides', signal), [status.generation])
  const revision = policy.reply?.kind === 'ok' ? policy.reply.body : undefined
  const rules = overrides.reply?.kind === 'ok' ? overrides.reply.body.overrides : undefined
  return (
    <section className="section card strip" aria-label="Policy Status">
      <div className="tile">
        <p className="tile-label">Active Policy</p>
        {status.confirmed !== undefined ? (
          <>
            <p className="tile-value">Version {status.provisional ?? status.confirmed}</p>
            <p className="tile-note">
              {status.provisional !== undefined ? (
                <>On trial; version {status.confirmed} returns if it is undone</>
              ) : revision ? (
                <>
                  Since <TimeOfDay value={revision.created_at} /> · by {revision.actor}
                </>
              ) : null}
            </p>
          </>
        ) : status.stored_authority === 'unreadable' ? (
          <>
            <p className="tile-value tile-value-quiet">Not known</p>
            <p className="tile-note">The gateway could not read its stored policy for this answer.</p>
          </>
        ) : (
          <>
            <p className="tile-value tile-value-quiet">None yet</p>
            <p className="tile-note">
              {status.admission === 'recovery_required'
                ? 'Every request is refused until a starting policy is established after the restore.'
                : 'Every request is refused until a first policy is established.'}
            </p>
          </>
        )}
        <p className="tile-foot">
          <AdmissionBadge status={status} />
          {status.generation ? <code className="muted" title="Active Generation">{status.generation}</code> : null}
        </p>
        {policy.reply && policy.reply.kind !== 'ok' && !(policy.reply.kind === 'refused' && policy.reply.error === 'no_confirmed_policy') ? (
          <Problem reply={policy.reply} />
        ) : null}
      </div>
      <TrialDecision admin={admin} manage={can('manage_policy')} variant="tile" />
      <div className={rules?.length ? 'tile' : 'tile tile-empty'}>
        <p className="tile-label">Temporary Rules</p>
        {rules ? (
          rules.length ? (
            <>
              <p className="tile-value">{rules.length} active</p>
              <p className="tile-note">
                {overrideChange(rules[0])} · ends <TimeOfDay value={rules[0].expires_at} />
                {rules.length > 1 ? ` · and ${rules.length - 1} more` : null}
              </p>
            </>
          ) : (
            <p className="tile-value tile-value-quiet">None active</p>
          )
        ) : null}
        <p className="tile-foot">
          <a href="#/overrides">{rules?.length ? 'Manage Temporary Rules' : 'Add a Temporary Rule'}</a>
        </p>
        {overrides.reply && overrides.reply.kind !== 'ok' ? <Problem reply={overrides.reply} /> : null}
      </div>
    </section>
  )
}

function Admission({ status }: { status: Status }) {
  return (
    <section className="section notice tone-refused admission" role="alert" aria-labelledby="admission-title">
      <h2 id="admission-title" className="notice-title">
        The gateway is not accepting requests
      </h2>
      <p>{status.audit_failed ? admissionText.audit_failed : (admissionText[status.admission] ?? 'Every protected request is refused.')}</p>
      <MoreDetails facts={[['Controller state', status.state], ['Admission value', status.admission]]} />
    </section>
  )
}

// guideHidden is the page-memory key that hides the Getting started guide
// until My account shows it again; nothing is stored in the browser.
export const guideHidden = 'overview.guide.hidden'

// GettingStarted is the first-time guide once a policy is active: three
// steps to a first kept or undone change, each ticked from what the gateway
// reports. It links to the pages that do each step and changes nothing itself.
function GettingStarted({ admin, status, open }: Pick<ViewProps, 'admin' | 'open'> & { status: Status }) {
  const [hidden, setHidden] = useRemembered(guideHidden, false)
  const trials = useResource<Page<Activation>>((signal) => admin.get('/admin/activations?limit=1', signal), [status.revision])
  if (hidden) return null
  // The first trial ever is listed first; only one runs at a time, so once it
  // has ended a trial has been kept or undone.
  const first = trials.reply?.kind === 'ok' ? trials.reply.body.items[0] : undefined
  const steps = [
    { done: status.confirmed !== undefined, text: 'A policy is active.', go: 'policy' as const, label: 'Versions' },
    { done: first !== undefined, text: 'A draft change has been saved, checked and put on trial.', go: 'candidates' as const, label: 'Changes' },
    { done: first?.disposition !== undefined, text: 'A trial has ended: the change was kept, or undone.', go: 'candidates' as const, label: 'Changes' },
  ]
  const done = steps.filter((step) => step.done).length
  return (
    <details className="section card card-pad getting-started">
      <summary>
        Getting Started · {done} of {steps.length} done
      </summary>
      <ol className="setup-steps">
        {steps.map((step) => (
          <li key={step.text}>
            <Badge tone={step.done ? 'ok' : 'neutral'}>{step.done ? 'Done' : 'To Do'}</Badge> {step.text}{' '}
            <button type="button" className="btn-link" onClick={() => open(step.go)}>
              {step.label}
            </button>
          </li>
        ))}
      </ol>
      <p className="hint">Changes go live on trial and are undone at the deadline unless kept. Temporary rules end automatically.</p>
      <button type="button" className="btn btn-small" onClick={() => setHidden(true)}>
        Hide This Guide
      </button>
    </details>
  )
}

// FirstBase explains the way out of a new or restored installation, which
// admits nothing until a policy that denies everything is established.
function FirstBase(props: { recovery: boolean; manage: boolean; goChanges: () => void }) {
  return (
    <section className="section card setup" aria-labelledby="first-base-title">
      <h2 id="first-base-title">{props.recovery ? 'Establish a starting policy after the restore' : 'Establish the first policy'}</h2>
      <ol className="setup-steps">
        <li>Save a draft change: a complete policy that denies every endpoint and gives no permissions.</li>
        <li>Establish it as the {props.recovery ? 'starting policy after the restore' : 'first policy'}. Requests are still refused, but the gateway is ready.</li>
        <li>Save the policy you want as a draft, put it on trial, check its effect, then keep it.</li>
      </ol>
      {props.manage ? (
        <button type="button" className="btn btn-primary" onClick={props.goChanges}>
          Go to Changes
        </button>
      ) : (
        <p className="hint">An administrator with the manage_policy permission does this.</p>
      )}
    </section>
  )
}
