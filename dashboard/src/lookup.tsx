import { useEffect, useId, useState, type FormEvent } from 'react'
import type { Admin } from './http'
import type { Page, Reply } from './api'
import { useRemembered, useResource } from './hooks'
import { Empty, Field } from './ui'
import { PageHeader } from './header'
import { Problem, ReplyView } from './replies'
import type { ViewProps } from './views'
import { EventDetail, EventRows, type RequestEvent } from './requests'

// The Request Lookup page: find request events and show one request's decision.

// lookupRef opens Request lookup for one reference.
export function lookupRef(ref: string) {
  window.location.hash = `#/request-lookup?ref=${encodeURIComponent(ref)}`
}

// RequestLookup finds request events by reference, account, operation or
// decision and shows one request's decision path.
const blankLookup = { ref: '', subject: '', operation: '', decision: '' }

export function RequestLookup({ admin }: ViewProps) {
  const id = useId()
  // A Ref in the address starts a new search. Otherwise the last search of
  // this session is shown again: it is kept in the page's memory, so an
  // account searched for never goes into the address or browser history.
  const linked = new URLSearchParams(window.location.hash.split('?')[1] ?? '').get('ref')
  const [saved, remember] = useRemembered('lookup', { form: blankLookup, query: '' })
  const start = linked ? { form: { ...blankLookup, ref: linked }, query: `ref=${encodeURIComponent(linked)}` } : saved
  const [form, setForm] = useState(start.form)
  const [query, setSearch] = useState(start.query)
  const results = useResource<Page<RequestEvent> | null>(
    (signal) => (query ? admin.get(`/admin/events?${query}&limit=50`, signal) : Promise.resolve({ kind: 'ok', status: 200, body: null, etag: null } as Reply<null>)),
    [query],
  )
  const [selected, setSelected] = useState<string | null>(null)
  const setQuery = (next: string, nextForm = form) => {
    setSearch(next)
    remember({ form: nextForm, query: next })
  }
  useEffect(() => {
    if (linked) remember(start)
  }, [])
  const search = (event: FormEvent) => {
    event.preventDefault()
    const q = new URLSearchParams()
    for (const [key, value] of Object.entries(form)) if (value.trim()) q.set(key, value.trim())
    setSelected(null)
    setQuery(q.toString())
  }
  const items = results.reply?.kind === 'ok' && results.reply.body ? results.reply.body.items : []
  const chosen = items.find((e) => e.request_id === selected) ?? (items.length === 1 ? items[0] : undefined)
  return (
    <>
      <PageHeader
        title="Request Lookup"
        search={false}
        lede="Find a request by Ref, account or endpoint and see each check it passed or refused."
      />
      <form className="section card card-pad lookup-form" onSubmit={search} role="search" aria-label="Find Requests">
        <Field id={`${id}-ref`} label="Reference (Ref)">
          <input id={`${id}-ref`} className="input mono" value={form.ref} onChange={(e) => setForm({ ...form, ref: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-subject`} label="Account">
          <input id={`${id}-subject`} className="input mono" value={form.subject} onChange={(e) => setForm({ ...form, subject: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-op`} label="Endpoint">
          <input
            id={`${id}-op`}
            className="input mono"
            value={form.operation}
            onChange={(e) => setForm({ ...form, operation: e.target.value })}
            spellCheck={false}
            autoComplete="off"
          />
        </Field>
        <Field id={`${id}-decision`} label="Decision">
          <select id={`${id}-decision`} className="input" value={form.decision} onChange={(e) => setForm({ ...form, decision: e.target.value })}>
            <option value="">Any</option>
            <option value="allowed">Allowed</option>
            <option value="refused">Refused</option>
          </select>
        </Field>
        <div className="lookup-submit">
          <button type="submit" className="btn btn-primary">
            Find Requests
          </button>
        </div>
      </form>
      {results.reply && results.reply.kind !== 'ok' ? <Problem reply={results.reply} /> : null}
      {query && results.reply?.kind === 'ok' && items.length === 0 ? (
        <Empty title="No request matches.">
          <p className="hint">
            Only the latest 10,000 requests since the gateway started are searched. Older requests are in the security log on the gateway host.
            A full Ref is 32 letters and digits.
          </p>
        </Empty>
      ) : null}
      {!query ? (
        <RecentRefusals
          admin={admin}
          pick={(ref) => {
            const picked = { ...blankLookup, ref }
            setForm(picked)
            setSelected(null)
            setQuery(`ref=${encodeURIComponent(ref)}`, picked)
          }}
        />
      ) : null}
      {chosen ? <EventDetail event={chosen} /> : null}
      {items.length > 1 ? (
        <section className="section card" aria-labelledby="matches-title">
          <div className="card-head card-head-pad">
            <h2 id="matches-title">{items.length} matching requests</h2>
            <span className="muted">Newest first. Choose a Ref to see its decision.</span>
          </div>
          <div className="card-body">
            <EventRows events={items} open={setSelected} label="Matching Requests" />
          </div>
        </section>
      ) : null}
    </>
  )
}

// RecentRefusals gives Request lookup a starting point before any search: the
// latest refusals, each one choice away from its full decision.
function RecentRefusals({ admin, pick }: { admin: Admin; pick: (ref: string) => void }) {
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?decision=refused&limit=10', signal), [])
  return (
    <section className="section card" aria-labelledby="recent-title">
      <div className="card-head card-head-pad">
        <h2 id="recent-title">Latest Refusals</h2>
        <span className="muted">A Ref from an error message finds exactly one request.</span>
      </div>
      <div className="card-body">
        <ReplyView reply={recent.reply}>
          {(r) => (r.items.length ? <EventRows events={r.items} open={pick} label="Latest Refusals" /> : <p className="hint card-empty">Nothing refused recently.</p>)}
        </ReplyView>
      </div>
    </section>
  )
}
