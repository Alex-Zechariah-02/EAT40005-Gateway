import { useEffect, useState } from 'react'
import type { Override, Policy } from '../shared/api'
import { capital } from './designer/model'
import type { RequestEvent } from '../monitor/requests'
import { overrideChange } from '../deploy/rule'
import { Badge, ListToolbar, matches, TableScroll } from '../shared/ui'
import { Time } from '../shared/time'
import { XRef } from '../shared/links'
import { limitWords, publicTab, recentRequests, sharedLimit } from './policy'
import { PermitMatrix } from './revision'

// What one role may call under the active policy, chosen from tabs.

// RolesSection shows what one role may call under the active policy, chosen
// from tabs: its endpoints grouped by API in one table, the rate limit said
// once per group, and the temporary rules that change it now listed beside
// it, never merged in. A role comes only from a verified sign-in token.
export function RolesSection(props: { policy: Policy; active: Override[]; recent: RequestEvent[]; canChange: boolean; linked: string | null }) {
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
                  Rule {rule.id}: {overrideChange(rule, doc)}, until <Time value={rule.expires_at} />
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
