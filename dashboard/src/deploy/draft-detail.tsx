import { useEffect, useId, useRef, useState } from 'react'
import { newKey } from '../shared/http'
import type { Candidate, Outcome, Revision } from '../shared/api'
import { differences, removedApis } from '../shared/diff'
import { useMutation, useResource } from '../shared/hooks'
import { ImpactPreview } from './impact'
import { Badge, describedBy, Field, fieldError } from '../shared/ui'
import { ChangeSet } from '../shared/review'
import { Confirm, TypedConfirmation } from '../shared/dialogs'
import { duration, time } from '../shared/time'
import { MutationResult, Problem, Receipt } from '../shared/replies'
import type { ViewProps } from '../app/views'
import { CandidateFacts, CandidateStatus, Stepper } from './draft'

// One draft change: its review, impact preview, trial or withdrawal, and their confirmations.

type Action = 'activate' | 'withdraw' | 'genesis' | 'recovery'

// A trial shorter than this leaves too little time for real requests to
// arrive, for a few rereads of the trial's traffic and for a deliberate
// decision. It is advice only: the gateway accepts 1 to 86400 seconds.
const adviseTrialSeconds = 120
const shortTrialAdvice =
  'Under 2 minutes leaves little time to observe real traffic. If not kept by the deadline, the change is undone automatically.'

export function CandidateDetail(props: {
  id: number
  admin: ViewProps['admin']
  confirmed?: Revision
  ifMatch?: string
  mode?: string
  manage: boolean
  changed: () => void
}) {
  const formId = useId()
  const heading = useRef<HTMLHeadingElement>(null)
  const detail = useResource<Candidate>((signal) => props.admin.get(`/admin/candidates/${props.id}`, signal), [props.id])
  const mutation = useMutation(props.admin, () => {
    detail.refresh()
    props.changed()
  })
  const [seconds, setSeconds] = useState('300')
  // step is where a usable change is in its guided path: review it, preview
  // its impact, then start its trial.
  const [step, setStep] = useState(0)
  const [asking, setAsking] = useState<Action | null>(null)
  const [sent, setSent] = useState<Action | null>(null)
  const lastReply = mutation.phase.state === 'replied' ? mutation.phase.reply : undefined
  const secondsError = fieldError(lastReply, 'confirm_within_seconds')
  const candidate = detail.reply?.kind === 'ok' ? detail.reply.body : undefined
  const usable = candidate?.status === 'validated'
  const changes = props.confirmed?.document && candidate?.document ? differences(props.confirmed.document, candidate.document) : null
  // The riskiest trials are confirmed by typing a word: a policy that denies
  // everything, or one that removes a whole API.
  const removed = changes ? removedApis(changes) : []
  const confirmWord = candidate?.deny_all ? 'deny all' : removed.length > 0 ? removed[0] : ''
  const [typed, setTyped] = useState('')
  useEffect(() => setTyped(''), [asking])

  // Opening a change for review moves focus to it.
  useEffect(() => {
    heading.current?.focus()
  }, [])

  function act(kind: Action) {
    setAsking(null)
    if (!candidate) return
    setSent(kind)
    if (kind === 'withdraw') {
      mutation.run({ method: 'DELETE', path: `/admin/candidates/${candidate.id}`, key: newKey() })
    } else if (kind === 'activate') {
      mutation.run({
        method: 'POST',
        path: '/admin/provisional',
        body: { candidate: candidate.id, checksum: candidate.checksum, confirm_within_seconds: Number(seconds) },
        ifMatch: props.ifMatch,
        key: newKey(),
      })
    } else {
      // Genesis and recovery have a fixed server identity, so resending the
      // same request after a lost answer returns the recorded receipt.
      mutation.run({
        method: 'POST',
        path: kind === 'genesis' ? '/admin/genesis' : '/admin/recovery-base',
        body: { candidate: candidate.id, checksum: candidate.checksum },
      })
    }
  }

  const period = Number(seconds)
  const shortTrial = Number.isInteger(period) && period > 0 && period < adviseTrialSeconds
  const guided = usable && props.mode === 'normal'
  return (
    <section className="section card card-pad detail" aria-labelledby={`${formId}-title`}>
      <div className="detail-head">
        <h2 id={`${formId}-title`} ref={heading} tabIndex={-1}>
          Change {props.id}
        </h2>
        {candidate ? <CandidateStatus status={candidate.status} /> : null}
        {candidate?.active ? <Badge tone="ok">Active</Badge> : null}
      </div>
      {guided ? <Stepper step={step} onStep={setStep} /> : null}
      {detail.loading ? <p className="hint">Reading from the gateway…</p> : null}
      {detail.reply && detail.reply.kind !== 'ok' ? <Problem reply={detail.reply} /> : null}
      {candidate ? (
        <>
          {!guided || step === 0 ? (
            <CandidateFacts candidate={candidate} changes={changes} />
          ) : null}

          {guided && step === 1 ? <ImpactPreview admin={props.admin} candidate={candidate.id} /> : null}
          {guided && step === 2 && !props.manage ? <p className="hint">Starting a trial needs the “Change the policy” permission.</p> : null}

          {props.manage && usable && (!guided || step === 2) ? (
            <div className="detail-actions">
              {props.mode === 'normal' ? (
                <form
                  onSubmit={(e) => {
                    e.preventDefault()
                    setAsking('activate')
                  }}
                >
                  <h3>Put on Trial</h3>
                  <Field
                    id={`${formId}-seconds`}
                    label="Trial Length (Seconds)"
                    hint="1 to 86400. Unless someone keeps the change within this time, the gateway undoes it and returns to the active version now by itself."
                    error={secondsError}
                  >
                    <input
                      id={`${formId}-seconds`}
                      className="input input-number"
                      type="number"
                      inputMode="numeric"
                      min={1}
                      max={86400}
                      step={1}
                      required
                      value={seconds}
                      aria-invalid={secondsError ? true : undefined}
                      aria-describedby={describedBy(`${formId}-seconds`, 'hint', secondsError)}
                      onChange={(e) => setSeconds(e.target.value)}
                    />
                  </Field>
                  {shortTrial ? <p className="notice tone-pending trial-advice">{shortTrialAdvice}</p> : null}
                  <div className="actions">
                    <button type="submit" className="btn btn-primary" disabled={mutation.pending}>
                      Put on Trial
                    </button>
                    <button type="button" className="btn btn-danger-quiet" onClick={() => setAsking('withdraw')} disabled={mutation.pending}>
                      Withdraw
                    </button>
                  </div>
                </form>
              ) : null}
              {props.mode === 'bootstrap_locked' && candidate.deny_all ? (
                <div className="actions">
                  <button type="button" className="btn btn-primary" onClick={() => setAsking('genesis')} disabled={mutation.pending}>
                    Establish as the First Policy
                  </button>
                </div>
              ) : null}
              {props.mode === 'recovery_required' && candidate.deny_all ? (
                <div className="actions">
                  <button type="button" className="btn btn-primary" onClick={() => setAsking('recovery')} disabled={mutation.pending}>
                    Establish as the Starting Policy After a Restore
                  </button>
                </div>
              ) : null}
              {props.mode !== 'normal' && !candidate.deny_all ? <p className="hint">Only a policy that denies everything can be established first.</p> : null}
            </div>
          ) : null}
          {guided ? (
            <div className="stepper-nav">
              {step > 0 ? (
                <button type="button" className="btn" onClick={() => setStep(step - 1)}>
                  Back
                </button>
              ) : null}
              {step < 2 ? (
                <button type="button" className="btn btn-primary" onClick={() => setStep(step + 1)}>
                  {step === 0 ? 'Next: Preview Impact' : 'Next: Start Trial'}
                </button>
              ) : null}
              {/* The trial step's own form offers Withdraw beside Put on trial. */}
              {props.manage && step < 2 ? (
                <button type="button" className="btn btn-danger-quiet" onClick={() => setAsking('withdraw')} disabled={mutation.pending}>
                  Withdraw
                </button>
              ) : null}
            </div>
          ) : null}
        </>
      ) : null}

      <MutationResult<Outcome>
        mutation={mutation}
        onRefresh={() => {
          detail.refresh()
          props.changed()
        }}
        success={(o) => (
          <>
            {o.provisional ? (
              <p className="notice-title">
                Version {o.provisional} is on trial and active. Keep it before {time(o.deadline)}, at the top of this page, or it is undone.
              </p>
            ) : o.revision ? (
              <p className="notice-title">Established: version {o.revision} is the active policy.</p>
            ) : sent === 'withdraw' ? (
              <p className="notice-title">Withdrawn: change {props.id} can no longer be put on trial.</p>
            ) : (
              <p className="notice-title">Done.</p>
            )}
            <Receipt outcome={o} />
          </>
        )}
      />

      <Confirm
        open={asking === 'activate'}
        title={`Put change ${props.id} on trial?`}
        confirmLabel="Put on Trial"
        disabled={confirmWord !== '' && typed.trim() !== confirmWord}
        onCancel={() => setAsking(null)}
        onConfirm={() => act('activate')}
      >
        <p>It is enforced from the next request.</p>
        {changes && changes.length > 0 ? <ChangeSet changes={changes} /> : null}
        <p>
          Unless someone keeps it within {Number.isInteger(period) && period > 0 ? duration(period) : `${seconds} seconds`}, the gateway undoes it and
          returns to the active version now
          {props.confirmed ? `, version ${props.confirmed.id}` : ''}.
        </p>
        {changes ? (
          <p className="hint">
            {changes.length === 0
              ? 'No differences from the active version.'
              : changes.length === 1
                ? 'One difference from the active version.'
                : `${changes.length} differences from the active version.`}
          </p>
        ) : null}
        {shortTrial ? <p className="notice tone-pending">{shortTrialAdvice}</p> : null}
        {confirmWord ? (
          <>
            <p className="notice tone-refused">
              {candidate?.deny_all
                ? 'This policy denies every request: Fetch stops working for everyone until it is undone.'
                : `This change removes API ${removed.join(', ')} whole: its endpoints can no longer be called.`}
            </p>
            <TypedConfirmation word={confirmWord} value={typed} onChange={setTyped} />
          </>
        ) : null}
      </Confirm>
      <Confirm open={asking === 'withdraw'} title={`Withdraw change ${props.id}?`} confirmLabel="Withdraw Change" danger onCancel={() => setAsking(null)} onConfirm={() => act('withdraw')}>
        <p>It can no longer be put on trial. The active policy does not change.</p>
      </Confirm>
      <Confirm
        open={asking === 'genesis' || asking === 'recovery'}
        title={`Establish change ${props.id}?`}
        confirmLabel="Establish"
        disabled={typed.trim() !== 'deny all'}
        onCancel={() => setAsking(null)}
        onConfirm={() => asking && act(asking)}
      >
        <p>
          This policy, which lets no request through, becomes the {asking === 'recovery' ? 'starting policy after a restore' : 'first policy'}, active
          at once. Later changes go through trials.
        </p>
        <TypedConfirmation word="deny all" value={typed} onChange={setTyped} />
      </Confirm>
    </section>
  )
}
