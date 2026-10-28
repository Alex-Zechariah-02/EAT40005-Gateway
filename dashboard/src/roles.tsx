import type { Operation, OverridesView, Page, Policy, Revision } from './api'
import { useResource } from './hooks'
import type { RequestEvent } from './monitor'
import { overrideChange } from './overrides'
import { useState } from 'react'
import { Empty, ListToolbar, matches, PageHeader, Problem, ReadStamp, ReplyView, time } from './ui'
import type { ViewProps } from './views'

// The latest recorded requests this page reads for the accounts seen with
// each role: one page of the gateway's recent events.
const recentRequests = 100

function rateWords(operation: Operation): string {
  if (!operation.rate) return 'no rate limit'
  const per = operation.rate.key === 'client_address' ? 'client address' : 'account'
  return `up to ${operation.rate.count} per ${operation.rate.window_seconds} s per ${per}`
}

function EndpointLine({ operation }: { operation: Operation }) {
  return (
    <li>
      <code>{operation.id}</code> <span className="muted">{`${operation.method} ${operation.target}`}</span> · {rateWords(operation)}
      {operation.access === 'deny' ? ' · denied for everyone' : ''}
    </li>
  )
}

// Roles states what each role may do under the policy in force, in plain
// sentences, with the temporary rules that change it now listed beside it,
// never merged in. It only reads: the gateway decides every request, and a
// role comes only from a verified sign-in token.
export function Roles({ admin, can }: ViewProps) {
  const [query, setQuery] = useState('')
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const rules = useResource<OverridesView>((signal) => admin.get('/admin/overrides', signal), [])
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get(`/admin/events?limit=${recentRequests}`, signal), [])
  const refresh = () => {
    policy.refresh()
    rules.refresh()
    recent.refresh()
  }
  const active = rules.reply?.kind === 'ok' ? rules.reply.body.overrides : []
  const onTrial = rules.reply?.kind === 'ok' ? rules.reply.body.provisional : undefined
  const seen = new Map<string, string[]>()
  if (recent.reply?.kind === 'ok') {
    for (const event of recent.reply.body.items) {
      if (!event.subject) continue
      for (const role of event.roles) {
        const subjects = seen.get(role) ?? []
        if (!subjects.includes(event.subject)) seen.set(role, [...subjects, event.subject])
      }
    }
  }
  return (
    <>
      <PageHeader
        title="Roles"
        lede="What each role can do under the policy in force. A role comes only from a verified sign-in token; this page changes nothing. Change permissions on the Policy page."
      >
        <ReadStamp
          okAt={policy.last?.at ?? null}
          failed={policy.failed}
          loading={policy.loading || rules.loading || recent.loading}
          onRefresh={refresh}
        />
      </PageHeader>
      {rules.reply && rules.reply.kind !== 'ok' ? <Problem reply={rules.reply} /> : null}
      {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
      <ReplyView reply={policy.reply}>
        {(revision) => {
          const doc = revision.document as Policy | undefined
          if (!doc) return <Empty title="No policy is in force yet." />
          const allowedFor = (role: string) =>
            doc.operations.filter((operation) => doc.permits.some((permit) => permit.role === role && permit.operation === operation.id))
          const open = doc.operations.filter((operation) => operation.auth === 'none')
          const shownOpen = open.filter((operation) => matches(query, 'anyone', operation.id, `${operation.method} ${operation.target}`))
          const shown = doc.roles.filter((role) => matches(query, role.id, ...allowedFor(role.id).map((operation) => operation.id)))
          return (
            <>
              <p className="hint">
                Shown: version {revision.id}
                {onTrial === revision.id ? ', on trial: it is undone at its deadline unless someone keeps it' : ''}. Accounts are those seen in the
                latest {recentRequests} requests.
              </p>
              <ListToolbar
                label="Filter roles and endpoints"
                query={query}
                onQuery={setQuery}
                shown={shown.length}
                total={doc.roles.length}
                noun={['role', 'roles']}
              >
                {can('manage_policy') ? (
                  <a className="btn btn-primary" href="#/designer">
                    Change permissions in the designer
                  </a>
                ) : null}
              </ListToolbar>
              {shown.length === 0 && shownOpen.length === 0 ? <p className="hint">No role or endpoint matches the filter.</p> : null}
              {shownOpen.length > 0 ? (
                <section className="section card card-pad" aria-labelledby="roles-public">
                  <h2 id="roles-public">Anyone, without signing in</h2>
                  <p className="hint">These endpoints need no role.</p>
                  <ul className="role-list">
                    {shownOpen.map((operation) => (
                      <EndpointLine key={operation.id} operation={operation} />
                    ))}
                  </ul>
                </section>
              ) : null}
              {shown.map((role) => {
                const allowed = allowedFor(role.id)
                const temporary = active.filter(
                  (rule) => rule.role === role.id || (rule.role === undefined && allowed.some((operation) => operation.id === rule.operation)),
                )
                const accounts = seen.get(role.id) ?? []
                return (
                  <section key={role.id} className="section card card-pad" aria-labelledby={`role-${role.id}`}>
                    <h2 id={`role-${role.id}`}>{role.id}</h2>
                    {allowed.length === 0 ? (
                      <p>This role has no permissions: an account with only this role can call no endpoint that needs a sign-in.</p>
                    ) : (
                      <>
                        <p>
                          Can call {allowed.length} {allowed.length === 1 ? 'endpoint' : 'endpoints'}:
                        </p>
                        <ul className="role-list">
                          {allowed.map((operation) => (
                            <EndpointLine key={operation.id} operation={operation} />
                          ))}
                        </ul>
                      </>
                    )}
                    {temporary.length > 0 ? (
                      <>
                        <h3>Temporary rules now</h3>
                        <ul className="role-list">
                          {temporary.map((rule) => (
                            <li key={rule.id}>
                              Rule {rule.id}: {overrideChange(rule, doc)}, until {time(rule.expires_at)}
                            </li>
                          ))}
                        </ul>
                      </>
                    ) : null}
                    <p className="hint">
                      {accounts.length > 0 ? (
                        <>
                          Seen in recent requests:{' '}
                          {accounts.map((subject, index) => (
                            <span key={subject}>
                              {index > 0 ? ', ' : ''}
                              <code>{subject}</code>
                            </span>
                          ))}
                        </>
                      ) : (
                        'No account with this role in recent requests.'
                      )}
                    </p>
                  </section>
                )
              })}
            </>
          )
        }}
      </ReplyView>
    </>
  )
}
