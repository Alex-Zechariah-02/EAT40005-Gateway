import { useState } from 'react'
import { newKey } from '../shared/http'
import { useMutation, useResource } from '../shared/hooks'
import { Badge, TableScroll } from '../shared/ui'
import { MutationResult, ReplyView } from '../shared/replies'
import { PageHeader } from '../app/header'
import { ReadStamp, Time } from '../shared/time'
import type { ViewProps } from '../app/views'
import { permissionWords } from '../shared/checks'

// The Team page: administrators and trusted token signing keys.

interface Administrator {
  name: string
  permissions: string[]
  status: string
}
interface SigningKey {
  kid: string
  fingerprint: string
  loaded_at: string
  verified: number
  last_verified_at: string | null
}

// TeamView lists the gateway's administrators and the token signing keys it
// trusts. Adding an administrator stays an operator command.
export function TeamView({ admin, can }: ViewProps) {
  const admins = useResource<{ items: Administrator[] }>((signal) => admin.get('/admin/administrators', signal), [])
  const keys = useResource<{ items: SigningKey[] }>((signal) => admin.get('/admin/keys', signal), [])
  const reload = useMutation<{ items: SigningKey[] }>(admin, keys.refresh)
  // trustedBefore is the key list read before the latest reload was sent,
  // so the result can say which key IDs it added and removed.
  const [trustedBefore, setTrustedBefore] = useState<SigningKey[] | null>(null)
  return (
    <>
      <PageHeader title="Team" lede="Administrators, their permissions and the keys trusted to sign tokens.">
        <ReadStamp
          okAt={admins.last?.at ?? null} failed={admins.failed}
          loading={admins.loading}
          onRefresh={() => {
            admins.refresh()
            keys.refresh()
          }}
        />
      </PageHeader>
      <section className="section card" aria-labelledby="team-title">
        <div className="card-head card-head-pad">
          <h2 id="team-title">Administrators</h2>
          <span className="muted">
            Added by the operator with <code>gateway enrol</code>
          </span>
        </div>
        <div className="card-body">
          <ReplyView reply={admins.reply}>
            {(a) => (
              <TableScroll label="Administrators">
                <table>
                  <thead>
                    <tr>
                      <th scope="col">Name</th>
                      <th scope="col">Can</th>
                      <th scope="col">Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {a.items.map((x) => (
                      <tr key={x.name}>
                        <td>
                          <code>{x.name}</code>
                        </td>
                        <td>
                          <ul className="chips">
                            {x.permissions.map((p) => (
                              <li key={p} className="chip" title={p}>
                                {permissionWords[p] ?? p}
                              </li>
                            ))}
                          </ul>
                        </td>
                        <td>
                          <Badge tone={x.status === 'active' ? 'ok' : 'neutral'}>{x.status === 'active' ? 'Active' : x.status}</Badge>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableScroll>
            )}
          </ReplyView>
        </div>
      </section>
      <section className="section card" aria-labelledby="keys-title">
        <div className="card-head card-head-pad">
          <h2 id="keys-title">Token Signing Keys</h2>
          {can('manage_policy') ? (
            <button
              type="button"
              className="btn btn-small"
              disabled={reload.pending}
              onClick={() => {
                setTrustedBefore(keys.last?.body.items ?? null)
                void reload.run({ method: 'POST', path: '/admin/keys/reload', key: newKey() })
              }}
            >
              {reload.pending ? 'Reloading…' : 'Reload Keys from Settings'}
            </button>
          ) : null}
        </div>
        <div className="card-body">
          <ReplyView reply={keys.reply}>
            {(k) => (
              <TableScroll label="Signing Keys">
                <table>
                  <thead>
                    <tr>
                      <th scope="col">Key ID</th>
                      <th scope="col">Fingerprint</th>
                      <th scope="col">Loaded</th>
                      <th scope="col">Use Since the Gateway Started</th>
                    </tr>
                  </thead>
                  <tbody>
                    {k.items.map((x) => (
                      <tr key={x.kid}>
                        <td>
                          <code>{x.kid}</code>
                        </td>
                        <td>
                          <code className="soft">{x.fingerprint}</code>
                        </td>
                        <td className="nowrap">
                          <Time value={x.loaded_at} />
                        </td>
                        <td>
                          {x.last_verified_at ? (
                            <>
                              <Badge tone="ok">In Use</Badge> <span className="num">{x.verified.toLocaleString()}</span> tokens, last <Time value={x.last_verified_at} />
                            </>
                          ) : (
                            <span className="muted">Not used since the gateway started</span>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableScroll>
            )}
          </ReplyView>
          <div className="card-pad-x">
            <MutationResult mutation={reload} onRefresh={keys.refresh} success={(r) => <KeyChange before={trustedBefore} after={r.items} />} />
          </div>
        </div>
      </section>
      <p className="hint section">
        Signing keys are read from the gateway host, not the policy, so a policy rollback never restores a retired key. Before retiring a key, check it is no longer in use: tokens it signed are refused once it is retired.
      </p>
    </>
  )
}

// KeyChange says what a key reload changed, by key ID and fingerprint only:
// the Dashboard never sees key material.
function KeyChange({ before, after }: { before: SigningKey[] | null; after: SigningKey[] }) {
  const trusted = `${after.length} key${after.length === 1 ? '' : 's'} trusted`
  if (!before) return <p className="notice-title">Reloaded: {trusted}.</p>
  const known = (list: SigningKey[], key: SigningKey) => list.some((k) => k.kid === key.kid && k.fingerprint === key.fingerprint)
  const added = after.filter((key) => !known(before, key))
  const removed = before.filter((key) => !known(after, key))
  return (
    <>
      <p className="notice-title">Reloaded: {trusted}.</p>
      <p>
        {added.length === 0 && removed.length === 0 ? 'The same keys as before: nothing changed.' : null}
        {added.length > 0 ? `Now also trusted: ${added.map((k) => k.kid).join(', ')}. ` : null}
        {removed.length > 0 ? `No longer trusted: ${removed.map((k) => k.kid).join(', ')}; tokens signed with them are refused from the next request.` : null}
      </p>
    </>
  )
}
