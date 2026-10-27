import { Fragment, useEffect, useId, useRef, useState } from 'react'
import { explain, type HistoryEntry } from './api'
import { usePaged, useResource } from './hooks'
import { Badge, CodeBlock, Empty, PageHeader, Problem, ReadStamp, ReplyView, ShowMore, TableScroll, Time } from './ui'
import type { ViewProps } from './views'

type Area = 'policy' | 'overrides' | 'accounts' | 'keys' | 'administrators' | 'operator'

// What each recorded kind of transition is, in words, and the area it
// belongs to. A kind not listed is shown by its code.
const kinds: Record<string, { label: string; area: Area }> = {
  genesis: { label: 'First policy established', area: 'policy' },
  recovery_base: { label: 'Recovery base established', area: 'policy' },
  'candidate.create': { label: 'Candidate staged', area: 'policy' },
  'candidate.withdraw': { label: 'Candidate withdrawn', area: 'policy' },
  'cpa.activate': { label: 'Provisional activation', area: 'policy' },
  'cpa.confirm': { label: 'Provisional policy confirmed', area: 'policy' },
  'cpa.rollback': { label: 'Provisional policy rolled back', area: 'policy' },
  'cpa.timeout': { label: 'Deadline passed, rolled back', area: 'policy' },
  publish: { label: 'Policy published', area: 'policy' },
  'tpo.create': { label: 'Override created', area: 'overrides' },
  'tpo.revoke': { label: 'Override revoked', area: 'overrides' },
  'tpo.expire': { label: 'Override expired', area: 'overrides' },
  'account.block': { label: 'Account blocked', area: 'accounts' },
  'account.unblock': { label: 'Account unblocked', area: 'accounts' },
  'keys.reload': { label: 'Signing keys reloaded', area: 'keys' },
  'admin.enrol': { label: 'Administrator enrolled', area: 'administrators' },
  'admin.password': { label: 'Password changed', area: 'administrators' },
  'operator.install': { label: 'Installed by the operator', area: 'operator' },
  'operator.enrol': { label: 'Enrolment issued by the operator', area: 'operator' },
  'operator.restore': { label: 'Restored from a backup by the operator', area: 'operator' },
  'operator.migrate': { label: 'Database migrated by the operator', area: 'operator' },
}

const areas: { id: Area | 'all'; label: string }[] = [
  { id: 'all', label: 'All areas' },
  { id: 'policy', label: 'Policy lifecycle' },
  { id: 'overrides', label: 'Temporary overrides' },
  { id: 'accounts', label: 'Blocked accounts' },
  { id: 'keys', label: 'Signing keys' },
  { id: 'administrators', label: 'Administrators' },
  { id: 'operator', label: 'Operator' },
]

// The most pages "Load all" reads at once: 2000 entries at 100 a page.
const allPages = 20

// History lists every recorded administrative transition as the controller
// stored it: who asked for what, against which revision, and the outcome.
// Request bodies, passwords and tokens are never recorded. The filters work
// on the entries read so far.
export function History({ admin }: ViewProps) {
  const id = useId()
  const history = usePaged<HistoryEntry>(admin, '/admin/history?limit=100', [])
  const [selected, setSelected] = useState<string | null>(null)
  const [area, setArea] = useState<Area | 'all'>('all')
  const [outcome, setOutcome] = useState('all')
  const [actor, setActor] = useState('all')
  const [newestFirst, setNewestFirst] = useState(false)

  const actors = [...new Set(history.items.map((h) => h.actor))].sort()
  const shown = history.items.filter(
    (h) => (area === 'all' || kinds[h.kind]?.area === area) && (outcome === 'all' || h.outcome === outcome) && (actor === 'all' || h.actor === actor),
  )
  if (newestFirst) shown.reverse()
  const filtered = shown.length !== history.items.length

  return (
    <>
      <PageHeader
        title="History"
        lede="Every administrative transition the controller recorded, with its outcome. Request bodies, passwords and tokens are never recorded."
      >
        <ReadStamp readAt={history.readAt} loading={history.loading} onRefresh={history.refresh} />
      </PageHeader>
      <section className="section">
        {history.reply && history.reply.kind !== 'ok' ? <Problem reply={history.reply} /> : null}
        {history.reply?.kind === 'ok' && history.items.length === 0 ? <Empty title="No transition has been recorded yet." /> : null}
        {history.items.length > 0 ? (
          <>
            <div className="filters" role="group" aria-label="Filter the history">
              <div className="field">
                <label htmlFor={`${id}-area`}>Area</label>
                <select id={`${id}-area`} className="input" value={area} onChange={(e) => setArea(e.target.value as Area | 'all')}>
                  {areas.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.label}
                    </option>
                  ))}
                </select>
              </div>
              <div className="field">
                <label htmlFor={`${id}-outcome`}>Outcome</label>
                <select id={`${id}-outcome`} className="input" value={outcome} onChange={(e) => setOutcome(e.target.value)}>
                  <option value="all">All outcomes</option>
                  <option value="committed">committed</option>
                  <option value="rejected">rejected</option>
                </select>
              </div>
              <div className="field">
                <label htmlFor={`${id}-actor`}>By</label>
                <select id={`${id}-actor`} className="input" value={actor} onChange={(e) => setActor(e.target.value)}>
                  <option value="all">Anyone</option>
                  {actors.map((a) => (
                    <option key={a} value={a}>
                      {a}
                    </option>
                  ))}
                </select>
              </div>
              <label className="check">
                <input type="checkbox" checked={newestFirst} onChange={(e) => setNewestFirst(e.target.checked)} />
                <span>Newest first</span>
              </label>
            </div>
            <p className="hint list-count" role="status">
              {filtered ? `Showing ${shown.length} of the ${history.items.length} entries read` : `Showing ${history.items.length} entries`}
              {history.next ? '. The gateway lists the oldest first and more exist, so the newest are not read yet.' : ', the whole history.'}
            </p>
          </>
        ) : null}
        {history.items.length > 0 && shown.length === 0 ? <Empty title="No entry read so far matches these filters." /> : null}
        {shown.length > 0 ? (
          <TableScroll label="Administrative history">
            <table>
              <thead>
                <tr>
                  <th scope="col">#</th>
                  <th scope="col">What</th>
                  <th scope="col">By</th>
                  <th scope="col">Target</th>
                  <th scope="col">Outcome</th>
                  <th scope="col">Revision</th>
                  <th scope="col">Recorded</th>
                  <th scope="col">
                    <span className="visually-hidden">Details</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {shown.map((h) => (
                  <Fragment key={h.seq}>
                    <tr aria-current={selected === h.transition_id ? 'true' : undefined}>
                      <td className="num">{h.seq}</td>
                      <td>
                        <span className="cell-main">{kinds[h.kind]?.label ?? h.kind}</span>
                        <code className="cell-sub">{h.kind}</code>
                      </td>
                      <td>{h.actor}</td>
                      <td>{h.target}</td>
                      <td>
                        {h.outcome === 'committed' ? 'committed' : <Badge tone="refused">{h.outcome}</Badge>}
                        {h.reason ? (
                          <>
                            {' '}
                            <code title={explain(h.reason) || undefined}>{h.reason}</code>
                          </>
                        ) : null}
                      </td>
                      <td className="num">{h.revision ?? ''}</td>
                      <td>
                        <Time value={h.recorded_at} />
                      </td>
                      <td className="row-actions">
                        <button
                          type="button"
                          className="btn btn-small"
                          onClick={() => setSelected(selected === h.transition_id ? null : h.transition_id)}
                          aria-expanded={selected === h.transition_id}
                        >
                          Details<span className="visually-hidden"> of entry {h.seq}</span>
                        </button>
                      </td>
                    </tr>
                    {/* The record opens under its own row, not below the
                        whole list. */}
                    {selected === h.transition_id ? (
                      <tr className="row-detail">
                        <td colSpan={8}>
                          <Entry admin={admin} id={h.transition_id} />
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </TableScroll>
        ) : null}
        <div className="actions">
          <ShowMore list={history} />
          {history.next ? (
            <button type="button" className="btn" onClick={() => history.rest(allPages)} disabled={history.paging}>
              Load all
            </button>
          ) : null}
        </div>
      </section>
    </>
  )
}

const entryFields: [keyof HistoryEntry, string][] = [
  ['seq', 'Sequence'],
  ['transition_id', 'Transition'],
  ['kind', 'Kind'],
  ['actor', 'By'],
  ['target', 'Target'],
  ['expected_revision', 'Expected revision'],
  ['outcome', 'Outcome'],
  ['reason', 'Reason'],
  ['revision', 'Resulting revision'],
  ['candidate', 'Candidate'],
  ['recorded_at', 'Recorded'],
  ['published_at', 'Published'],
  ['published_by', 'Published by'],
  ['audited_at', 'Audited'],
]

function Entry({ admin, id }: { admin: ViewProps['admin']; id: string }) {
  const heading = useRef<HTMLHeadingElement>(null)
  const entry = useResource<HistoryEntry>((signal) => admin.get(`/admin/history/${encodeURIComponent(id)}`, signal), [id])
  useEffect(() => {
    heading.current?.focus()
  }, [])
  return (
    <section className="entry-detail" aria-labelledby="entry-title">
      <h2 id="entry-title" ref={heading} tabIndex={-1}>
        Transition <code>{id}</code>
      </h2>
      <ReplyView reply={entry.reply}>
        {(h) => (
          <>
            <p className="entry-what">{kinds[h.kind]?.label ?? h.kind}</p>
            {h.reason && explain(h.reason) ? <p className="hint">{explain(h.reason)}</p> : null}
            <dl className="facts">
              {entryFields
                .filter(([key]) => h[key] !== undefined && h[key] !== '')
                .map(([key, label]) => (
                  <div key={key} className="fact">
                    <dt>{label}</dt>
                    <dd>{key.endsWith('_at') ? <Time value={String(h[key])} /> : <code>{String(h[key])}</code>}</dd>
                  </div>
                ))}
            </dl>
            <details className="document">
              <summary>Record as stored</summary>
              <CodeBlock label={`Entry ${h.seq} as stored`}>{JSON.stringify(h, null, 2)}</CodeBlock>
            </details>
          </>
        )}
      </ReplyView>
    </section>
  )
}
