import { useContext, useState } from 'react'
import { forwardRule, type Policy, type Revision } from '../shared/api'
import { useGatewayState, liveMs, usePoll, useResource, ViewMemory } from '../shared/hooks'
import { RequestRules } from './request-rules'
import { Badge, Empty, ListToolbar, matches, MoreDetails, TableScroll } from '../shared/ui'
import { PageHeader } from '../app/header'
import { Problem, ReplyView } from '../shared/replies'
import { ReadStamp } from '../shared/time'
import { useFocus, useTarget, XRef } from '../shared/links'
import { connectHandoff, type ViewProps } from '../app/views'
import type { Traffic } from '../monitor/summary'

// The APIs page: each API's endpoints, who may call them, limits and health.

// apiName names an API; the gateway counts requests that matched no route
// under "unknown".
export function apiName(id: string): string {
  if (id === 'unknown') return 'No route matched'
  return id.slice(0, 1).toUpperCase() + id.slice(1)
}

// limitShort states a rate limit briefly, such as "120/min". The counting key
// is said once above the table; a limit that counts the other way says so.
function limitShort(o: Policy['operations'][number]): string {
  if (!o.rate) return 'None'
  const seconds = o.rate.window_seconds
  const span = seconds === 1 ? 's' : seconds === 60 ? 'min' : seconds === 3600 ? 'h' : `${seconds} s`
  const usual = o.auth === 'none' ? 'client_address' : 'subject'
  const unusual = o.rate.key !== usual ? (o.rate.key === 'client_address' ? ' per address' : ' per account') : ''
  return `${o.rate.count}/${span}${unusual}`
}

function bytesShort(bytes: number): string {
  return bytes >= 1024 ? `${(bytes / 1024).toLocaleString(undefined, { maximumFractionDigits: 1 })} KB` : `${bytes} B`
}

// ApisView shows one block per API: its operations, who may call them, their
// limits and how the API is answering now.
export function ApisView({ admin, can, open }: ViewProps) {
  const state = useGatewayState()
  const memory = useContext(ViewMemory)
  const [query, setQuery] = useState('')
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const traffic = useResource<Traffic>((signal) => admin.get('/admin/traffic?window=300', signal), [])
  usePoll(traffic.refresh, liveMs, state.critical)
  // The last figures read stay shown when a refresh fails; the problem
  // notice above says the latest read failed.
  const health = traffic.last?.body.by_api ?? []
  // A link from another page names one API or one endpoint: it is brought
  // into view and highlighted.
  const linkedApi = useFocus('api')
  const linkedOperation = useFocus('operation')
  useTarget(policy.reply?.kind === 'ok', linkedOperation ?? linkedApi)
  return (
    <>
      <PageHeader
        title="APIs"
        lede="Each protected API with its endpoints, who may call them, limits and traffic."
      >
        <ReadStamp
          okAt={policy.last?.at ?? null} failed={policy.failed}
          loading={policy.loading}
          onRefresh={() => {
            policy.refresh()
            traffic.refresh()
          }}
        />
      </PageHeader>
      {traffic.reply && traffic.reply.kind !== 'ok' ? <Problem reply={traffic.reply} /> : null}
      <ReplyView reply={policy.reply}>
        {(revision) => {
          const doc = revision.document as Policy | undefined
          if (!doc) return <Empty title="No policy is active yet." />
          const found = (o: (typeof doc.operations)[number]) =>
            matches(
              query,
              o.api,
              apiName(o.api),
              o.id,
              `${o.method} ${o.target}`,
              ...doc.permits.filter((p) => p.operation === o.id).map((p) => p.role),
            )
          const shown = doc.operations.filter(found).length
          return (
            <>
              <ListToolbar
                label="Filter APIs and endpoints"
                query={query}
                onQuery={setQuery}
                shown={shown}
                total={doc.operations.length}
                noun={['endpoint', 'endpoints']}
              >
                {can('manage_policy') ? (
                  <>
                    <a className="btn" href="#/policy">
                      Edit Endpoints in Policy
                    </a>
                    <button
                      type="button"
                      className="btn btn-primary"
                      onClick={() => {
                        memory?.set(connectHandoff, true)
                        open('policy')
                      }}
                    >
                      Connect an API
                    </button>
                  </>
                ) : null}
              </ListToolbar>
              {shown === 0 ? <p className="hint">No API or endpoint matches the filter.</p> : null}
              {doc.apis.map((api) => {
                const all = doc.operations.filter((o) => o.api === api.id)
                const ops = all.filter(found)
                if (ops.length === 0 && all.length > 0) return null
                const h = health.find((x) => x.api === api.id)
                const upstream = doc.upstreams.find((u) => u.id === api.upstream)
                return (
                  <section
                    key={api.id}
                    className="section card"
                    aria-labelledby={`api-${api.id}`}
                    data-target={!linkedOperation && linkedApi === api.id ? 'true' : undefined}
                  >
                    <div className="card-head card-head-pad api-head">
                      <div>
                        <h2 id={`api-${api.id}`}>{apiName(api.id)}</h2>
                        <p className="hint">
                          {all.length} endpoints · service address <code>{upstream?.dial ?? api.upstream}</code> · limits count per client
                          address for public endpoints, per account otherwise
                        </p>
                        {forwardRule(api.forward) ? (
                          <p className="hint">
                            Forwarded As: {forwardRule(api.forward)}. Checks use the path the client sent.
                          </p>
                        ) : null}
                      </div>
                      {h ? (
                        <dl className="api-figures">
                          <div>
                            <dt>Requests</dt>
                            <dd>{h.total.toLocaleString()}</dd>
                          </div>
                          <div>
                            <dt>Refused</dt>
                            <dd>{h.refused.toLocaleString()}</dd>
                          </div>
                          <div>
                            <dt>Typical</dt>
                            <dd>{h.p50_ms.toFixed(1)} ms</dd>
                          </div>
                          <div>
                            <dt>Slowest 5%</dt>
                            <dd>{h.p95_ms.toFixed(1)} ms</dd>
                          </div>
                        </dl>
                      ) : (
                        <span className="muted">No requests in the last 5 minutes</span>
                      )}
                    </div>
                    <div className="card-body">
                      <TableScroll label={`Endpoints of ${api.id}`}>
                        <table className="ops-table">
                          <thead>
                            <tr>
                              <th scope="col">Endpoint</th>
                              <th scope="col">Who May Call</th>
                              <th scope="col">Limit</th>
                              <th scope="col">Body</th>
                            </tr>
                          </thead>
                          <tbody>
                            {ops.map((o) => {
                              const roles = doc.permits.filter((p) => p.operation === o.id).map((p) => p.role)
                              return (
                                <tr key={o.id} data-target={linkedOperation === o.id ? 'true' : undefined}>
                                  <td>
                                    <code className="ops-route">
                                      <span className="method">{o.method}</span> {o.target}
                                    </code>
                                    <span className="cell-sub">
                                      <XRef view="policy" focus={{ select: `operation:${o.id}` }}>
                                        <code>{o.id}</code>
                                      </XRef>
                                    </span>
                                  </td>
                                  <td>
                                    {o.access !== 'allow' ? (
                                      <Badge tone="refused">Closed</Badge>
                                    ) : o.auth !== 'required' ? (
                                      <span className="chip">Public</span>
                                    ) : roles.length ? (
                                      <span className="chips">
                                        {roles.map((role) => (
                                          <XRef key={role} view="versions" focus={{ role }}>
                                            <span className="chip">{role}</span>
                                          </XRef>
                                        ))}
                                      </span>
                                    ) : (
                                      <span className="muted">No Role Yet</span>
                                    )}
                                  </td>
                                  <td className="nowrap">{limitShort(o)}</td>
                                  <td className="nowrap">
                                    {o.body ? `JSON ≤ ${bytesShort(o.body.max_bytes)}` : <span className="muted">None</span>}
                                  </td>
                                </tr>
                              )
                            })}
                          </tbody>
                        </table>
                      </TableScroll>
                      {ops.some((o) => o.body || o.headers) ? (
                        <details className="api-rules">
                          <summary>Request Rules</summary>
                          {ops
                            .filter((o) => o.body || o.headers)
                            .map((o) => (
                              <div key={o.id}>
                                <RequestRules operation={o} />
                                <MoreDetails
                                  facts={[
                                    ['Body Schema (JSON)', o.body ? JSON.stringify(o.body.schema) : undefined],
                                    ['Header Schema (JSON)', o.headers ? JSON.stringify(o.headers) : undefined],
                                  ]}
                                />
                              </div>
                            ))}
                        </details>
                      ) : null}
                    </div>
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
