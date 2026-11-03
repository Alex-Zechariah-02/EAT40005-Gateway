import { useId, useState, type FormEvent } from 'react'
import type { Candidate, Page, Reply, Revision } from './api'
import { usePoll, useResource } from './hooks'
import { EventDetail, EventRows, type RequestEvent } from './monitor'
import { Badge, Empty, Field, PageHeader, Problem, ReadStamp, ReplyView, TableScroll } from './ui'
import type { ViewProps } from './views'

// The security log search, the access checker and the limits page: three
// reads the gateway answers itself. None of them changes anything.

interface LogPage {
  items: RequestEvent[]
  next: string | null
  complete: boolean
  files_searched: string[]
  files_missing: string[]
  unreadable_lines: number
}

const blankLogForm = { ref: '', subject: '', operation: '', decision: '', from: '', to: '' }

// logQuery turns the form into the search's query string; times entered in
// local time are sent as instants.
function logQuery(form: typeof blankLogForm): URLSearchParams {
  const query = new URLSearchParams()
  for (const name of ['ref', 'subject', 'operation', 'decision'] as const) {
    if (form[name].trim()) query.set(name, form[name].trim())
  }
  for (const name of ['from', 'to'] as const) {
    if (form[name]) query.set(name, new Date(form[name]).toISOString())
  }
  return query
}

export function SecurityLogView({ admin }: ViewProps) {
  const id = useId()
  const [form, setForm] = useState(blankLogForm)
  const [query, setQuery] = useState<URLSearchParams | null>(null)
  const [pages, setPages] = useState<LogPage[]>([])
  const [reply, setReply] = useState<Reply<LogPage> | null>(null)
  const [loading, setLoading] = useState(false)
  const [selected, setSelected] = useState<string | null>(null)

  async function read(next: URLSearchParams, more: boolean) {
    setLoading(true)
    const answer = await admin.get<LogPage>(`/admin/security-log?${next}`)
    setLoading(false)
    setReply(answer)
    if (answer.kind === 'ok') setPages((earlier) => (more ? [...earlier, answer.body] : [answer.body]))
  }
  function search(event: FormEvent) {
    event.preventDefault()
    const next = logQuery(form)
    setQuery(next)
    setSelected(null)
    void read(next, false)
  }
  const items = pages.flatMap((page) => page.items)
  const last = pages.at(-1)
  const chosen = items.find((item) => item.request_id === selected)
  const exportQuery = (format: string) => `/admin/security-log?${new URLSearchParams([...(query ?? new URLSearchParams()), ['format', format]])}`
  const set = (name: keyof typeof blankLogForm) => (e: { target: { value: string } }) => setForm({ ...form, [name]: e.target.value })

  return (
    <>
      <PageHeader title="Security Log" lede="Search every request the gateway recorded, in the current and earlier log files, and export the results." />
      <form className="section card card-pad log-form" onSubmit={search} role="search" aria-label="Search the Security Log">
        <Field id={`${id}-ref`} label="Reference (Ref)">
          <input id={`${id}-ref`} className="input mono" value={form.ref} onChange={set('ref')} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-subject`} label="Account">
          <input id={`${id}-subject`} className="input mono" value={form.subject} onChange={set('subject')} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-op`} label="Endpoint">
          <input id={`${id}-op`} className="input mono" value={form.operation} onChange={set('operation')} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-decision`} label="Decision">
          <select id={`${id}-decision`} className="input" value={form.decision} onChange={set('decision')}>
            <option value="">Any</option>
            <option value="allowed">Allowed</option>
            <option value="refused">Refused</option>
            <option value="failed">Outcome Unknown</option>
          </select>
        </Field>
        <Field id={`${id}-from`} label="From">
          <input id={`${id}-from`} type="datetime-local" className="input" value={form.from} onChange={set('from')} />
        </Field>
        <Field id={`${id}-to`} label="Until">
          <input id={`${id}-to`} type="datetime-local" className="input" value={form.to} onChange={set('to')} />
        </Field>
        <div className="log-form-actions">
          <button type="submit" className="btn btn-primary" disabled={loading}>
            {loading && !pages.length ? 'Searching…' : 'Search'}
          </button>
          {query ? (
            <>
              <a className="btn" href={exportQuery('csv')} download>
                Export CSV
              </a>
              <a className="btn" href={exportQuery('json')} download>
                Export JSON
              </a>
            </>
          ) : null}
        </div>
        <p className="hint log-form-hint">Exports hold up to 10,000 requests with the same filters. Searches and exports are recorded in the security log.</p>
      </form>
      {reply && reply.kind !== 'ok' ? <Problem reply={reply} /> : null}
      {chosen ? <EventDetail event={chosen} /> : null}
      {last ? (
        <section className="section card" aria-labelledby="log-title">
          <div className="card-head card-head-pad">
            <h2 id="log-title">{items.length === 1 ? '1 request' : `${items.length} requests`}</h2>
            <span className="muted">Newest first. Choose a Ref to see its checks.</span>
          </div>
          <div className="card-body">
            {items.length ? <EventRows events={items} open={setSelected} label="Security Log Results" /> : <Empty title="No request matches." />}
          </div>
          <div className="card-pad-x log-coverage">
            <p className="hint">
              Searched {last.files_searched.join(', ') || 'no files'}.
              {last.files_missing.length ? ` Not searched, moved away from the gateway: ${last.files_missing.join(', ')}.` : ''}
              {last.unreadable_lines ? ` ${last.unreadable_lines} damaged lines could not be read.` : ''}
              {last.complete ? ' This is everything the gateway still holds.' : ' Older records remain.'}
            </p>
            {last.next ? (
              <button
                type="button"
                className="btn btn-small"
                disabled={loading}
                onClick={() => void read(new URLSearchParams([...(query ?? new URLSearchParams()), ['after', last.next!]]), true)}
              >
                {loading ? 'Searching…' : 'Show Older'}
              </button>
            ) : null}
          </div>
        </section>
      ) : null}
    </>
  )
}

interface AccessAnswer {
  decision: 'allowed' | 'refused'
  code: string | null
  check: 'route' | 'access' | 'blocked' | 'role' | null
  operation: string | null
  public: boolean
  roles: string[]
  ignored_roles: string[]
  generation: string
  candidate: number | null
  not_checked: string[]
}

const checkWords: Record<string, string> = {
  route: 'No endpoint matches this method and path.',
  access: 'The endpoint is switched off for everyone (access deny).',
  blocked: 'The account is on the blocked list.',
  role: 'None of these roles has permission for the endpoint.',
}

const uncheckedWords: Record<string, string> = {
  token: 'a valid sign-in token',
  rate: 'the rate limit, which depends on live traffic',
  body: 'the request body rules, which depend on the real body',
}

export function AccessCheckView({ admin }: ViewProps) {
  const id = useId()
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const drafts = useResource<Page<Candidate>>((signal) => admin.get('/admin/candidates', signal), [])
  const [form, setForm] = useState({ role: '', subject: '', endpoint: '', candidate: '' })
  const [reply, setReply] = useState<Reply<AccessAnswer> | null>(null)
  const [pending, setPending] = useState(false)
  const document = policy.reply?.kind === 'ok' ? policy.reply.body.document : undefined
  const operations = document?.operations ?? []
  const open = drafts.reply?.kind === 'ok' ? drafts.reply.body.items.filter((c) => c.status === 'validated') : []

  async function check(event: FormEvent) {
    event.preventDefault()
    const operation = operations.find((op) => op.id === form.endpoint)
    if (!operation) return
    setPending(true)
    const answer = await admin.send<AccessAnswer>({
      method: 'POST',
      path: '/admin/access-check',
      body: {
        method: operation.method,
        // A prefix endpoint is checked with a sample path inside it.
        path: operation.target.endsWith('/*') ? operation.target.slice(0, -1) + 'example' : operation.target,
        roles: form.role ? [form.role] : [],
        subject: form.subject.trim() || null,
        candidate: form.candidate ? Number(form.candidate) : null,
      },
    })
    setPending(false)
    setReply(answer)
  }

  return (
    <>
      <PageHeader title="Access Checker" lede="Ask the gateway whether a role may call an endpoint, and which check decides." />
      <form className="section card card-pad log-form" onSubmit={check} aria-label="Check Access">
        <Field id={`${id}-role`} label="Role">
          <select id={`${id}-role`} className="input" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
            <option value="">No role</option>
            {document?.roles.map((role) => (
              <option key={role.id} value={role.id}>
                {role.id}
              </option>
            ))}
          </select>
        </Field>
        <Field id={`${id}-endpoint`} label="Endpoint" wide>
          <select id={`${id}-endpoint`} className="input" value={form.endpoint} onChange={(e) => setForm({ ...form, endpoint: e.target.value })} required>
            <option value="">Choose an endpoint</option>
            {operations.map((op) => (
              <option key={op.id} value={op.id}>
                {op.method} {op.target} ({op.id})
              </option>
            ))}
          </select>
        </Field>
        <Field id={`${id}-subject`} label="Account (optional)">
          <input id={`${id}-subject`} className="input mono" value={form.subject} onChange={(e) => setForm({ ...form, subject: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-candidate`} label="Policy">
          <select id={`${id}-candidate`} className="input" value={form.candidate} onChange={(e) => setForm({ ...form, candidate: e.target.value })}>
            <option value="">The policy in force</option>
            {open.map((c) => (
              <option key={c.id} value={c.id}>
                Draft change {c.id}
              </option>
            ))}
          </select>
        </Field>
        <div className="log-form-actions">
          <button type="submit" className="btn btn-primary" disabled={pending || !form.endpoint}>
            {pending ? 'Checking…' : 'Check Access'}
          </button>
        </div>
        <p className="hint log-form-hint">The role stands for a role in a verified sign-in token. Give an account to also check the blocked list.</p>
      </form>
      {policy.reply && policy.reply.kind !== 'ok' ? <Problem reply={policy.reply} /> : null}
      {reply && reply.kind !== 'ok' ? <Problem reply={reply} /> : null}
      {reply?.kind === 'ok' ? <AccessResult answer={reply.body} /> : null}
    </>
  )
}

function AccessResult({ answer }: { answer: AccessAnswer }) {
  const allowed = answer.decision === 'allowed'
  return (
    <section className="section card card-pad" aria-labelledby="access-result" aria-live="polite">
      <h2 id="access-result" className="access-verdict">
        <Badge tone={allowed ? 'ok' : 'refused'}>{allowed ? 'Allowed' : 'Refused'}</Badge>{' '}
        {allowed ? (answer.public ? 'A public endpoint: anyone may call it.' : 'These checks pass.') : checkWords[answer.check ?? ''] ?? answer.code}
      </h2>
      <p className="hint">
        {answer.candidate ? `Checked against draft change ${answer.candidate}, with the temporary rules in force.` : 'Checked against the policy in force, including temporary rules.'}{' '}
        {answer.ignored_roles.length ? `Roles the policy does not define are ignored: ${answer.ignored_roles.join(', ')}.` : ''}
      </p>
      {answer.not_checked.length ? (
        <p className="hint">
          A real request must also pass {answer.not_checked.length === 1 ? 'one check' : `${answer.not_checked.length} checks`} not made here:{' '}
          {answer.not_checked.map((step) => uncheckedWords[step] ?? step).join('; ')}. These were not checked.
        </p>
      ) : null}
    </section>
  )
}

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
                  {body.unused_permits.since ? ` since ${new Date(body.unused_permits.since).toLocaleString()}` : ''}.
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
