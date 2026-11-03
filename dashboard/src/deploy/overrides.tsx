import { useState } from 'react'
import { newKey } from '../shared/http'
import type { Outcome, Override, OverridesView, Revision } from '../shared/api'
import { useGatewayState, useMutation, usePaged, useRereadWhen, useResource } from '../shared/hooks'
import { actorName, MutationResult, Problem, Receipt, ShowMore } from '../shared/replies'
import { Badge, Empty, ListToolbar, matches, TableScroll } from '../shared/ui'
import { Confirm } from '../shared/dialogs'
import { ReadStamp, Remaining, Time } from '../shared/time'
import { PageHeader } from '../app/header'
import { useFocus, useTarget, XRef } from '../shared/links'
import type { ViewProps } from '../app/views'
import { AddRule, overrideChange, OverrideKind } from './rule'

// The Temporary Rules page: the rules in force, revoking them, and their history.

const statusWords: Record<string, string> = { active: 'Active', revoked: 'Revoked', expired: 'Ended', quarantined: 'Set aside after a restore' }

const statusTones: Record<string, 'pending' | 'neutral' | 'refused'> = {
  active: 'pending',
  revoked: 'neutral',
  expired: 'neutral',
  quarantined: 'refused',
}

// Overrides lists the temporary overrides in force, creates one after the
// gateway has checked it, and revokes one. An override changes the effective
// policy only; the confirmed base stays as it is.
export function Overrides({ admin, can }: ViewProps) {
  const state = useGatewayState()
  const active = useResource<OverridesView>((signal) => admin.get('/admin/overrides', signal), [])
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const [reads, setReads] = useState(0)
  // A rule's end, revocation or a new rule made elsewhere changes the
  // gateway's status, which the shell rereads every 3 s while rules are
  // active; the rules in force, the policy and past rules are then read
  // again from the gateway, not from a local countdown. A reread this page
  // asks for goes the same way.
  useRereadWhen(state.version, () => {
    active.refresh()
    policy.refresh()
    setReads((n) => n + 1)
  })
  const refresh = state.refresh
  const mutation = useMutation(admin, refresh)
  const [revoking, setRevoking] = useState<Override | null>(null)
  // A link from another page, such as the history, names one rule: its row
  // is brought into view and highlighted.
  const linkedRule = useFocus('rule')
  useTarget(active.reply?.kind === 'ok', linkedRule)
  const [query, setQuery] = useState('')
  const [guided, setGuided] = useState(false)

  const view = active.reply?.kind === 'ok' ? active.reply.body : undefined
  const etag = active.reply?.kind === 'ok' ? active.reply.etag : null
  const document = policy.reply?.kind === 'ok' ? policy.reply.body.document : undefined
  const manage = can('manage_policy')
  const shown = (view?.overrides ?? []).filter((o) =>
    matches(query, o.id, `rule ${o.id}`, o.operation, o.role, o.target, overrideChange(o, document)),
  )

  return (
    <>
      <PageHeader
        title="Temporary Rules"
        lede="Time-limited rules that allow or restrict access on top of the active policy."
      >
        <ReadStamp okAt={active.last?.at ?? null} failed={active.failed} loading={active.loading || policy.loading} onRefresh={refresh} />
      </PageHeader>

      <section className="section card" aria-labelledby="overrides-title">
        <div className="card-head card-head-pad">
          <div>
            <h2 id="overrides-title">Active Rules</h2>
            {view ? (
              <p className="hint">
                On version {view.revision}
                {view.provisional ? ' (on trial)' : ''}
              </p>
            ) : null}
          </div>
        </div>
        <div className="card-body">
          {active.reply && active.reply.kind !== 'ok' ? <Problem reply={active.reply} /> : null}
          {view ? (
            <>
              <ListToolbar
                label="Filter active temporary rules"
                query={query}
                onQuery={setQuery}
                shown={shown.length}
                total={view.overrides.length}
                noun={['active rule', 'active rules']}
              >
                {manage && document ? (
                  <button type="button" className="btn btn-primary" onClick={() => setGuided(true)} disabled={view.provisional !== undefined}>
                    Add a Temporary Rule
                  </button>
                ) : null}
                {manage && view.provisional !== undefined ? (
                  <span className="hint">A change is on trial: the gateway refuses new temporary rules until it is kept or undone.</span>
                ) : null}
              </ListToolbar>
              {view.overrides.length === 0 ? (
                <p className="hint card-empty">No temporary rules are active.</p>
              ) : shown.length === 0 ? (
                <p className="hint card-empty">No active rule matches the filter.</p>
              ) : (
                <TableScroll label="Active Temporary Rules">
                  <table>
                    <thead>
                      <tr>
                        <th scope="col">Rule</th>
                        <th scope="col">Kind</th>
                        <th scope="col">Change</th>
                        <th scope="col">Added</th>
                        <th scope="col">Ends</th>
                        {manage ? (
                          <th scope="col">
                            <span className="visually-hidden">Action</span>
                          </th>
                        ) : null}
                      </tr>
                    </thead>
                    <tbody>
                      {shown.map((o) => (
                        <tr key={o.id} data-target={linkedRule === String(o.id) ? 'true' : undefined}>
                          <td className="num">{o.id}</td>
                          <td>
                            <OverrideKind type={o.type} />
                          </td>
                          <td>
                            {overrideChange(o, document)}
                            <span className="cell-sub">
                              {o.operation ? (
                                <XRef view="policy" focus={{ select: `operation:${o.operation}` }}>
                                  <code>{o.target}</code>
                                </XRef>
                              ) : (
                                <code>{o.target}</code>
                              )}
                            </span>
                          </td>
                          <td>
                            <Time value={o.created_at} /> <span className="muted">by {o.actor}</span>
                          </td>
                          <td>
                            <Time value={o.expires_at} />
                            <span className="cell-sub">
                              <Remaining key={o.expires_at} until={o.expires_at} onPassed={refresh} />
                            </span>
                          </td>
                          {manage ? (
                            <td className="row-actions">
                              <button
                                type="button"
                                className="btn btn-small btn-danger-quiet"
                                onClick={() => setRevoking(o)}
                                disabled={mutation.pending}
                              >
                                Revoke
                                <span className="visually-hidden"> rule {o.id}</span>
                              </button>
                            </td>
                          ) : null}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </TableScroll>
              )}
            </>
          ) : null}
          <div className="card-pad-x">
            <MutationResult<Outcome>
              mutation={mutation}
              onRefresh={refresh}
              success={(o) => (
                <>
                  <p className="notice-title">Rule {o.override} revoked. It no longer applies.</p>
                  <Receipt outcome={o} />
                </>
              )}
            />
          </div>
        </div>
      </section>

      {manage && document && view ? (
        <AddRule
          admin={admin}
          policy={document}
          etag={etag}
          confirmed={view.confirmed}
          created={refresh}
          open={guided}
          onClose={() => setGuided(false)}
        />
      ) : null}
      {can('read_history') ? <OverrideHistory admin={admin} reads={reads} /> : null}

      <Confirm
        open={revoking !== null}
        title={`Revoke rule ${revoking?.id ?? ''}?`}
        confirmLabel={`Revoke rule ${revoking?.id ?? ''}`}
        danger
        onCancel={() => setRevoking(null)}
        onConfirm={() => {
          const o = revoking
          setRevoking(null)
          if (o) mutation.run({ method: 'DELETE', path: `/admin/overrides/${o.id}`, key: newKey() })
        }}
      >
        {revoking ? <p>{overrideChange(revoking, document)} ends now.</p> : null}
        <p>
          <code>{revoking?.target}</code> returns to the policy from the next request.
        </p>
      </Confirm>
    </>
  )
}

function OverrideHistory({ admin, reads }: { admin: ViewProps['admin']; reads: number }) {
  const linkedRule = useFocus('rule')
  const history = usePaged<Override>(admin, '/admin/overrides/history', [reads])
  return (
    <section className="section card card-pad" aria-labelledby="past-rules-title">
      <h2 id="past-rules-title">Past Rules</h2>
      {history.reply && history.reply.kind !== 'ok' ? <Problem reply={history.reply} /> : null}
      {history.reply?.kind === 'ok' && history.items.length === 0 ? <Empty title="No temporary rule has been added yet." /> : null}
      {history.items.length > 0 ? (
        <TableScroll label="Past Rules">
          <table>
            <thead>
              <tr>
                <th scope="col">Rule</th>
                <th scope="col">Kind</th>
                <th scope="col">Change</th>
                <th scope="col">Added</th>
                <th scope="col">Ends</th>
                <th scope="col">Status</th>
              </tr>
            </thead>
            <tbody>
              {history.items.map((o) => (
                <tr key={o.id} data-target={linkedRule === String(o.id) ? 'true' : undefined}>
                  <td className="num">{o.id}</td>
                  <td>
                    <OverrideKind type={o.type} />
                  </td>
                  <td>
                    {overrideChange(o)}
                    <br />
                    <code className="muted">{o.target}</code>
                  </td>
                  <td>
                    <Time value={o.created_at} /> <span className="muted">by {o.actor}</span>
                  </td>
                  <td>
                    <Time value={o.expires_at} />
                  </td>
                  <td>
                    <Badge tone={statusTones[o.status] ?? 'neutral'}>{statusWords[o.status] ?? o.status}</Badge>
                    {o.resolved_at ? (
                      <span className="cell-sub">
                        <Time value={o.resolved_at} /> by {actorName(o.resolved_by)}
                      </span>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      ) : null}
      <ShowMore list={history} />
    </section>
  )
}
