import type { Activation, OverridesView, Page, Revision, Status } from './api'
import { useGatewayState, useRemembered, useResource } from './hooks'
import { overrideChange } from './overrides'
import { OverviewLive } from './monitor'
import { TrialDecision } from './provisional'
import { admissionText, AdmissionBadge } from './state'
import { Badge, MoreDetails, PageHeader, Problem, ReadStamp, TimeOfDay } from './ui'
import type { ViewProps } from './views'

// Overview answers "is the gateway healthy and what is it enforcing?" at a
// glance: the policy in force, any change on trial, temporary rules, then
// what traffic is doing. Every figure is the gateway's own.
export function Overview({ admin, can, open }: ViewProps) {
  const state = useGatewayState()
  // The last status read; the read stamp and problem notice say when a later
  // read failed.
  const status = state.facts
  const blocked = status && (status.admission !== 'ready' || status.audit_failed)
  return (
    <>
      <PageHeader title="Overview" lede="What the gateway enforces now and how it is deciding requests, as it reports them. Nothing here is decided by this page.">
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
    <section className="section card strip" aria-label="Policy status">
      <div className="tile">
        <p className="tile-label">Policy in force</p>
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
          {status.generation ? <code className="muted" title="Generation in force">{status.generation}</code> : null}
        </p>
        {policy.reply && policy.reply.kind !== 'ok' && !(policy.reply.kind === 'refused' && policy.reply.error === 'no_confirmed_policy') ? (
          <Problem reply={policy.reply} />
        ) : null}
      </div>
      <TrialDecision admin={admin} manage={can('manage_policy')} variant="tile" />
      <div className="tile">
        <p className="tile-label">Temporary rules</p>
        {rules ? (
          <>
            <p className={rules.length ? 'tile-value' : 'tile-value tile-value-quiet'}>{rules.length ? `${rules.length} active` : 'None active'}</p>
            <p className="tile-note">
              {rules.length ? (
                <>
                  {overrideChange(rules[0])} · ends <TimeOfDay value={rules[0].expires_at} />
                  {rules.length > 1 ? ` · and ${rules.length - 1} more` : null}
                </>
              ) : (
                'For incidents: freeze, block or open up an endpoint for a set time.'
              )}
            </p>
          </>
        ) : null}
        <p className="tile-foot">
          <a href="#/overrides">{rules?.length ? 'Manage temporary rules' : 'Add a temporary rule'}</a>
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

// GettingStarted is the first-time guide once a policy is in force: three
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
    { done: status.confirmed !== undefined, text: 'A policy is in force.', go: 'policy' as const, label: 'Policy versions' },
    { done: first !== undefined, text: 'A draft change has been saved, checked and put on trial.', go: 'candidates' as const, label: 'Changes' },
    { done: first?.disposition !== undefined, text: 'A trial has ended: the change was kept, or undone.', go: 'candidates' as const, label: 'Changes' },
  ]
  const done = steps.filter((step) => step.done).length
  return (
    <details className="section card card-pad getting-started">
      <summary>
        Getting started · {done} of {steps.length} done
      </summary>
      <ol className="setup-steps">
        {steps.map((step) => (
          <li key={step.text}>
            <Badge tone={step.done ? 'ok' : 'neutral'}>{step.done ? 'Done' : 'To do'}</Badge> {step.text}{' '}
            <button type="button" className="btn-link" onClick={() => open(step.go)}>
              {step.label}
            </button>
          </li>
        ))}
      </ol>
      <p className="hint">Every change goes live on trial first and is undone at its deadline unless someone keeps it. Temporary rules end by themselves.</p>
      <button type="button" className="btn btn-small" onClick={() => setHidden(true)}>
        Hide this guide
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
