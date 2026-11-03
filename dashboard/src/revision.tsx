import { useEffect, useRef } from 'react'
import type { Policy, Revision } from './api'
import { differences } from './diff'
import { useResource } from './hooks'
import { Badge, CodeBlock, Glyph, TableScroll } from './ui'
import { ChangeList } from './review'
import { ReplyView } from './replies'
import { Time } from './time'
import { XRef } from './links'
import type { ViewProps } from './views'
import { checksText, RequestRules } from './request-rules'

// One policy version in detail: its origin, its tables and the permission matrix.

// How each revision came to be, as the controller records it.
export const origins: Record<string, string> = {
  genesis: 'first policy',
  recovery_base: 'starting policy after a restore',
  membership: 'account membership change',
  publish: 'kept from a trial',
}

export function RevisionDetail(props: {
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
          Version {props.id}
        </h2>
        {isConfirmed ? <Badge tone="ok">Active</Badge> : null}
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
                <dt>Previous Version</dt>
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
              <p className="hint">This is the active version, shown above.</p>
            ) : props.confirmed?.document && r.document ? (
              <>
                <h3>Changes from the Active Version (Version {props.confirmed.id}) to this version</h3>
                <p className="hint">Computed by this page for review.</p>
                {(() => {
                  const changes = differences(props.confirmed.document, r.document)
                  return changes.length === 0 ? <p>No differences.</p> : <ChangeList changes={changes} />
                })()}
              </>
            ) : null}
            <details className="document">
              <summary>Full Document</summary>
              <CodeBlock label={`Version ${r.id} document`}>{JSON.stringify(r.document, null, 2)}</CodeBlock>
            </details>
            <div className="actions">
              {props.propose && r.document && !isConfirmed ? (
                <button type="button" className="btn btn-primary" onClick={() => r.document && props.propose?.(r.document)}>
                  Start a Draft Change from This Version
                </button>
              ) : null}
              <button type="button" className="btn" onClick={props.close}>
                Close
              </button>
            </div>
            {props.propose && !isConfirmed ? (
              <p className="hint">
                Opens in Changes. Nothing changes until it is saved, put on trial and kept.
              </p>
            ) : null}
          </>
        )}
      </ReplyView>
    </section>
  )
}

export function PolicyTables({ policy: p, part }: { policy: Policy; part: 'endpoints' | 'addresses' }) {
  const checked = p.operations.filter((o) => o.body || o.headers)
  if (part === 'endpoints')
    return (
      <>
        <section className="section" aria-labelledby="versions-endpoints">
          <h2 id="versions-endpoints" className="visually-hidden">
            Endpoints
          </h2>
          <p className="section-lede">
            Methods and paths the gateway accepts. All other requests are refused.
          </p>
          <TableScroll label="Endpoints">
            <table>
              <thead>
                <tr>
                  <th scope="col">Endpoint</th>
                  <th scope="col">Method and Path, API</th>
                  <th scope="col">Access</th>
                  <th scope="col">Token</th>
                  <th scope="col">Request Checks</th>
                  <th scope="col">Rate Limit</th>
                </tr>
              </thead>
              <tbody>
                {p.operations.map((o) => (
                  <tr key={o.id}>
                    <td>
                      <XRef view="policy" focus={{ select: `operation:${o.id}` }}>
                        <code>{o.id}</code>
                      </XRef>
                    </td>
                    <td>
                      <code className="route">
                        <span className="method">{o.method}</span> {o.target}
                      </code>
                      <span className="cell-sub">{o.api} API</span>
                    </td>
                    {/* The usual values are plain; only an exception is marked. */}
                    <td>{o.access === 'allow' ? 'allow' : <Badge tone="refused">{o.access}</Badge>}</td>
                    <td>{o.auth === 'none' ? <Badge tone="neutral">Public</Badge> : o.auth}</td>
                    <td>{checksText(o)}</td>
                    <td className="num">{o.rate ? `${o.rate.count} per ${o.rate.window_seconds} s per ${o.rate.key === 'client_address' ? 'client address' : 'account'}` : 'none'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </TableScroll>
        </section>

        {checked.length > 0 ? (
          <section className="section" aria-labelledby="rules-title">
            <h3 id="rules-title">Request Rules</h3>
            <p className="section-lede">
              Checked before forwarding. An endpoint without a body rule accepts no body.
            </p>
            <div className="rules">
              {checked.map((o) => (
                <RequestRules key={o.id} operation={o} />
              ))}
            </div>
          </section>
        ) : null}
      </>
    )
  return (
    <>
      <section className="section" aria-labelledby="versions-addresses">
        <h2 id="versions-addresses" className="visually-hidden">
          Service Addresses
        </h2>
        <p className="section-lede">Each API forwards only to its service address.</p>
        <TableScroll label="Service Addresses">
          <table>
            <thead>
              <tr>
                <th scope="col">Service Address</th>
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
// per operation and one column per role. A public operation needs neither a
// token nor a role.
export function PermitMatrix({ policy: p }: { policy: Policy }) {
  const permitted = (role: string, operation: string) => p.permits.some((permit) => permit.role === role && permit.operation === operation)
  return (
    <section className="section" aria-labelledby="permits-title">
      <h3 id="permits-title">Role Permissions</h3>
      <p className="section-lede">
        An account may call an endpoint only if a role in its sign-in token has permission. Roles not defined here are ignored.
      </p>
      <TableScroll label="Role Permissions">
        <table className="matrix">
          <thead>
            <tr>
              <th scope="col">Endpoint</th>
              {p.roles.map((r) => (
                <th scope="col" key={r.id}>
                  {r.id}
                </th>
              ))}
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
                        <span className="visually-hidden">no permission</span>
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
