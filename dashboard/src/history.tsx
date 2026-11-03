import { Fragment, useEffect, useId, useRef, useState } from 'react'
import { explain, type HistoryEntry } from './api'
import { usePaged, useRemembered, useResource } from './hooks'
import { actorName, Badge, CodeBlock, Empty, PageHeader, Problem, ReadStamp, ReplyView, ShowMore, TableScroll, Time, XRef } from './ui'
import type { ViewProps } from './views'

// targetWords names a history target in the Dashboard's words, in title
// case; the exact target stays beside it. An ID, such as an account, is
// shown as it is.
const targetNames: Record<string, string> = { candidate: 'Draft Change', revision: 'Version', override: 'Temporary Rule' }
function targetWords(target: string): string {
  const match = /^(candidate|revision|override)(?: (\d+))?$/.exec(target)
  if (match) return match[2] ? `${targetNames[match[1]]} ${match[2]}` : targetNames[match[1]]
  if (/^[a-z]+( [a-z]+)*( \d+)?$/.test(target)) return target.replace(/\b[a-z]/g, (letter) => letter.toUpperCase())
  return target
}

// TargetLink names a target and, when it is a numbered version, draft change
// or temporary rule, links to it on its own page.
function TargetLink({ target }: { target: string }) {
  const match = /^(candidate|revision|override) (\d+)$/.exec(target)
  const words = targetWords(target)
  if (!match) return <>{words}</>
  const id = Number(match[2])
  if (match[1] === 'revision') return <XRef view="versions" focus={{ version: id }}>{words}</XRef>
  if (match[1] === 'candidate') return <XRef view="changes" focus={{ candidate: id }}>{words}</XRef>
  return <XRef view="temporary-rules" focus={{ rule: id }}>{words}</XRef>
}

type Area = 'policy' | 'overrides' | 'accounts' | 'keys' | 'administrators' | 'operator'

// What each recorded kind of transition is, in words, and the area it
// belongs to. A kind not listed is shown by its code.
const kinds: Record<string, { label: string; area: Area }> = {
  genesis: { label: 'First Policy Established', area: 'policy' },
  recovery_base: { label: 'Starting Policy After a Restore Established', area: 'policy' },
  'candidate.create': { label: 'Draft Change Saved', area: 'policy' },
  'candidate.withdraw': { label: 'Draft Change Withdrawn', area: 'policy' },
  'cpa.activate': { label: 'Trial Started', area: 'policy' },
  'cpa.confirm': { label: 'Change Kept (Confirmed)', area: 'policy' },
  'cpa.rollback': { label: 'Change Undone (Rolled Back)', area: 'policy' },
  'cpa.timeout': { label: 'Change Undone Automatically at the Deadline', area: 'policy' },
  publish: { label: 'Policy Activated', area: 'policy' },
  'tpo.create': { label: 'Temporary Rule Added', area: 'overrides' },
  'tpo.revoke': { label: 'Temporary Rule Revoked', area: 'overrides' },
  'tpo.expire': { label: 'Temporary Rule Ended', area: 'overrides' },
  'account.block': { label: 'Account Blocked', area: 'accounts' },
  'account.unblock': { label: 'Account Unblocked', area: 'accounts' },
  'keys.reload': { label: 'Signing Keys Reloaded', area: 'keys' },
  'admin.enrol': { label: 'Administrator Enrolled', area: 'administrators' },
  'admin.password': { label: 'Password Changed', area: 'administrators' },
  'operator.install': { label: 'Installed by the Operator', area: 'operator' },
  'operator.enrol': { label: 'Enrolment Issued by the Operator', area: 'operator' },
  'operator.restore': { label: 'Restored from a Backup by the Operator', area: 'operator' },
  'operator.migrate': { label: 'Database Migrated by the Operator', area: 'operator' },
}

const areas: { id: Area | 'all'; label: string }[] = [
  { id: 'all', label: 'All Areas' },
  { id: 'policy', label: 'Policy Lifecycle' },
  { id: 'overrides', label: 'Temporary Rules' },
  { id: 'accounts', label: 'Blocked Accounts' },
  { id: 'keys', label: 'Signing Keys' },
  { id: 'administrators', label: 'Administrators' },
  { id: 'operator', label: 'Operator' },
]

// The most pages "Load all" reads at once: 2000 entries at 100 a page.
const allPages = 20

// historyPath asks the gateway for one page of the history in the chosen
// order, with the filters applied by the gateway, so a count of matches
// covers the whole history rather than the entries read so far.
function historyPath(filters: HistoryFilters): string {
  const query = new URLSearchParams({ limit: '100', order: filters.newestFirst ? 'desc' : 'asc' })
  if (filters.area !== 'all') {
    query.set(
      'kind',
      Object.entries(kinds)
        .filter(([, k]) => k.area === filters.area)
        .map(([kind]) => kind)
        .join(','),
    )
  }
  if (filters.outcome !== 'all') query.set('outcome', filters.outcome)
  if (filters.actor !== 'all') query.set('actor', filters.actor)
  return `/admin/history?${query}`
}

interface HistoryFilters {
  area: Area | 'all'
  outcome: string
  actor: string
  newestFirst: boolean
}

// History lists every recorded administrative transition as the controller
// stored it: who asked for what, against which revision, and the outcome.
// Request bodies, passwords and tokens are never recorded. It lists the
// newest first unless asked otherwise, and the gateway applies the filters.
export function History({ admin }: ViewProps) {
  const id = useId()
  const [filters, setFilters] = useRemembered<HistoryFilters>('history', { area: 'all', outcome: 'all', actor: 'all', newestFirst: true })
  const path = historyPath(filters)
  const history = usePaged<HistoryEntry>(admin, path, [path])
  const team = useResource<{ items: { name: string }[] }>((signal) => admin.get('/admin/administrators', signal), [])
  const [selected, setSelected] = useState<string | null>(null)
  const { area, outcome, actor, newestFirst } = filters
  const choose = (change: Partial<HistoryFilters>) => setFilters({ ...filters, ...change })

  // The administrators, anyone else seen in the entries read (the operator
  // and the gateway itself) and the current choice.
  const actors = [...new Set([...(team.last?.body.items.map((a) => a.name) ?? []), ...history.items.map((h) => h.actor), ...(actor === 'all' ? [] : [actor])])].sort()
  const shown = history.items
  const filtered = area !== 'all' || outcome !== 'all' || actor !== 'all'

  return (
    <>
      <PageHeader
        title="History"
        lede="Every administrative action, who made it and whether the gateway applied it."
      >
        <ReadStamp okAt={history.last?.at ?? null} failed={history.failed} loading={history.loading} onRefresh={history.refresh} />
      </PageHeader>
      <section className="section">
        {history.reply && history.reply.kind !== 'ok' ? <Problem reply={history.reply} /> : null}
        {history.reply?.kind === 'ok' && history.items.length === 0 && !filtered ? <Empty title="No transition has been recorded yet." /> : null}
        {history.items.length > 0 || filtered ? (
          <>
            <div className="filters" role="group" aria-label="Filter the history">
              <div className="field">
                <label htmlFor={`${id}-area`}>Area</label>
                <select id={`${id}-area`} className="input" value={area} onChange={(e) => choose({ area: e.target.value as Area | 'all' })}>
                  {areas.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.label}
                    </option>
                  ))}
                </select>
              </div>
              <div className="field">
                <label htmlFor={`${id}-outcome`}>Outcome</label>
                <select id={`${id}-outcome`} className="input" value={outcome} onChange={(e) => choose({ outcome: e.target.value })}>
                  <option value="all">All Outcomes</option>
                  <option value="committed">Applied</option>
                  <option value="rejected">Refused</option>
                </select>
              </div>
              <div className="field">
                <label htmlFor={`${id}-actor`}>By</label>
                <select id={`${id}-actor`} className="input" value={actor} onChange={(e) => choose({ actor: e.target.value })}>
                  <option value="all">Anyone</option>
                  {actors.map((a) => (
                    <option key={a} value={a}>
                      {a}
                    </option>
                  ))}
                </select>
              </div>
              <label className="check">
                <input type="checkbox" checked={newestFirst} onChange={(e) => choose({ newestFirst: e.target.checked })} />
                <span>Newest First</span>
              </label>
            </div>
            <p className="hint list-count" role="status">
              {`Showing ${shown.length} ${filtered ? 'matching ' : ''}${shown.length === 1 ? 'entry' : 'entries'}`}
              {history.next
                ? `, ${newestFirst ? 'newest' : 'oldest'} first. More ${filtered ? 'matches' : 'entries'} exist.`
                : filtered
                  ? ': every match in the history.'
                  : ', the whole history.'}
            </p>
          </>
        ) : null}
        {history.reply?.kind === 'ok' && shown.length === 0 && filtered ? <Empty title="No recorded transition matches these filters." /> : null}
        {shown.length > 0 ? (
          <TableScroll label="Administrative History">
            <table>
              <thead>
                <tr>
                  <th scope="col">#</th>
                  <th scope="col">What</th>
                  <th scope="col">By</th>
                  <th scope="col">Target</th>
                  <th scope="col">Outcome</th>
                  <th scope="col">Version</th>
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
                      <td>{actorName(h.actor)}</td>
                      <td>
                        <span className="cell-main">
                          <TargetLink target={h.target} />
                        </span>
                        {targetWords(h.target) !== h.target ? <code className="cell-sub">{h.target}</code> : null}
                      </td>
                      <td>
                        {h.outcome === 'committed' ? <Badge tone="ok">Applied</Badge> : <Badge tone="refused">{h.outcome === 'rejected' ? 'Refused' : h.outcome}</Badge>}
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
              Load All
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
  ['expected_revision', 'Based on version'],
  ['outcome', 'Outcome'],
  ['reason', 'Reason'],
  ['revision', 'Resulting Version'],
  ['candidate', 'Draft Change'],
  ['recorded_at', 'Recorded'],
  ['published_at', 'Published'],
  ['published_by', 'Published by'],
  ['audited_at', 'Recorded in the security log'],
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
              <summary>Record as Stored</summary>
              <CodeBlock label={`Entry ${h.seq} as stored`}>{JSON.stringify(h, null, 2)}</CodeBlock>
            </details>
          </>
        )}
      </ReplyView>
    </section>
  )
}
