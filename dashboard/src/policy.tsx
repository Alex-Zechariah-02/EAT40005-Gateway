import { useEffect, useRef, useState } from 'react'
import type { Operation, Override, OverridesView, Page, Policy, Revision } from './api'
import { capital } from './designer-model'
import { differences } from './diff'
import { usePaged, useResource } from './hooks'
import type { RequestEvent } from './monitor'
import { overrideChange } from './overrides'
import { Badge, ChangeList, CodeBlock, Empty, Glyph, ListToolbar, matches, PageHeader, Problem, ReadStamp, ReplyView, ShowMore, TableScroll, time, Time, useFocus, XRef } from './ui'
import type { ViewProps } from './views'

// PolicyView is the Versions page: the active policy in detail (what each
// role may call, the endpoints and their request rules, the service
// addresses) and every version put in force, each of which can be inspected
// and proposed again. Changes take effect only through a draft change, a
// trial and keeping it.
export function PolicyView({ admin, can, open }: ViewProps) {
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const revisions = usePaged<Revision>(admin, '/admin/revisions', [])
  const rules = useResource<OverridesView>((signal) => admin.get('/admin/overrides', signal), [])
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get(`/admin/events?limit=${recentRequests}`, signal), [])
  const [inspected, setInspected] = useState<number | null>(null)
  const refresh = () => {
    policy.refresh()
    revisions.refresh()
    rules.refresh()
    recent.refresh()
  }
  const revision = policy.reply?.kind === 'ok' ? policy.reply.body : undefined
  const none = policy.reply?.kind === 'refused' && policy.reply.error === 'no_confirmed_policy'
  const [part, setPart] = useState<Part>('roles')
  // A link from another page opens one version's detail or one role's tab.
  const linkedVersion = useFocus('version')
  const linkedRole = useFocus('role')
  useEffect(() => {
    if (linkedVersion && /^\d+$/.test(linkedVersion)) {
      setPart('history')
      setInspected(Number(linkedVersion))
    }
  }, [linkedVersion])
  useEffect(() => {
    if (linkedRole) setPart('roles')
  }, [linkedRole])
  const history = (
    <>
      <section className="section" aria-labelledby="versions-history">
        <h2 id="versions-history" className={revision?.document ? 'visually-hidden' : undefined}>
          Version History
        </h2>
        <p className="section-lede">Every version made active, oldest first.</p>
        {revisions.reply && revisions.reply.kind !== 'ok' ? <Problem reply={revisions.reply} /> : null}
        <TableScroll label="Policy versions">
          <table>
            <thead>
              <tr>
                <th scope="col">Version</th>
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
                    {r.id} {r.id === revision?.id ? <Badge tone="ok">Active</Badge> : null}
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
                      Inspect<span className="visually-hidden"> version {r.id}</span>
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
          propose={can('manage_policy') ? (document) => open('changes', { draft: JSON.stringify(document, null, 2) }) : undefined}
          close={() => setInspected(null)}
        />
      ) : null}
    </>
  )

  return (
    <>
      <PageHeader title="Versions" lede="The active policy in detail, and every version made active.">
        {revision && can('manage_policy') ? (
          <button type="button" className="btn btn-small" onClick={() => open('changes', { draft: JSON.stringify(revision.document, null, 2) })}>
            Start a Change from This
          </button>
        ) : null}
        <ReadStamp
          okAt={policy.last?.at ?? null}
          failed={policy.failed}
          loading={policy.loading || revisions.loading || rules.loading || recent.loading}
          onRefresh={refresh}
        />
      </PageHeader>
      {none ? (
        <Empty title="No policy is active yet.">
          <p>A new or restored installation refuses every request until an administrator establishes a policy that denies everything.</p>
          <button type="button" className="btn" onClick={() => open('changes')}>
            Go to Changes
          </button>
        </Empty>
      ) : null}
      {policy.reply && policy.reply.kind !== 'ok' && !none ? <Problem reply={policy.reply} /> : null}
      {rules.reply && rules.reply.kind !== 'ok' ? <Problem reply={rules.reply} /> : null}
      {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
      {revision?.document ? (
        <>
          <p className="hint version-line">
            Version {revision.id}, {origins[revision.origin] ?? revision.origin}, by {revision.actor} at <Time value={revision.created_at} />
            {rules.reply?.kind === 'ok' && rules.reply.body.provisional === revision.id
              ? ', on trial: it is undone at its deadline unless someone keeps it'
              : ''}
            . Profile <code>{revision.document.profile}</code>.
          </p>
          <div className="section-tabs" role="tablist" aria-label="Versions sections">
            {parts.map(([id, label], index) => (
              <button
                key={id}
                id={`versions-tab-${id}`}
                type="button"
                role="tab"
                aria-selected={part === id}
                aria-controls="versions-part"
                tabIndex={part === id ? 0 : -1}
                onClick={() => setPart(id)}
                onKeyDown={(event) => {
                  const step = event.key === 'ArrowRight' ? 1 : event.key === 'ArrowLeft' ? -1 : 0
                  let next = step ? (index + step + parts.length) % parts.length : -1
                  if (event.key === 'Home') next = 0
                  if (event.key === 'End') next = parts.length - 1
                  if (next < 0) return
                  event.preventDefault()
                  setPart(parts[next][0])
                  document.getElementById(`versions-tab-${parts[next][0]}`)?.focus()
                }}
              >
                {label}
              </button>
            ))}
          </div>
          <div id="versions-part" role="tabpanel" aria-labelledby={`versions-tab-${part}`}>
            {part === 'roles' ? (
              <RolesSection
                linked={linkedRole}
                policy={revision.document}
                active={rules.reply?.kind === 'ok' ? rules.reply.body.overrides : []}
                recent={recent.reply?.kind === 'ok' ? recent.reply.body.items : []}
                canChange={can('manage_policy')}
              />
            ) : part === 'endpoints' || part === 'addresses' ? (
              <PolicyTables policy={revision.document} part={part} />
            ) : (
              history
            )}
          </div>
        </>
      ) : (
        history
      )}
    </>
  )
}

// The parts of the Versions page, one tab each.
type Part = 'roles' | 'endpoints' | 'addresses' | 'history'
const parts: [Part, string][] = [
  ['roles', 'Roles'],
  ['endpoints', 'Endpoints'],
  ['addresses', 'Service Addresses'],
  ['history', 'Version History'],
]

// The latest recorded requests read for the accounts seen with each role:
// one page of the gateway's recent events.
const recentRequests = 100

// limitWords is an endpoint's rate limit in words, such as "60 per minute
// per account"; the same words always mean the same limit.
function limitWords(operation: Operation): string {
  if (!operation.rate) return 'No rate limit'
  const { count, window_seconds: seconds, key } = operation.rate
  const span = seconds === 1 ? 'second' : seconds === 60 ? 'minute' : seconds === 3600 ? 'hour' : `${seconds} s`
  return `${count} per ${span} per ${key === 'client_address' ? 'client address' : 'account'}`
}

// sharedLimit is the limit more than half of a group's endpoints have, said
// once in the group's heading; each endpoint with another limit shows its own.
function sharedLimit(operations: Operation[]): string | undefined {
  const counts = new Map<string, number>()
  for (const operation of operations) counts.set(limitWords(operation), (counts.get(limitWords(operation)) ?? 0) + 1)
  const [words, count] = [...counts].sort((a, b) => b[1] - a[1])[0] ?? []
  return words && count && count * 2 > operations.length ? words : undefined
}

// The tab of the endpoints anyone may call; no role can have this ID.
const publicTab = ' public'

// RolesSection shows what one role may call under the active policy, chosen
// from tabs: its endpoints grouped by API in one table, the rate limit said
// once per group, and the temporary rules that change it now listed beside
// it, never merged in. A role comes only from a verified sign-in token.
function RolesSection(props: { policy: Policy; active: Override[]; recent: RequestEvent[]; canChange: boolean; linked: string | null }) {
  const { policy: doc, active } = props
  const [chosen, setChosen] = useState(props.linked ?? doc.roles[0]?.id ?? publicTab)
  useEffect(() => {
    if (props.linked) setChosen(props.linked)
  }, [props.linked])
  const [query, setQuery] = useState('')
  const seen = new Map<string, string[]>()
  for (const event of props.recent) {
    if (!event.subject) continue
    for (const role of event.roles) {
      const subjects = seen.get(role) ?? []
      if (!subjects.includes(event.subject)) seen.set(role, [...subjects, event.subject])
    }
  }
  const allowedFor = (role: string) =>
    role === publicTab
      ? doc.operations.filter((operation) => operation.auth === 'none')
      : doc.operations.filter((operation) => doc.permits.some((permit) => permit.role === role && permit.operation === operation.id))
  const tabs: [string, string][] = [...doc.roles.map((role): [string, string] => [role.id, role.id]), [publicTab, 'Public']]
  const role = tabs.some(([id]) => id === chosen) ? chosen : tabs[0][0]
  const allowed = allowedFor(role)
  const shown = allowed.filter((operation) => matches(query, operation.id, operation.api, `${operation.method} ${operation.target}`))
  const groups = [...new Set(shown.map((operation) => operation.api))].map((api) => ({
    api,
    operations: shown.filter((operation) => operation.api === api),
  }))
  const temporary =
    role === publicTab
      ? []
      : active.filter((rule) => rule.role === role || (rule.role === undefined && allowed.some((operation) => operation.id === rule.operation)))
  const accounts = seen.get(role) ?? []
  const who = role === publicTab ? 'anyone' : role
  return (
    <section className="section" aria-labelledby="versions-roles">
      <h2 id="versions-roles" className="visually-hidden">
        Roles
      </h2>
      <p className="section-lede">Endpoints each role may call under the active policy, grouped by API.</p>
      <div className="role-tabs" role="tablist" aria-label="Roles">
        {tabs.map(([id, label], index) => (
          <button
            key={id}
            id={`role-tab-${index}`}
            type="button"
            role="tab"
            aria-selected={id === role}
            aria-controls="role-view"
            tabIndex={id === role ? 0 : -1}
            onClick={() => setChosen(id)}
            onKeyDown={(event) => {
              const step = event.key === 'ArrowRight' ? 1 : event.key === 'ArrowLeft' ? -1 : 0
              let next = step ? (index + step + tabs.length) % tabs.length : -1
              if (event.key === 'Home') next = 0
              if (event.key === 'End') next = tabs.length - 1
              if (next < 0) return
              event.preventDefault()
              setChosen(tabs[next][0])
              document.getElementById(`role-tab-${next}`)?.focus()
            }}
          >
            <span className={id === publicTab ? undefined : 'role-tab-name'}>{label}</span>{' '}
            <span className="role-tab-count">{allowedFor(id).length}</span>
          </button>
        ))}
      </div>
      <div id="role-view" className="role-view" role="tabpanel" aria-labelledby={`role-tab-${tabs.findIndex(([id]) => id === role)}`}>
        <div className="role-summary">
          <p className="role-summary-lead">
            {role === publicTab ? (
              <>
                Anyone may call {allowed.length === 1 ? 'this endpoint' : `these ${allowed.length} endpoints`} without signing in.
              </>
            ) : allowed.length === 0 ? (
              <>
                <code>{role}</code> has no permissions.
              </>
            ) : (
              <>
                <code>{role}</code> can call {allowed.length} {allowed.length === 1 ? 'endpoint' : 'endpoints'}.
              </>
            )}
          </p>
          {role === publicTab ? (
            <p className="hint">Their rate limits count requests per client address.</p>
          ) : (
            <p className="hint">
              {accounts.length > 0 ? (
                <>
                  Accounts seen with this role in the latest {recentRequests} requests:{' '}
                  {accounts.map((subject, index) => (
                    <span key={subject}>
                      {index > 0 ? ', ' : ''}
                      <code>{subject}</code>
                    </span>
                  ))}
                </>
              ) : (
                `No account with this role in the latest ${recentRequests} requests.`
              )}
            </p>
          )}
        </div>
        {temporary.length > 0 ? (
          <div className="notice tone-pending role-temporary">
            <p className="notice-title">Temporary Rules Changing This Role Now</p>
            <ul>
              {temporary.map((rule) => (
                <li key={rule.id}>
                  Rule {rule.id}: {overrideChange(rule, doc)}, until {time(rule.expires_at)}
                </li>
              ))}
            </ul>
          </div>
        ) : null}
        {allowed.length > 0 ? (
          <ListToolbar
            label="Filter endpoints"
            query={query}
            onQuery={setQuery}
            shown={shown.length}
            total={allowed.length}
            noun={['endpoint', 'endpoints']}
          >
            {props.canChange ? (
              <a className="btn" href="#/policy">
                Change Permissions
              </a>
            ) : null}
          </ListToolbar>
        ) : null}
        {allowed.length > 0 && shown.length === 0 ? <p className="hint">No endpoint matches the filter.</p> : null}
        {groups.length > 0 ? (
          <TableScroll label={`Endpoints ${who} may call`}>
            <table className="role-table">
              <thead>
                <tr>
                  <th scope="col">Endpoint</th>
                  <th scope="col">ID</th>
                  <th scope="col">Rate Limit</th>
                </tr>
              </thead>
              {groups.map((group) => {
                const common = sharedLimit(group.operations)
                const allShare = group.operations.every((operation) => limitWords(operation) === common)
                return (
                  <tbody key={group.api}>
                    <tr className="role-group">
                      <th scope="colgroup" colSpan={3}>
                        <span className="role-group-name">{group.api ? `${capital(group.api)} API` : 'No API'}</span>
                        <span className="role-group-meta">
                          {group.operations.length} {group.operations.length === 1 ? 'endpoint' : 'endpoints'}
                          {common ? ` · ${common}${allShare ? '' : ', unless shown'}` : ''}
                        </span>
                      </th>
                    </tr>
                    {group.operations.map((operation) => {
                      const limit = limitWords(operation)
                      return (
                        <tr key={operation.id}>
                          <td>
                            <code>
                              <span className="method">{operation.method}</span> {operation.target}
                            </code>
                            {operation.access === 'deny' ? (
                              <>
                                {' '}
                                <Badge tone="refused">Access: Deny</Badge>
                              </>
                            ) : null}
                          </td>
                          <td>
                            <XRef view="policy" focus={{ select: `operation:${operation.id}` }}>
                              <code>{operation.id}</code>
                            </XRef>
                          </td>
                          <td>{limit === common ? <span className="visually-hidden">{limit}</span> : limit}</td>
                        </tr>
                      )
                    })}
                  </tbody>
                )
              })}
            </table>
          </TableScroll>
        ) : null}
      </div>
      <PermitMatrix policy={doc} />
    </section>
  )
}

// How each revision came to be, as the controller records it.
const origins: Record<string, string> = {
  genesis: 'first policy',
  recovery_base: 'starting policy after a restore',
  membership: 'account membership change',
  publish: 'kept from a trial',
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

function PolicyTables({ policy: p, part }: { policy: Policy; part: 'endpoints' | 'addresses' }) {
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
function PermitMatrix({ policy: p }: { policy: Policy }) {
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
  // 'each' marks the rule every element of an array must meet.
  required: boolean | 'each'
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
  range(node.minItems, node.maxItems, ' items')
  if (hasType(node, 'object') && node.additionalProperties === false) limits.push('no other members')
  return limits.join('; ')
}

function typeText(node: Schema): string {
  return Array.isArray(node.type) ? node.type.join(' or ') : String(node.type ?? 'any')
}

// hasType tells whether a schema node allows a type, named alone or in a list.
function hasType(node: Schema, type: string): boolean {
  return node.type === type || (Array.isArray(node.type) && node.type.includes(type))
}

// rules lists the members a schema declares, following nested objects and
// the elements of arrays (written field[]).
function rules(schema: unknown, place: Rule['place'], prefix = ''): Rule[] {
  if (!isSchema(schema) || !isSchema(schema.properties)) return []
  const required = Array.isArray(schema.required) ? schema.required : []
  const list: Rule[] = []
  for (const [name, node] of Object.entries(schema.properties)) {
    if (!isSchema(node)) continue
    const field = `${prefix}${name}`
    list.push({ field, place, type: typeText(node), required: required.includes(name), limits: limitsText(node) })
    list.push(...within(node, place, field))
  }
  return list
}

// within lists what a member's own schema declares: its object members and,
// when it may be an array, its elements, which may be arrays in turn.
function within(node: Schema, place: Rule['place'], field: string): Rule[] {
  const list = rules(node, place, `${field}.`)
  if (hasType(node, 'array') && isSchema(node.items)) {
    const element = `${field}[]`
    list.push({ field: element, place, type: typeText(node.items), required: 'each', limits: limitsText(node.items) })
    list.push(...within(node.items, place, element))
  }
  return list
}

// RequestRules states an endpoint's body and header rules in words. It
// describes the policy only: the gateway's compiler and request check stay
// the only validator.
export function RequestRules({ operation: o }: { operation: Operation }) {
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
                <td>{r.required === 'each' ? 'every element' : r.required ? 'required' : 'optional'}</td>
                <td>{r.limits || <span className="muted">none</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </TableScroll>
    </div>
  )
}
