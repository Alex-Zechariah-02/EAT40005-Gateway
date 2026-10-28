import { useEffect, useId, useRef, useState, type FormEvent } from 'react'
import { newKey, type Candidate, type Outcome, type Revision, type Status } from './api'
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
  // when no confirmed policy is in force (a new or restored installation).
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
        lede="A draft change is a complete proposed policy. The gateway checks it when it is saved; you can preview its effect on recent traffic; it then goes live on trial and is undone by itself unless someone keeps it."
      >
        <ReadStamp okAt={list.last?.at ?? null} failed={list.failed} loading={list.loading || policy.loading} onRefresh={refresh} />
      </PageHeader>

      <TrialDecision admin={admin} manage={manage} variant="full" />

      <Lifecycle status={status} />

      <section className="section card" aria-labelledby="staged-title">
        <div className="card-head card-head-pad">
          <h2 id="staged-title">Draft changes</h2>
          {manage ? (
            <a className="card-link" href="#stage-title">
              New draft change
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
                  Draft one in the <a href="#/designer">Policy</a> designer, or paste a document below.
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
            <TableScroll label="Draft changes">
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
                        <CandidateStatus status={c.status} /> {c.active ? <Badge tone="ok">in force</Badge> : null}
                      </td>
                      <td>{c.parent ? `version ${c.parent}` : 'no version in force'}</td>
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

const steps = [
  { name: 'Draft', text: 'Edit the policy in the Policy designer, or load a JSON document.' },
  { name: 'Checked', text: 'Saving a draft runs the gateway’s own compiler over the whole document. Nothing changes yet.' },
  { name: 'Impact preview', text: 'Replays recent requests to show who would be refused or allowed differently.' },
  { name: 'On trial', text: 'In force from the next request, with a deadline. Undo it at any time.' },
  { name: 'Kept', text: 'Stays in force for good. Unless it is kept, the gateway undoes it by itself at the deadline.' },
]

// Lifecycle shows how a change goes live, with the step the gateway reports
// now marked when a change is on trial.
function Lifecycle({ status }: { status?: Status }) {
  const waiting = status?.provisional !== undefined
  return (
    <section className="section" aria-label="How a change goes live">
      <ol className="lifecycle">
        {steps.map((step, n) => (
          <li key={step.name} className={waiting && n === 3 ? 'lifecycle-step is-current' : 'lifecycle-step'} aria-current={waiting && n === 3 ? 'step' : undefined}>
            <span className="lifecycle-number" aria-hidden="true">
              {n + 1}
            </span>
            <span className="lifecycle-name">{step.name}</span>
            <span className="lifecycle-text">{step.text}</span>
          </li>
        ))}
      </ol>
    </section>
  )
}

const statusWords: Record<string, string> = {
  validated: 'Checked',
  established: 'Put in force',
  quarantined: 'Set aside after a restore',
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
        <h2 id="stage-title">Save a draft change</h2>
        {props.confirmed?.document ? (
          <button type="button" className="btn btn-small" onClick={() => (text.trim() === '' ? startFromConfirmed() : setReplacing(true))}>
            Start from version {props.confirmed.id}
          </button>
        ) : null}
      </div>
      <form onSubmit={submit}>
        <Field id={`${id}-file`} label="Load a policy document from a file (optional)">
          <input id={`${id}-file`} className="file" type="file" accept="application/json,.json" onChange={(e) => load(e.target.files?.[0])} />
        </Field>
        <Field
          id={`${id}-text`}
          label="Complete policy document (JSON)"
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
            {mutation.pending ? 'Saving…' : 'Save draft and check'}
          </button>
        </div>
      </form>
      <Confirm
        open={replacing}
        title="Replace the document being edited?"
        confirmLabel="Replace it"
        onCancel={() => setReplacing(false)}
        onConfirm={startFromConfirmed}
      >
        <p>The text in the editor is replaced by version {props.confirmed?.id}, the version in force. Nothing is sent to the gateway.</p>
      </Confirm>
      <MutationResult<{ receipt: Outcome; candidate: Candidate }>
        mutation={mutation}
        onRefresh={props.changed}
        success={(body) => (
          <>
            <p className="notice-title">The gateway checked it: saved as draft change {body.candidate.id}. Nothing is in force yet.</p>
            <div className="actions">
              <button type="button" className="btn" onClick={() => props.review(body.candidate.id)}>
                Review change {body.candidate.id}
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
  'Under 2 minutes leaves little time for real requests to arrive, to check how they were decided and to decide. Unless someone keeps it in time, the gateway undoes it by itself.'

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
  return (
    <section className="section card card-pad detail" aria-labelledby={`${formId}-title`}>
      <div className="detail-head">
        <h2 id={`${formId}-title`} ref={heading} tabIndex={-1}>
          Change {props.id}
        </h2>
        {candidate ? <CandidateStatus status={candidate.status} /> : null}
        {candidate?.active ? <Badge tone="ok">in force</Badge> : null}
      </div>
      {detail.loading ? <p className="hint">Reading from the gateway…</p> : null}
      {detail.reply && detail.reply.kind !== 'ok' ? <Problem reply={detail.reply} /> : null}
      {candidate ? (
        <>
          <dl className="facts facts-inline">
            <div className="fact">
              <dt>Based on</dt>
              <dd>{candidate.parent ? `version ${candidate.parent}` : 'no version in force'}</dd>
            </div>
            <div className="fact">
              <dt>Denies everything</dt>
              <dd>{candidate.deny_all ? 'yes' : 'no'}</dd>
            </div>
            <div className="fact">
              <dt>Saved</dt>
              <dd>
                <Time value={candidate.created_at} /> by {candidate.actor}
              </dd>
            </div>
            <div className="fact">
              <dt>Exact form</dt>
              <dd>
                <MoreDetails facts={[['Checksum (SHA-256)', candidate.checksum]]} />
              </dd>
            </div>
          </dl>

          <h3>Differences from the version in force</h3>
          {changes ? (
            changes.length === 0 ? (
              <p className="hint">No differences.</p>
            ) : (
              <ChangeList changes={changes} />
            )
          ) : (
            <p className="hint">No version is in force to compare with.</p>
          )}

          <details className="document">
            <summary>Full document</summary>
            <CodeBlock label={`Change ${candidate.id} document`}>{JSON.stringify(candidate.document, null, 2)}</CodeBlock>
          </details>

          {usable && props.mode === 'normal' ? <ImpactPreview admin={props.admin} candidate={candidate.id} /> : null}

          {props.manage && usable ? (
            <div className="detail-actions">
              {props.mode === 'normal' ? (
                <form
                  onSubmit={(e) => {
                    e.preventDefault()
                    setAsking('activate')
                  }}
                >
                  <h3>Put on trial</h3>
                  <Field
                    id={`${formId}-seconds`}
                    label="Trial length (seconds)"
                    hint="1 to 86400. Unless someone keeps the change within this time, the gateway undoes it and returns to the version in force now by itself."
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
                      Put on trial
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
                    Establish as the first policy
                  </button>
                </div>
              ) : null}
              {props.mode === 'recovery_required' && candidate.deny_all ? (
                <div className="actions">
                  <button type="button" className="btn btn-primary" onClick={() => setAsking('recovery')} disabled={mutation.pending}>
                    Establish as the starting policy after a restore
                  </button>
                </div>
              ) : null}
              {props.mode !== 'normal' && !candidate.deny_all ? <p className="hint">Only a policy that denies everything can be established first.</p> : null}
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
                Version {o.provisional} is on trial and in force. Keep it before {time(o.deadline)}, at the top of this page, or it is undone.
              </p>
            ) : o.revision ? (
              <p className="notice-title">Established: version {o.revision} is the policy in force.</p>
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
        confirmLabel="Put on trial"
        disabled={confirmWord !== '' && typed.trim() !== confirmWord}
        onCancel={() => setAsking(null)}
        onConfirm={() => act('activate')}
      >
        <p>It is enforced from the next request.</p>
        {changes && changes.length > 0 ? <ChangeSet changes={changes} /> : null}
        <p>
          Unless someone keeps it within {Number.isInteger(period) && period > 0 ? duration(period) : `${seconds} seconds`}, the gateway undoes it and
          returns to the version in force now
          {props.confirmed ? `, version ${props.confirmed.id}` : ''}.
        </p>
        {changes ? (
          <p className="hint">
            {changes.length === 0
              ? 'No differences from the version in force.'
              : changes.length === 1
                ? 'One difference from the version in force.'
                : `${changes.length} differences from the version in force.`}
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
      <Confirm open={asking === 'withdraw'} title={`Withdraw change ${props.id}?`} confirmLabel="Withdraw change" danger onCancel={() => setAsking(null)} onConfirm={() => act('withdraw')}>
        <p>It can no longer be put on trial. The policy in force does not change.</p>
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
          This policy, which lets no request through, becomes the {asking === 'recovery' ? 'starting policy after a restore' : 'first policy'}, in
          force at once. Later changes go through trials.
        </p>
        <TypedConfirmation word="deny all" value={typed} onChange={setTyped} />
      </Confirm>
    </section>
  )
}
