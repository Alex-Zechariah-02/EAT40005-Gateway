import type { OverridesView, Revision, Status } from './api'
import { useGatewayState, useResource } from './hooks'
import { overrideChange } from './overrides'
import { OverviewLive } from './monitor'
import { TrialDecision } from './provisional'
import { admissionText, AdmissionBadge } from './state'
import { PageHeader, Problem, ReadStamp, TimeOfDay } from './ui'
import type { ViewProps } from './views'

// Overview answers "is the gateway healthy and what is it enforcing?" at a
// glance: the policy in force, any change on trial, temporary rules, then
// what traffic is doing. Every figure is the gateway's own.
export function Overview({ admin, can, open }: ViewProps) {
  const state = useGatewayState()
  const status = state.reply?.kind === 'ok' ? state.reply.body : undefined
  const blocked = status && (status.admission !== 'ready' || status.audit_failed)
  return (
    <>
      <PageHeader title="Overview" lede="What the gateway enforces now and how it is deciding requests, as it reports them. Nothing here is decided by this page.">
        <ReadStamp readAt={state.readAt} loading={state.loading} onRefresh={state.refresh} />
      </PageHeader>
      {state.reply && state.reply.kind !== 'ok' ? <Problem reply={state.reply} /> : null}
      {status && blocked ? <Admission status={status} /> : null}
      {status && (status.admission === 'bootstrap_locked' || status.admission === 'recovery_required') ? (
        <FirstBase recovery={status.admission === 'recovery_required'} manage={can('manage_policy')} goChanges={() => open('candidates')} />
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
            <p className="tile-value">Revision {status.provisional ?? status.confirmed}</p>
            <p className="tile-note">
              {status.provisional !== undefined ? (
                <>On trial; confirmed revision {status.confirmed} is the fallback</>
              ) : revision ? (
                <>
                  Since <TimeOfDay value={revision.created_at} /> · by {revision.actor}
                </>
              ) : null}
            </p>
          </>
        ) : (
          <>
            <p className="tile-value tile-value-quiet">None yet</p>
            <p className="tile-note">Nothing is admitted until a first policy is established.</p>
          </>
        )}
        <p className="tile-foot">
          <AdmissionBadge status={status} />
          {status.generation ? <code className="muted" title="Policy generation served">{status.generation}</code> : null}
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
                'For incidents: freeze, block or open up an operation for a set time.'
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
        The gateway is not admitting requests
      </h2>
      <p>{status.audit_failed ? admissionText.audit_failed : (admissionText[status.admission] ?? 'Every protected request is refused.')}</p>
      <p className="hint">
        Controller state <code>{status.state}</code> · admission <code>{status.admission}</code>
      </p>
    </section>
  )
}

// FirstBase explains the way out of a new or restored installation, which
// admits nothing until a policy that denies everything is established.
function FirstBase(props: { recovery: boolean; manage: boolean; goChanges: () => void }) {
  return (
    <section className="section card setup" aria-labelledby="first-base-title">
      <h2 id="first-base-title">{props.recovery ? 'Establish a recovery base' : 'Establish the first policy'}</h2>
      <ol className="setup-steps">
        <li>Stage a complete policy document that denies every operation and grants no permits.</li>
        <li>Establish it as the {props.recovery ? 'recovery base' : 'first confirmed policy'}. Requests are still refused, but the gateway is ready.</li>
        <li>Stage the policy you want, put it on trial, check its effect, then confirm it.</li>
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
