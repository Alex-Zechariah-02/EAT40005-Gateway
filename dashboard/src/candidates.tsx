import { useEffect, useId, useRef, useState, type FormEvent } from 'react'
import { newKey, type Candidate, type Outcome, type Revision } from './api'
import { differences, removedApis } from './diff'
import { useGatewayState, useMutation, usePaged, useRereadWhen, useResource } from './hooks'
import { ImpactPreview } from './monitor'
import { ActivationHistory, TrialDecision } from './provisional'
import {
  Badge,
  ChangeList,
  ChangeSet,
  CodeBlock,
  Confirm,
  describedBy,
  duration,
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
  ShowMore,
  TableScroll,
  time,
  Time,
  TypedConfirmation,
  useFocus,
} from './ui'
import type { ViewProps } from './views'

const maxCandidateBytes = 80 * 1024

// Changes is the guided way a policy change goes live: drafted, checked
// by the gateway's own compiler when staged, previewed against recent
// traffic, put on trial with a rollback deadline, then confirmed or rolled
// back. A staged change (a "candidate" to the gateway) is never in force by
// itself.
export function Candidates({ admin, can, draft, candidate }: ViewProps & { draft?: string; candidate?: number }) {
  const state = useGatewayState()
  const list = usePaged<Candidate>(admin, '/admin/candidates', [])
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const [selected, setSelected] = useState<number | null>(candidate ?? null)
  const [trials, setTrials] = useState(0)
  const [query, setQuery] = useState('')
  useEffect(() => {
    if (candidate !== undefined) setSelected(candidate)
  }, [candidate])
  // A link from another page, such as the history, opens one draft change.
  const linked = useFocus('candidate')
  useEffect(() => {
    if (linked && /^\d+$/.test(linked)) setSelected(Number(linked))
  }, [linked])
  // The list, the confirmed policy and the past trials are read again
  // whenever the gateway's status changes (a trial confirmed, rolled back or
  // timed out, here or elsewhere) and whenever this page asks for a reread.
  useRereadWhen(state.version, () => {
    list.refresh()
    policy.refresh()
    setTrials((n) => n + 1)
  })
  const refresh = () => state.refresh()

  // A candidate names the confirmed revision it was prepared against; "r0"
  // when no confirmed policy is active (a new or restored installation).
  const ifMatch =
    policy.reply?.kind === 'ok'
      ? (policy.reply.etag ?? undefined)
      : policy.reply?.kind === 'refused' && policy.reply.error === 'no_confirmed_policy'
        ? '"r0"'
        : undefined
  const confirmed = policy.reply?.kind === 'ok' ? policy.reply.body : undefined
  const status = state.facts
  const manage = can('manage_policy')
  const shown = list.items.filter((c) =>
    matches(
      query,
      c.id,
      `change ${c.id}`,
      statusWords[c.status] ?? c.status,
      c.actor,
      c.parent ? `version ${c.parent}` : undefined,
      c.deny_all ? 'denies everything' : undefined,
    ),
  )

  return (
    <>
      <PageHeader
        title="Changes"
        lede="Review draft changes, preview their impact and activate one through a timed trial."
      >
        <ReadStamp okAt={list.last?.at ?? null} failed={list.failed} loading={list.loading || policy.loading} onRefresh={refresh} />
      </PageHeader>

      <TrialDecision admin={admin} manage={manage} variant="full" />

      <section className="section card" aria-labelledby="staged-title">
        <div className="card-head card-head-pad">
          <h2 id="staged-title">Draft Changes</h2>
          {manage ? (
            <a className="card-link" href="#stage-title">
              New Draft Change
            </a>
          ) : null}
        </div>
        <div className="card-body">
          {list.reply && list.reply.kind !== 'ok' ? <Problem reply={list.reply} /> : null}
          {list.reply?.kind === 'ok' && list.items.length === 0 ? (
            <p className="hint card-empty">
              No draft changes yet.{' '}
              {manage ? (
                <>
                  Draft one on the <a href="#/policy">Policy</a> page, or paste a document below.
                </>
              ) : null}
            </p>
          ) : null}
          {list.items.length > 0 ? (
            <ListToolbar
              label="Filter draft changes"
              query={query}
              onQuery={setQuery}
              shown={shown.length}
              total={list.items.length}
              more={list.next !== undefined}
              noun={['draft change', 'draft changes']}
            />
          ) : null}
          {list.items.length > 0 && shown.length === 0 ? <p className="hint card-empty">No loaded draft change matches the filter.</p> : null}
          {shown.length > 0 ? (
            <TableScroll label="Draft Changes">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Change</th>
                    <th scope="col">State</th>
                    <th scope="col">Based on</th>
                    <th scope="col">Saved</th>
                    <th scope="col">
                      <span className="visually-hidden">Review</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {shown.map((c) => (
                    <tr key={c.id} aria-current={selected === c.id ? 'true' : undefined}>
                      <td className="num">
                        <strong>{c.id}</strong>
                        {c.deny_all ? <span className="cell-sub">denies everything</span> : null}
                      </td>
                      <td>
                        <CandidateStatus status={c.status} /> {c.active ? <Badge tone="ok">Active</Badge> : null}
                      </td>
                      <td>{c.parent ? `version ${c.parent}` : 'no active version'}</td>
                      <td>
                        <Time value={c.created_at} /> <span className="muted">by {c.actor}</span>
                      </td>
                      <td className="row-actions">
                        <button type="button" className="btn btn-small" onClick={() => setSelected(c.id)} aria-pressed={selected === c.id}>
                          Review
                          <span className="visually-hidden"> change {c.id}</span>
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </TableScroll>
          ) : null}
          <div className="card-pad-x">
            <ShowMore list={list} />
          </div>
        </div>
      </section>

      {selected !== null ? (
        <CandidateDetail key={selected} id={selected} admin={admin} confirmed={confirmed} ifMatch={ifMatch} mode={status?.state} manage={manage} changed={refresh} />
      ) : null}

      {manage ? <StageForm admin={admin} ifMatch={ifMatch} draft={draft} confirmed={confirmed} changed={refresh} review={setSelected} /> : null}

      {can('read_history') ? <ActivationHistory admin={admin} reads={trials} /> : null}
    </>
  )
}

const stepNames = ['Review changes', 'Preview impact', 'Start trial']

// Stepper shows where a change is in its path to a trial. A finished step
// can be opened again; the next one opens with Next.
function Stepper({ step, onStep }: { step: number; onStep: (step: number) => void }) {
  return (
    <ol className="stepper" aria-label="Steps">
      {stepNames.map((name, n) => (
        <li key={name} className={n === step ? 'is-current' : n < step ? 'is-done' : undefined}>
          <button type="button" aria-current={n === step ? 'step' : undefined} disabled={n > step} onClick={() => onStep(n)}>
            <span className="stepper-number" aria-hidden="true">
              {n + 1}
            </span>
            {name}
          </button>
        </li>
      ))}
    </ol>
  )
}

const statusWords: Record<string, string> = {
  validated: 'Checked',
  established: 'Activated',
  quarantined: 'Set Aside After a Restore',
  withdrawn: 'Withdrawn',
}

export function CandidateStatus({ status }: { status: string }) {
  const words = statusWords[status] ?? status
  if (status === 'validated')
    return (
      <Badge tone="neutral" glyph="ok">
        {words}
      </Badge>
    )
  return <Badge tone={status === 'established' ? 'ok' : status === 'quarantined' ? 'refused' : 'neutral'}>{words}</Badge>
}

function StageForm(props: {
  admin: ViewProps['admin']
  ifMatch?: string
  draft?: string
  confirmed?: Revision
  changed: () => void
  review: (id: number) => void
}) {
  const id = useId()
  const [text, setText] = useState(props.draft ?? '')
  const [replacing, setReplacing] = useState(false)
  const mutation = useMutation<{ receipt: Outcome; candidate: Candidate }>(props.admin, props.changed)
  const bytes = new TextEncoder().encode(text).length
  let localError: string | undefined
  if (text.trim() !== '') {
    try {
      JSON.parse(text)
    } catch {
      localError = 'This is not valid JSON.'
    }
  }
  if (bytes > maxCandidateBytes) localError = `The document is ${bytes} bytes; the gateway accepts at most ${maxCandidateBytes}.`

  async function load(file: File | undefined) {
    if (file) setText(await file.text())
  }

  // startFromConfirmed puts the confirmed document in the editor, to be
  // changed and staged as a whole: a change is always a complete policy.
  function startFromConfirmed() {
    setReplacing(false)
    if (props.confirmed?.document) setText(JSON.stringify(props.confirmed.document, null, 2))
  }

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (localError || text.trim() === '') return
    // The document is sent as written, so the gateway's strict decoder sees
    // exactly what the administrator reviewed (repeated members included).
    await mutation.run({ method: 'POST', path: '/admin/candidates', rawBody: `{"policy":${text}}`, ifMatch: props.ifMatch, key: newKey() })
  }

  return (
    <section className="section card card-pad" aria-labelledby="stage-title">
      <div className="card-head">
        <h2 id="stage-title">Save a Draft Change</h2>
        {props.confirmed?.document ? (
          <button type="button" className="btn btn-small" onClick={() => (text.trim() === '' ? startFromConfirmed() : setReplacing(true))}>
            Start from version {props.confirmed.id}
          </button>
        ) : null}
      </div>
      <form onSubmit={submit}>
        <Field id={`${id}-file`} label="Load a Policy Document from a File (Optional)">
          <input id={`${id}-file`} className="file" type="file" accept="application/json,.json" onChange={(e) => load(e.target.files?.[0])} />
        </Field>
        <Field
          id={`${id}-text`}
          label="Complete Policy Document (JSON)"
          hint={`Checked by the gateway with the same compiler that builds the enforced policy. ${bytes.toLocaleString()} of ${maxCandidateBytes.toLocaleString()} bytes.`}
          error={localError}
        >
          <textarea
            id={`${id}-text`}
            className="input code-input"
            rows={8}
            spellCheck={false}
            autoComplete="off"
            value={text}
            aria-invalid={localError ? true : undefined}
            aria-describedby={describedBy(`${id}-text`, 'hint', localError)}
            onChange={(e) => setText(e.target.value)}
          />
        </Field>
        <div className="actions">
          <button type="submit" className="btn btn-primary" disabled={mutation.pending || props.ifMatch === undefined || text.trim() === ''}>
            {mutation.pending ? 'Saving…' : 'Save Draft and Check'}
          </button>
        </div>
      </form>
      <Confirm
        open={replacing}
        title="Replace the document being edited?"
        confirmLabel="Replace It"
        onCancel={() => setReplacing(false)}
        onConfirm={startFromConfirmed}
      >
        <p>The text in the editor is replaced by version {props.confirmed?.id}, the active version. Nothing is sent to the gateway.</p>
      </Confirm>
      <MutationResult<{ receipt: Outcome; candidate: Candidate }>
        mutation={mutation}
        onRefresh={props.changed}
        success={(body) => (
          <>
            <p className="notice-title">The gateway checked it: saved as draft change {body.candidate.id}. Nothing is active yet.</p>
            <div className="actions">
              <button type="button" className="btn" onClick={() => props.review(body.candidate.id)}>
                Review Change {body.candidate.id}
              </button>
            </div>
            <Receipt outcome={body.receipt} />
          </>
        )}
      />
    </section>
  )
}

type Action = 'activate' | 'withdraw' | 'genesis' | 'recovery'

// A trial shorter than this leaves too little time for real requests to
// arrive, for a few rereads of the trial's traffic and for a deliberate
// decision. It is advice only: the gateway accepts 1 to 86400 seconds.
const adviseTrialSeconds = 120
const shortTrialAdvice =
  'Under 2 minutes leaves little time to observe real traffic. If not kept by the deadline, the change is undone automatically.'

function CandidateDetail(props: {
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
            <>
          <dl className="facts facts-inline">
            <div className="fact">
              <dt>Based on</dt>
              <dd>{candidate.parent ? `version ${candidate.parent}` : 'no active version'}</dd>
            </div>
            <div className="fact">
              <dt>Denies Everything</dt>
              <dd>{candidate.deny_all ? 'yes' : 'no'}</dd>
            </div>
            <div className="fact">
              <dt>Saved</dt>
              <dd>
                <Time value={candidate.created_at} /> by {candidate.actor}
              </dd>
            </div>
            <div className="fact">
              <dt>Exact Form</dt>
              <dd>
                <MoreDetails facts={[['Checksum (SHA-256)', candidate.checksum]]} />
              </dd>
            </div>
          </dl>

          <h3>Differences from the Active Version</h3>
          {changes ? (
            changes.length === 0 ? (
              <p className="hint">No differences.</p>
            ) : (
              <ChangeList changes={changes} />
            )
          ) : (
            <p className="hint">No version is active to compare with.</p>
          )}

          <details className="document">
            <summary>Full Document</summary>
            <CodeBlock label={`Change ${candidate.id} document`}>{JSON.stringify(candidate.document, null, 2)}</CodeBlock>
          </details>
            </>
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
