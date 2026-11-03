import { useId, useState } from 'react'
import type { Candidate, Page } from '../shared/api'
import { usePoll, useResource } from '../shared/hooks'
import { Badge, Field, TableScroll } from '../shared/ui'
import { PageHeader } from '../app/header'
import { ReplyView } from '../shared/replies'
import { ReadStamp, Time } from '../shared/time'
import type { ViewProps } from '../app/views'

// The limits page, the advice page and the Overview's alerts: reads the
// gateway answers itself. None of them changes anything.

interface Limit {
  used: number
  max: number
}

const limitRows: { id: string; name: string; atLimit: string; bytes?: boolean }[] = [
  { id: 'temporary_rules', name: 'Active Temporary Rules', atLimit: 'New temporary rules are refused until one ends or is revoked.' },
  { id: 'live_views', name: 'Open Live Views', atLimit: 'Another Live Traffic view cannot connect until one closes.' },
  { id: 'administrator_sessions', name: 'Administrator Sessions', atLimit: 'New sign-ins are refused until a session ends.' },
  { id: 'audit_files', name: 'Security Log Files', atLimit: 'The gateway stops accepting requests until earlier files are moved away and it restarts.' },
  { id: 'audit_active_bytes', name: 'Current Security Log File', atLimit: 'The file is closed and a new one started.', bytes: true },
  { id: 'rate_counters', name: 'Rate Limit Counters', atLimit: 'Requests that need a new counter are refused until older ones expire.' },
  { id: 'recent_requests', name: 'Recent Requests Kept for Live Views', atLimit: 'The oldest is dropped; the security log keeps every request.' },
]

function amount(value: number, bytes?: boolean): string {
  if (!bytes) return value.toLocaleString()
  return value >= 1 << 20 ? `${(value / (1 << 20)).toFixed(1)} MiB` : `${(value / 1024).toFixed(1)} KiB`
}

export function LimitsView({ admin }: ViewProps) {
  const limits = useResource<Record<string, Limit>>((signal) => admin.get('/admin/limits', signal), [])
  return (
    <>
      <PageHeader title="Limits" lede="The gateway's built-in limits, how much of each is in use, and what happens at the limit.">
        <ReadStamp okAt={limits.last?.at ?? null} failed={limits.failed} loading={limits.loading} onRefresh={limits.refresh} />
      </PageHeader>
      <section className="section card" aria-label="Limits">
        <ReplyView reply={limits.reply}>
          {(body) => (
            <TableScroll label="Limits">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Limit</th>
                    <th scope="col">In Use</th>
                    <th scope="col">At the Limit</th>
                  </tr>
                </thead>
                <tbody>
                  {limitRows.map((row) => {
                    const limit = body[row.id]
                    if (!limit) return null
                    const share = limit.max ? limit.used / limit.max : 0
                    return (
                      <tr key={row.id}>
                        <th scope="row">{row.name}</th>
                        <td className="nowrap">
                          <span className="num">
                            {amount(limit.used, row.bytes)} of {amount(limit.max, row.bytes)}
                          </span>{' '}
                          {share >= 0.8 ? <Badge tone={share >= 1 ? 'refused' : 'unknown'}>{share >= 1 ? 'At the limit' : 'Nearly full'}</Badge> : null}
                          <meter className="limit-meter" min={0} max={limit.max} value={limit.used} low={limit.max * 0.8} high={limit.max * 0.95} optimum={0} aria-label={`${row.name} in use`} />
                        </td>
                        <td className="muted">{row.atLimit}</td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </TableScroll>
          )}
        </ReplyView>
      </section>
    </>
  )
}

interface Finding {
  id: string
  severity: 'critical' | 'high' | 'medium' | 'low'
  object: string
  message: string
}

const severityTone = { critical: 'refused', high: 'refused', medium: 'unknown', low: 'neutral' } as const
const severityWord = { critical: 'Critical', high: 'High', medium: 'Medium', low: 'Low' }

function FindingRows({ items, label }: { items: Finding[]; label: string }) {
  return (
    <TableScroll label={label}>
      <table>
        <thead>
          <tr>
            <th scope="col">Severity</th>
            <th scope="col">Finding</th>
            <th scope="col">Applies To</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr key={`${item.id}:${item.object}`}>
              <td className="nowrap">
                <Badge tone={severityTone[item.severity]}>{severityWord[item.severity]}</Badge>
              </td>
              <td>{item.message}</td>
              <td>
                <code>{item.object}</code> <span className="muted">({item.id})</span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </TableScroll>
  )
}

interface AdviceAnswer {
  rules: string
  candidate: number | null
  warnings: Finding[]
  unused_permits: {
    requests: number
    since: string | null
    complete: boolean
    files_searched: string[]
    files_missing: string[]
    withheld: string | null
    items: Finding[]
    min_minutes: number
    min_requests: number
  }
}

// AdviceView shows the gateway's fixed checks: settings worth a second look
// and permissions no request has used. They advise only.
export function AdviceView({ admin }: ViewProps) {
  const id = useId()
  const [candidate, setCandidate] = useState('')
  const drafts = useResource<Page<Candidate>>((signal) => admin.get('/admin/candidates', signal), [])
  const advice = useResource<AdviceAnswer>((signal) => admin.get(`/admin/advice${candidate ? `?candidate=${candidate}` : ''}`, signal), [candidate])
  const open = drafts.reply?.kind === 'ok' ? drafts.reply.body.items.filter((c) => c.status === 'validated') : []
  return (
    <>
      <PageHeader title="Advice" lede="Settings worth a second look and permissions no request has used. Advice only.">
        <ReadStamp okAt={advice.last?.at ?? null} failed={advice.failed} loading={advice.loading} onRefresh={advice.refresh} />
      </PageHeader>
      <div className="section card card-pad log-form">
        <Field id={`${id}-policy`} label="Policy">
          <select id={`${id}-policy`} className="input" value={candidate} onChange={(e) => setCandidate(e.target.value)}>
            <option value="">The policy in force</option>
            {open.map((c) => (
              <option key={c.id} value={c.id}>
                Draft change {c.id}
              </option>
            ))}
          </select>
        </Field>
      </div>
      <ReplyView reply={advice.reply}>
        {(body) => (
          <>
            <section className="section card" aria-labelledby="warnings-title">
              <div className="card-head card-head-pad">
                <h2 id="warnings-title">Settings to Review</h2>
                <span className="muted">{body.warnings.length ? `${body.warnings.length} found` : 'None found'}</span>
              </div>
              <div className="card-body">
                {body.warnings.length ? <FindingRows items={body.warnings} label="Settings to Review" /> : <p className="hint card-empty">No setting matches a check.</p>}
              </div>
            </section>
            <section className="section card" aria-labelledby="unused-title">
              <div className="card-head card-head-pad">
                <h2 id="unused-title">Unused Permissions</h2>
                <span className="muted">
                  Based on {body.unused_permits.requests.toLocaleString()} requests in the security log
                  {body.unused_permits.since ? (
                    <>
                      {' '}
                      since <Time value={body.unused_permits.since} />
                    </>
                  ) : (
                    ''
                  )}
                  .
                </span>
              </div>
              <div className="card-body">
                {body.unused_permits.files_missing.length || !body.unused_permits.complete ? (
                  <p className="hint card-pad-x">
                    {body.unused_permits.files_missing.length ? `Not read, moved away from the gateway: ${body.unused_permits.files_missing.join(', ')}. ` : ''}
                    {body.unused_permits.complete ? '' : 'Only the newest part of the log was read; older requests may have used these permissions.'}
                  </p>
                ) : null}
                {body.unused_permits.withheld ? (
                  <p className="hint card-empty">
                    {body.unused_permits.withheld} Shown once the security log covers {body.unused_permits.min_minutes} minutes and at least{' '}
                    {body.unused_permits.min_requests.toLocaleString()} requests.
                  </p>
                ) : body.unused_permits.items.length ? (
                  <FindingRows items={body.unused_permits.items} label="Unused Permissions" />
                ) : (
                  <p className="hint card-empty">Every permission was used.</p>
                )}
              </div>
            </section>
            <p className="hint section">Checked with rule set {body.rules}{body.candidate ? `, against draft change ${body.candidate}` : ''}.</p>
          </>
        )}
      </ReplyView>
    </>
  )
}

// Alerts shows the gateway's active alerts above the Overview, and nothing
// when there are none. It reads every 5 seconds: each read is recorded in
// the security log, so it is not read every second.
export function Alerts({ admin, open }: { admin: ViewProps['admin']; open: ViewProps['open'] }) {
  const alerts = useResource<{ alerts: Finding[] }>((signal) => admin.get('/admin/alerts', signal), [])
  usePoll(alerts.refresh, 5_000, false)
  const items = alerts.reply?.kind === 'ok' ? alerts.reply.body.alerts : []
  if (!items.length) return null
  return (
    <section className="section notice tone-refused" aria-label="Alerts" role="status">
      <p className="notice-title">{items.length === 1 ? '1 alert' : `${items.length} alerts`}</p>
      <ul className="alert-list">
        {items.map((item) => (
          <li key={item.id}>
            <Badge tone={severityTone[item.severity]}>{severityWord[item.severity]}</Badge> {item.message}{' '}
            {item.id === 'A1' ? (
              <button type="button" className="link-button" onClick={() => open('request-lookup')}>
                Look Up Requests
              </button>
            ) : item.id === 'A3' ? (
              <button type="button" className="link-button" onClick={() => open('limits')}>
                See Limits
              </button>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  )
}
