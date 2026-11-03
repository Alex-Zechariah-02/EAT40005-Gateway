import { useEffect, useState } from 'react'
import type { Operation, OverridesView, Page, Revision } from '../shared/api'
import { usePaged, useResource } from '../shared/hooks'
import type { RequestEvent } from '../monitor/requests'
import { Badge, Empty, TableScroll } from '../shared/ui'
import { PageHeader } from '../app/header'
import { Problem, ShowMore } from '../shared/replies'
import { ReadStamp, Time } from '../shared/time'
import { useFocus } from '../shared/links'
import type { ViewProps } from '../app/views'
import { RolesSection } from './roles'
import { origins, PolicyTables, RevisionDetail } from './revision'

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
export const recentRequests = 100

// limitWords is an endpoint's rate limit in words, such as "60 per minute
// per account"; the same words always mean the same limit.
export function limitWords(operation: Operation): string {
  if (!operation.rate) return 'No rate limit'
  const { count, window_seconds: seconds, key } = operation.rate
  const span = seconds === 1 ? 'second' : seconds === 60 ? 'minute' : seconds === 3600 ? 'hour' : `${seconds} s`
  return `${count} per ${span} per ${key === 'client_address' ? 'client address' : 'account'}`
}

// sharedLimit is the limit more than half of a group's endpoints have, said
// once in the group's heading; each endpoint with another limit shows its own.
export function sharedLimit(operations: Operation[]): string | undefined {
  const counts = new Map<string, number>()
  for (const operation of operations) counts.set(limitWords(operation), (counts.get(limitWords(operation)) ?? 0) + 1)
  const [words, count] = [...counts].sort((a, b) => b[1] - a[1])[0] ?? []
  return words && count && count * 2 > operations.length ? words : undefined
}

// The tab of the endpoints anyone may call; no role can have this ID.
export const publicTab = ' public'
