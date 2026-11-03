import { useEffect, useId, useState, type FormEvent } from 'react'
import { newKey } from './http'
import type { Candidate, Outcome, Revision } from './api'
import { useGatewayState, useMutation, usePaged, useRereadWhen, useResource } from './hooks'
import { ActivationHistory } from './activation'
import { TrialDecision } from './provisional'
import { Badge, describedBy, Field, ListToolbar, matches, TableScroll } from './ui'
import { Confirm } from './dialogs'
import { ReadStamp, Time } from './time'
import { MutationResult, Problem, Receipt, ShowMore } from './replies'
import { PageHeader } from './header'
import { useFocus } from './links'
import type { ViewProps } from './views'
import { CandidateStatus, statusWords } from './draft'
import { CandidateDetail } from './draft-detail'

// The Changes page: the change on trial, saved draft changes, saving a new one, and trial history.

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
