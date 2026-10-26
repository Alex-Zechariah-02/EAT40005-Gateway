import { useId, useState, type FormEvent } from 'react'
import { newKey, type MutationRequest, type Outcome, type Revision, type Subject } from './api'
import { useGatewayState, useMutation, usePaged, useResource } from './hooks'
import {
  Arrowed,
  Badge,
  Confirm,
  describedBy,
  Empty,
  Field,
  fieldError,
  MutationResult,
  PageHeader,
  Problem,
  ReadStamp,
  Receipt,
  ShowMore,
  TableScroll,
  Tags,
} from './ui'
import type { ViewProps } from './views'

// Subjects lists the trusted API callers and the roles each holds, and lets
// an administrator with manage_subjects add, change or remove one. Each change
// publishes a new policy revision; the receipt names it.
export function Subjects({ admin, can }: ViewProps) {
  const state = useGatewayState()
  const subjects = usePaged<Subject>(admin, '/admin/subjects', [])
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const refresh = () => {
    subjects.refresh()
    policy.refresh()
    state.refresh()
  }
  const mutation = useMutation(admin, refresh)
  const [editing, setEditing] = useState<Subject | null>(null)
  const [removing, setRemoving] = useState<string | null>(null)

  // The ETag of the subject list names the confirmed revision the list was
  // read from; a change based on an older one is refused as stale.
  const etag = subjects.reply?.kind === 'ok' ? subjects.reply.etag : null
  const confirmed = policy.reply?.kind === 'ok' ? policy.reply.body : undefined
  const document = confirmed?.document
  const roles = document?.roles.map((r) => r.id) ?? []
  const manage = can('manage_subjects')
  // operations lists what a subject's roles are permitted in the confirmed
  // policy, as that policy states it.
  const operations = (s: Subject) => [...new Set(document?.permits.filter((p) => s.roles.includes(p.role)).map((p) => p.operation))].sort()
  const status = state.reply?.kind === 'ok' ? state.reply.body : undefined
  const layered = status !== undefined && (status.provisional !== undefined || (status.overrides?.length ?? 0) > 0)

  function remove(id: string) {
    setRemoving(null)
    mutation.run({ method: 'DELETE', path: `/admin/subjects/${encodeURIComponent(id)}`, ifMatch: etag ?? undefined, key: newKey() })
  }

  return (
    <>
      <PageHeader
        title="Subjects"
        lede="API callers, identified by their token's subject, and the roles each holds. Roles come only from this list, never from a token. Which operations a role may call is part of the policy and changes only through a candidate."
      >
        {manage ? (
          <button type="button" className="btn btn-primary" onClick={() => setEditing({ id: '', status: 'active', roles: [] })} disabled={mutation.pending}>
            Add a subject
          </button>
        ) : null}
        <ReadStamp readAt={subjects.readAt} loading={subjects.loading || policy.loading} onRefresh={refresh} />
      </PageHeader>

      {editing ? (
        <SubjectForm
          key={editing.id || 'new'}
          subject={editing}
          roles={roles}
          etag={etag}
          pending={mutation.pending}
          lastReply={mutation.phase.state === 'replied' ? mutation.phase.reply : undefined}
          submit={(request) => mutation.run(request)}
          close={() => setEditing(null)}
        />
      ) : null}

      <MutationResult<Outcome>
        mutation={mutation}
        onRefresh={refresh}
        success={(o) => (
          <>
            <p className="notice-title">Membership change published: from the next request the gateway uses revision {o.revision}.</p>
            <Receipt outcome={o} />
          </>
        )}
      />

      {layered ? (
        <p className="notice tone-pending">
          {status?.provisional !== undefined ? 'A provisional policy' : 'A temporary override'} is in force, so what the gateway enforces now can differ
          from the permitted operations below, which are those of confirmed revision {confirmed?.id}.
        </p>
      ) : null}
      {subjects.reply && subjects.reply.kind !== 'ok' ? <Problem reply={subjects.reply} /> : null}
      {subjects.reply?.kind === 'ok' && subjects.items.length === 0 ? (
        <Empty title="No subjects are defined.">
          <p>Without subjects, every operation that needs a token is refused.</p>
        </Empty>
      ) : null}
      {subjects.items.length > 0 ? (
        <TableScroll label="Subjects">
          <table>
            <thead>
              <tr>
                <th scope="col">Subject</th>
                <th scope="col">Status</th>
                <th scope="col">Roles</th>
                <th scope="col">Operations permitted through its roles</th>
                {manage ? (
                  <th scope="col">
                    <span className="visually-hidden">Actions</span>
                  </th>
                ) : null}
              </tr>
            </thead>
            <tbody>
              {subjects.items.map((s) => (
                <tr key={s.id}>
                  <td>
                    <code>{s.id}</code>
                  </td>
                  <td>{s.status === 'active' ? 'active' : <Badge tone="neutral">{s.status}</Badge>}</td>
                  <td>{s.roles.length > 0 ? <Tags items={s.roles} /> : <span className="muted">none</span>}</td>
                  <td>
                    {s.status !== 'active' ? (
                      <span className="muted">none while {s.status}</span>
                    ) : operations(s).length > 0 ? (
                      <Tags items={operations(s)} code />
                    ) : (
                      <span className="muted">none</span>
                    )}
                  </td>
                  {manage ? (
                    <td className="row-actions">
                      <button type="button" className="btn btn-small" onClick={() => setEditing(s)} disabled={mutation.pending}>
                        Change<span className="visually-hidden"> {s.id}</span>
                      </button>
                      <button type="button" className="btn btn-small btn-danger-quiet" onClick={() => setRemoving(s.id)} disabled={mutation.pending}>
                        Remove<span className="visually-hidden"> {s.id}</span>
                      </button>
                    </td>
                  ) : null}
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      ) : null}
      <ShowMore list={subjects} />

      <Confirm
        open={removing !== null}
        title={`Remove subject ${removing ?? ''}?`}
        confirmLabel="Remove and publish"
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => removing && remove(removing)}
      >
        <p>
          <code>{removing}</code> will no longer be admitted to any protected operation, from the next request.
        </p>
        <p className="hint">This publishes a new confirmed revision at once, without a provisional step.</p>
      </Confirm>
    </>
  )
}

function SubjectForm(props: {
  subject: Subject
  roles: string[]
  etag: string | null
  pending: boolean
  lastReply: Parameters<typeof fieldError>[0]
  submit: (request: MutationRequest) => void
  close: () => void
}) {
  const id = useId()
  const isNew = props.subject.id === ''
  const [subjectId, setSubjectId] = useState(props.subject.id)
  const [status, setStatus] = useState(props.subject.status)
  const [roles, setRoles] = useState<string[]>(props.subject.roles)
  const [confirming, setConfirming] = useState(false)
  // The gateway compares the id with the token's subject byte for byte; it
  // accepts 1 to 128 printable characters without spaces and checks again.
  const malformed = isNew && subjectId !== '' && !/^[\x21-\x7e]{1,128}$/.test(subjectId)
  const idError = malformed ? 'Use 1 to 128 printable characters, without spaces.' : fieldError(props.lastReply, 'id')

  function request(): MutationRequest {
    return {
      method: 'PUT',
      path: `/admin/subjects/${encodeURIComponent(subjectId)}`,
      body: { status, roles },
      ifMatch: props.etag ?? undefined,
      key: newKey(),
    }
  }

  function submit(event: FormEvent) {
    event.preventDefault()
    if (malformed) return
    setConfirming(true)
  }

  return (
    <form className="panel editor" onSubmit={submit} aria-labelledby={`${id}-title`}>
      <h2 id={`${id}-title`}>{isNew ? 'Add a subject' : `Change ${props.subject.id}`}</h2>
      <div className="form-grid">
        <Field id={`${id}-subject`} label="Subject" hint="The token subject, as the issuer writes it." error={idError}>
          <input
            id={`${id}-subject`}
            className="input"
            required
            readOnly={!isNew}
            maxLength={128}
            spellCheck={false}
            autoComplete="off"
            value={subjectId}
            aria-invalid={idError ? true : undefined}
            aria-describedby={describedBy(`${id}-subject`, 'hint', idError)}
            onChange={(e) => setSubjectId(e.target.value)}
          />
        </Field>
        <Field id={`${id}-status`} label="Status">
          <select id={`${id}-status`} className="input" value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="active">active</option>
            <option value="disabled">disabled (refused)</option>
          </select>
        </Field>
      </div>
      <fieldset className="checks">
        <legend>Roles</legend>
        {props.roles.map((role) => (
          <label key={role} className="check">
            <input
              type="checkbox"
              checked={roles.includes(role)}
              onChange={(e) => setRoles(e.target.checked ? [...roles, role] : roles.filter((r) => r !== role))}
            />
            <span>{role}</span>
          </label>
        ))}
      </fieldset>
      <div className="actions">
        <button type="submit" className="btn btn-primary" disabled={props.pending || subjectId === ''}>
          Review and publish
        </button>
        <button type="button" className="btn" onClick={props.close}>
          Close
        </button>
      </div>
      <Confirm
        open={confirming}
        title={`Publish membership for ${subjectId}?`}
        confirmLabel="Publish"
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setConfirming(false)
          props.submit(request())
        }}
      >
        <dl className="facts">
          <div className="fact">
            <dt>Status</dt>
            <dd>
              <Arrowed text={isNew ? status : props.subject.status === status ? `${status} (unchanged)` : `${props.subject.status} → ${status}`} />
            </dd>
          </div>
          <div className="fact">
            <dt>Roles</dt>
            <dd>
              <Arrowed text={`${isNew ? '' : `${props.subject.roles.join(', ') || 'none'} → `}${roles.join(', ') || 'none'}`} />
            </dd>
          </div>
        </dl>
        <p className="hint">This publishes a new confirmed revision at once, without a provisional step. The gateway enforces it from the next request.</p>
      </Confirm>
    </form>
  )
}
