import { useId, useState, type FormEvent } from 'react'
import type { Reply } from '../shared/api'
import { EventDetail, EventRows, type RequestEvent } from './requests'
import { Empty, Field } from '../shared/ui'
import { PageHeader } from '../app/header'
import { Problem } from '../shared/replies'
import type { ViewProps } from '../app/views'

// The Security Log page: a search over the gateway's recorded request outcomes.

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
