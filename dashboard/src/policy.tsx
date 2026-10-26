import { useEffect, useRef, useState } from 'react'
import type { Operation, Policy, Revision } from './api'
import { differences } from './diff'
import { usePaged, useResource } from './hooks'
import { Badge, ChangeList, CodeBlock, Empty, Glyph, PageHeader, Problem, ReadStamp, ReplyView, ShowMore, TableScroll, Time } from './ui'
import type { ViewProps } from './views'

// PolicyView shows the managed APIs, their upstreams and operations, the
// request rules the gateway checks, and the roles with their permitted
// operations, from the confirmed policy. Earlier revisions can be inspected
// and proposed again. Changes to any of them are proposed as a complete
// candidate and take effect only through confirmed activation.
export function PolicyView({ admin, can, open }: ViewProps) {
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const revisions = usePaged<Revision>(admin, '/admin/revisions', [])
  const [inspected, setInspected] = useState<number | null>(null)
  const refresh = () => {
    policy.refresh()
    revisions.refresh()
  }
  const revision = policy.reply?.kind === 'ok' ? policy.reply.body : undefined
  const none = policy.reply?.kind === 'refused' && policy.reply.error === 'no_confirmed_policy'

  return (
    <>
      <PageHeader
        title="Policy revisions"
        lede={
          revision ? (
            <>
              Revision {revision.id}, {origins[revision.origin] ?? revision.origin}, by {revision.actor} at <Time value={revision.created_at} />.
              Profile <code>{revision.document?.profile}</code>.
            </>
          ) : (
            'The confirmed policy as tables, and every revision ever published.'
          )
        }
      >
        {revision && can('manage_policy') ? (
          <button type="button" className="btn btn-small" onClick={() => open('candidates', { draft: JSON.stringify(revision.document, null, 2) })}>
            Start a change from this
          </button>
        ) : null}
        <ReadStamp readAt={policy.readAt} loading={policy.loading || revisions.loading} onRefresh={refresh} />
      </PageHeader>
      {none ? (
        <Empty title="No confirmed policy is in force.">
          <p>A new or restored installation admits nothing until an administrator establishes a policy that denies everything.</p>
          <button type="button" className="btn" onClick={() => open('candidates')}>
            Go to Changes
          </button>
        </Empty>
      ) : null}
      {policy.reply && policy.reply.kind !== 'ok' && !none ? <Problem reply={policy.reply} /> : null}
      {revision?.document ? <PolicyTables policy={revision.document} /> : null}

      <section className="section" aria-labelledby="revisions-title">
        <h2 id="revisions-title">Policy revisions</h2>
        <p className="section-lede">Every revision ever published, oldest first. Inspect one to compare it with the confirmed policy.</p>
        {revisions.reply && revisions.reply.kind !== 'ok' ? <Problem reply={revisions.reply} /> : null}
        <TableScroll label="Policy revisions">
          <table>
            <thead>
              <tr>
                <th scope="col">Revision</th>
                <th scope="col">Parent</th>
                <th scope="col">Origin</th>
                <th scope="col">By</th>
                <th scope="col">Created</th>
                <th scope="col">Checksum</th>
                <th scope="col">
                  <span className="visually-hidden">Inspect</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {revisions.items.map((r) => (
                <tr key={r.id} aria-current={inspected === r.id ? 'true' : undefined}>
                  <td className="num">
                    {r.id} {r.id === revision?.id ? <Badge tone="ok">confirmed</Badge> : null}
                  </td>
                  <td className="num">{r.parent ?? ''}</td>
                  <td>{origins[r.origin] ?? r.origin}</td>
                  <td>{r.actor}</td>
                  <td>
                    <Time value={r.created_at} />
                  </td>
                  <td>
                    <code className="muted" title={r.checksum}>
                      {r.checksum.slice(0, 16)}…
                    </code>
                  </td>
                  <td className="row-actions">
                    <button type="button" className="btn btn-small" onClick={() => setInspected(r.id)} aria-pressed={inspected === r.id}>
                      Inspect<span className="visually-hidden"> revision {r.id}</span>
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
        <ShowMore list={revisions} />
      </section>
      {inspected !== null ? (
        <RevisionDetail
          key={inspected}
          admin={admin}
          id={inspected}
          confirmed={revision}
          propose={can('manage_policy') ? (document) => open('candidates', { draft: JSON.stringify(document, null, 2) }) : undefined}
          close={() => setInspected(null)}
        />
      ) : null}
    </>
  )
}

// How each revision came to be, as the controller records it.
const origins: Record<string, string> = {
  genesis: 'first policy (genesis)',
  recovery_base: 'recovery base',
  membership: 'subject membership change',
  publish: 'activated candidate',
}

function RevisionDetail(props: {
  admin: ViewProps['admin']
  id: number
  confirmed?: Revision
  propose?: (document: Policy) => void
  close: () => void
}) {
  const heading = useRef<HTMLHeadingElement>(null)
  const detail = useResource<Revision>((signal) => props.admin.get(`/admin/revisions/${props.id}`, signal), [props.id])
  const isConfirmed = props.confirmed?.id === props.id

  // Opening a revision moves focus to it.
  useEffect(() => {
    heading.current?.focus()
  }, [])

  return (
    <section className="section panel detail" aria-labelledby="revision-title">
      <div className="detail-head">
        <h2 id="revision-title" ref={heading} tabIndex={-1}>
          Revision {props.id}
        </h2>
        {isConfirmed ? <Badge tone="ok">confirmed</Badge> : null}
      </div>
      {detail.loading ? <p className="hint">Reading from the gateway…</p> : null}
      <ReplyView reply={detail.reply}>
        {(r) => (
          <>
            <dl className="facts">
              <div className="fact">
                <dt>Origin</dt>
                <dd>
                  {origins[r.origin] ?? r.origin} <code className="muted">{r.origin}</code>
                </dd>
              </div>
              <div className="fact">
                <dt>Parent revision</dt>
                <dd>{r.parent ?? 'none'}</dd>
              </div>
              <div className="fact">
                <dt>Created</dt>
                <dd>
                  <Time value={r.created_at} /> by {r.actor}
                </dd>
              </div>
              <div className="fact">
                <dt>Checksum (SHA-256)</dt>
                <dd>
                  <code className="hash">{r.checksum}</code>
                </dd>
              </div>
            </dl>
            {isConfirmed ? (
              <p className="hint">This is the confirmed policy shown above.</p>
            ) : props.confirmed?.document && r.document ? (
              <>
                <h3>Changes from the confirmed policy (revision {props.confirmed.id}) to this revision</h3>
                <p className="hint">Computed by this page for review.</p>
                {(() => {
                  const changes = differences(props.confirmed.document, r.document)
                  return changes.length === 0 ? <p>No differences.</p> : <ChangeList changes={changes} />
                })()}
              </>
            ) : null}
            <details className="document">
              <summary>Full document</summary>
              <CodeBlock label={`Revision ${r.id} document`}>{JSON.stringify(r.document, null, 2)}</CodeBlock>
            </details>
            <div className="actions">
              {props.propose && r.document && !isConfirmed ? (
                <button type="button" className="btn btn-primary" onClick={() => r.document && props.propose?.(r.document)}>
                  Start a change from this revision
                </button>
              ) : null}
              <button type="button" className="btn" onClick={props.close}>
                Close
              </button>
            </div>
            {props.propose && !isConfirmed ? (
              <p className="hint">
                Opens the document in Candidates. Nothing changes until it is staged, activated and confirmed there.
              </p>
            ) : null}
          </>
        )}
      </ReplyView>
    </section>
  )
}

function PolicyTables({ policy: p }: { policy: Policy }) {
  const checked = p.operations.filter((o) => o.body || o.headers)
  return (
    <>
      <section className="section" aria-labelledby="operations-title">
        <h2 id="operations-title">Operations</h2>
        <p className="section-lede">
          Each operation is one method and path the gateway admits. Anything else is refused by default.
        </p>
        <TableScroll label="Operations">
          <table>
            <thead>
              <tr>
                <th scope="col">Operation</th>
                <th scope="col">Method and path, API</th>
                <th scope="col">Access</th>
                <th scope="col">Token</th>
                <th scope="col">Request checks</th>
                <th scope="col">Rate limit</th>
              </tr>
            </thead>
            <tbody>
              {p.operations.map((o) => (
                <tr key={o.id}>
                  <td>
                    <code>{o.id}</code>
                  </td>
                  <td>
                    <code className="route">
                      <span className="method">{o.method}</span> {o.target}
                    </code>
                    <span className="cell-sub">{o.api} API</span>
                  </td>
                  {/* The usual values are plain; only an exception is marked. */}
                  <td>{o.access === 'allow' ? 'allow' : <Badge tone="refused">{o.access}</Badge>}</td>
                  <td>{o.auth === 'none' ? <Badge tone="neutral">public</Badge> : o.auth}</td>
                  <td>{checksText(o)}</td>
                  <td className="num">{o.rate ? `${o.rate.count} per ${o.rate.window_seconds} s per ${o.rate.key === 'client_address' ? 'client address' : 'account'}` : 'none'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      </section>

      <PermitMatrix policy={p} />

      {checked.length > 0 ? (
        <section className="section" aria-labelledby="rules-title">
          <h2 id="rules-title">Request rules</h2>
          <p className="section-lede">
            What the gateway checks in a request before forwarding it. An operation without a body rule accepts no body.
          </p>
          <div className="rules">
            {checked.map((o) => (
              <RequestRules key={o.id} operation={o} />
            ))}
          </div>
        </section>
      ) : null}

      <section className="section">
        <h2>Upstreams</h2>
        <p className="section-lede">Each API's requests go only to its upstream, a fixed loopback address.</p>
        <TableScroll label="Upstreams">
          <table>
            <thead>
              <tr>
                <th scope="col">Upstream</th>
                <th scope="col">Address</th>
                <th scope="col">APIs</th>
              </tr>
            </thead>
            <tbody>
              {p.upstreams.map((u) => (
                <tr key={u.id}>
                  <td>{u.id}</td>
                  <td>
                    <code>{u.dial}</code>
                  </td>
                  <td>
                    {p.apis
                      .filter((a) => a.upstream === u.id)
                      .map((a) => a.id)
                      .join(', ')}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      </section>
    </>
  )
}

// PermitMatrix shows which roles hold a permit for which operation, one row
// per operation and one column per role, with the subjects holding each role.
// A public operation needs neither a token nor a role.
function PermitMatrix({ policy: p }: { policy: Policy }) {
  const permitted = (role: string, operation: string) => p.permits.some((permit) => permit.role === role && permit.operation === operation)
  return (
    <section className="section" aria-labelledby="permits-title">
      <h2 id="permits-title">Role permits</h2>
      <p className="section-lede">
        {p.subjects
          ? 'A subject may call an operation only when one of its roles holds a permit for it. Roles come from this policy, never from a token.'
          : 'An account may call an operation only when a role from its sign-in token holds a permit for it. The gateway ignores roles this policy does not define.'}
      </p>
      <TableScroll label="Role permits">
        <table className="matrix">
          <thead>
            <tr>
              <th scope="col">Operation</th>
              {p.roles.map((r) => {
                if (!p.subjects) {
                  return (
                    <th scope="col" key={r.id}>
                      {r.id}
                    </th>
                  )
                }
                const holders = p.subjects.filter((s) => s.roles.includes(r.id))
                const active = holders.filter((s) => s.status === 'active').length
                return (
                  <th scope="col" key={r.id}>
                    {r.id}
                    <span className="matrix-holders">
                      {holders.length === 0
                        ? 'no subjects'
                        : `${holders.length} ${holders.length === 1 ? 'subject' : 'subjects'}${active < holders.length ? `, ${holders.length - active} disabled` : ''}`}
                    </span>
                  </th>
                )
              })}
            </tr>
          </thead>
          <tbody>
            {p.operations.map((o) => (
              <tr key={o.id}>
                <th scope="row">
                  <code>{o.id}</code>
                </th>
                {o.access !== 'allow' ? (
                  <td className="matrix-cell matrix-public" colSpan={p.roles.length}>
                    closed to everyone by its access flag
                  </td>
                ) : o.auth === 'none' ? (
                  <td className="matrix-cell matrix-public" colSpan={p.roles.length}>
                    public: no token or role needed
                  </td>
                ) : (
                  p.roles.map((r) =>
                    permitted(r.id, o.id) ? (
                      <td className="matrix-cell" key={r.id}>
                        <span className="matrix-yes">
                          <Glyph name="ok" />
                        </span>
                        <span className="visually-hidden">permitted</span>
                      </td>
                    ) : (
                      <td className="matrix-cell" key={r.id}>
                        <span className="matrix-no" aria-hidden="true">
                          –
                        </span>
                        <span className="visually-hidden">no permit</span>
                      </td>
                    ),
                  )
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </TableScroll>
    </section>
  )
}

function checksText(o: Operation): string {
  const checks = []
  if (o.body) checks.push(`JSON body, at most ${o.body.max_bytes} bytes`)
  if (o.headers) checks.push('headers')
  return checks.length > 0 ? checks.join('; ') : 'no body accepted'
}

type Schema = Record<string, unknown>

interface Rule {
  field: string
  place: 'body' | 'header'
  type: string
  required: boolean
  limits: string
}

function isSchema(value: unknown): value is Schema {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

// limitsText states a schema node's constraints in words. The gateway accepts
// only these keywords, so nothing in a node goes unstated.
function limitsText(node: Schema): string {
  const limits: string[] = []
  if (Array.isArray(node.enum)) limits.push(`one of ${node.enum.map((v) => JSON.stringify(v)).join(', ')}`)
  const range = (min: unknown, max: unknown, unit: string) => {
    if (min !== undefined && max !== undefined) limits.push(`${min} to ${max}${unit}`)
    else if (min !== undefined) limits.push(`at least ${min}${unit}`)
    else if (max !== undefined) limits.push(`at most ${max}${unit}`)
  }
  range(node.minLength, node.maxLength, ' characters')
  range(node.minimum, node.maximum, '')
  if (node.type === 'object' && node.additionalProperties === false) limits.push('no other members')
  return limits.join('; ')
}

// rules lists the members a schema declares, following nested objects.
function rules(schema: unknown, place: Rule['place'], prefix = ''): Rule[] {
  if (!isSchema(schema) || !isSchema(schema.properties)) return []
  const required = Array.isArray(schema.required) ? schema.required : []
  const list: Rule[] = []
  for (const [name, node] of Object.entries(schema.properties)) {
    if (!isSchema(node)) continue
    const field = `${prefix}${name}`
    list.push({
      field,
      place,
      type: Array.isArray(node.type) ? node.type.join(' or ') : String(node.type ?? 'any'),
      required: required.includes(name),
      limits: limitsText(node),
    })
    list.push(...rules(node, place, `${field}.`))
  }
  return list
}

function RequestRules({ operation: o }: { operation: Operation }) {
  const body = o.body ? (o.body.schema as Schema) : undefined
  const headers = o.headers as Schema | undefined
  const list = [...rules(body, 'body'), ...rules(headers, 'header')]
  const closed = (schema?: Schema) => (schema?.additionalProperties === false ? 'refused' : 'accepted')
  return (
    <div className="rule-block">
      <h3>
        <code>{o.id}</code>{' '}
        <code className="route">
          <span className="method">{o.method}</span> {o.target}
        </code>
      </h3>
      <p className="hint">
        {body ? `JSON body (application/json), at most ${o.body!.max_bytes} bytes, no repeated members; members not listed are ${closed(body)}.` : 'No body accepted.'}
        {headers ? ' Each listed header may appear once; headers not listed are not checked by this rule.' : ''}
      </p>
      <TableScroll label={`Request rules of ${o.id}`}>
        <table>
          <thead>
            <tr>
              <th scope="col">Field</th>
              <th scope="col">In</th>
              <th scope="col">Type</th>
              <th scope="col">Required</th>
              <th scope="col">Constraints</th>
            </tr>
          </thead>
          <tbody>
            {list.map((r) => (
              <tr key={`${r.place}:${r.field}`}>
                <td>
                  <code>{r.field}</code>
                </td>
                <td>{r.place}</td>
                <td>
                  <code>{r.type}</code>
                </td>
                <td>{r.required ? 'required' : 'optional'}</td>
                <td>{r.limits || <span className="muted">none</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </TableScroll>
    </div>
  )
}
