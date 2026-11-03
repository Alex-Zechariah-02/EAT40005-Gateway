import { useId, useState } from 'react'
import { newKey } from './http'
import type { Page } from './api'
import { useMutation, useResource } from './hooks'
import { AccountLink, XRef } from './links'
import { Badge, Field, ListToolbar, matches, TableScroll } from './ui'
import { Confirm } from './dialogs'
import { MutationResult, Problem } from './replies'
import { PageHeader } from './header'
import { ReadStamp, TimeOfDay } from './time'
import type { ViewProps } from './views'
import { reasonOf, type RequestEvent } from './requests'

// The Accounts page: blocked accounts and accounts seen in traffic.

interface BlockedAccount {
  subject: string
  reason: string
  blocked_by: string
  blocked_at: string
}
// AccountsView blocks and unblocks accounts instantly. A block is not a
// policy change: no trial, no rollback, and it lasts until an explicit unblock.
// Accounts recently seen in traffic, with the roles their tokens carried, are
// listed so an administrator can act on what is happening.
export function AccountsView({ admin, can }: ViewProps) {
  const id = useId()
  const list = useResource<{ items: BlockedAccount[] }>((signal) => admin.get('/admin/blocked-accounts', signal), [])
  const [query, setQuery] = useState('')
  const [show, setShow] = useState<'all' | 'blocked' | 'seen'>('all')
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?limit=100', signal), [])
  const refresh = () => {
    list.refresh()
    recent.refresh()
  }
  const block = useMutation<BlockedAccount>(admin, list.refresh)
  const unblock = useMutation<{ subject: string; unblocked: boolean }>(admin, list.refresh)
  const [blocking, setBlocking] = useState(false)
  const [subject, setSubject] = useState('')
  const [reason, setReason] = useState('')
  const [confirming, setConfirming] = useState<string | null>(null)
  const manage = can('manage_subjects')
  const blockedList = list.reply?.kind === 'ok' ? list.reply.body.items : []
  const blocked = new Map(blockedList.map((b) => [b.subject, b]))
  const openBlock = (account = '') => {
    setSubject(account)
    setReason('')
    setBlocking(true)
  }
  const submit = () => {
    if (!subject.trim() || !reason.trim()) return
    setBlocking(false)
    void block.run({ method: 'PUT', path: `/admin/blocked-accounts/${encodeURIComponent(subject.trim())}`, body: { reason: reason.trim() }, key: newKey() })
  }

  // Every account the gateway knows about in one list: the blocked ones
  // first, newest block first, then the accounts seen in the latest
  // requests, newest first, with the roles and outcome of their last request.
  const seen = new Map<string, RequestEvent>()
  if (recent.reply?.kind === 'ok') for (const e of recent.reply.body.items) if (e.subject && !seen.has(e.subject)) seen.set(e.subject, e)
  const accounts = [
    ...[...blockedList].sort((a, b) => b.blocked_at.localeCompare(a.blocked_at)).map((b) => b.subject),
    ...[...seen.keys()].filter((account) => !blocked.has(account)),
  ]
  const counts = { all: accounts.length, blocked: blocked.size, seen: seen.size }
  const shown = accounts.filter((account) => {
    if (show === 'blocked' && !blocked.has(account)) return false
    if (show === 'seen' && !seen.has(account)) return false
    const b = blocked.get(account)
    const e = seen.get(account)
    return matches(query, account, b?.reason, b?.blocked_by, ...(e?.roles ?? []), e ? `${e.method} ${e.path}` : undefined)
  })
  const filters: ['all' | 'blocked' | 'seen', string][] = [
    ['all', 'All'],
    ['blocked', 'Blocked'],
    ['seen', 'Recently Seen'],
  ]

  return (
    <>
      <PageHeader title="Accounts" lede="Block or unblock accounts; a block applies at every endpoint at once.">
        <ReadStamp okAt={list.last?.at ?? null} failed={list.failed} loading={list.loading} onRefresh={refresh} />
        {manage ? (
          <button type="button" className="btn btn-primary btn-small" onClick={() => openBlock()}>
            Block an Account
          </button>
        ) : null}
      </PageHeader>
      {list.reply && list.reply.kind !== 'ok' ? <Problem reply={list.reply} /> : null}
      {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
      <MutationResult
        mutation={block}
        onRefresh={list.refresh}
        success={(b) => (
          <p className="notice-title">
            <code>{b.subject}</code> is blocked: its requests are refused from now on
          </p>
        )}
      />
      <MutationResult
        mutation={unblock}
        onRefresh={list.refresh}
        success={(u) => (
          <>
            <p className="notice-title">
              <code>{u.subject}</code> is unblocked and can use Fetch again
            </p>
            <p>Access is again limited to what its token roles and the policy allow.</p>
          </>
        )}
      />
      <section className="section card" aria-labelledby={`${id}-list`}>
        <h2 id={`${id}-list`} className="visually-hidden">
          Accounts
        </h2>
        <div className="card-head card-head-pad accounts-head">
          <div className="segmented" role="group" aria-label="Show">
            {filters.map(([value, label]) => (
              <button key={value} type="button" className="btn" aria-pressed={show === value} onClick={() => setShow(value)}>
                {label} <span className="segmented-count">{counts[value]}</span>
              </button>
            ))}
          </div>
          <ListToolbar label="Filter accounts" query={query} onQuery={setQuery} shown={shown.length} total={accounts.length} noun={['account', 'accounts']} />
        </div>
        <div className="card-body">
          {list.reply?.kind === 'ok' && recent.reply?.kind === 'ok' && accounts.length === 0 ? (
            <p className="hint card-empty">No account is blocked, and no signed-in request was seen recently.</p>
          ) : accounts.length > 0 && shown.length === 0 ? (
            <p className="hint card-empty">No account matches.</p>
          ) : null}
          {shown.length > 0 ? (
            <TableScroll label="Account List">
              <table className="accounts-table">
                <thead>
                  <tr>
                    <th scope="col">Account</th>
                    <th scope="col">Status</th>
                    <th scope="col">Roles</th>
                    <th scope="col">Last Request</th>
                    <th scope="col">Seen</th>
                    {manage ? (
                      <th scope="col">
                        <span className="visually-hidden">Actions</span>
                      </th>
                    ) : null}
                  </tr>
                </thead>
                <tbody>
                  {shown.map((account) => {
                    const b = blocked.get(account)
                    const e = seen.get(account)
                    return (
                      <tr key={account}>
                        <td>
                          <AccountLink account={account}>
                            <code>{account}</code>
                          </AccountLink>
                          {b ? (
                            <span className="cell-sub">
                              {b.reason} · by {b.blocked_by} · <TimeOfDay value={b.blocked_at} />
                            </span>
                          ) : null}
                        </td>
                        <td>{b ? <Badge tone="refused">Blocked</Badge> : <Badge tone="ok">Active</Badge>}</td>
                        <td>
                          {e ? (
                            e.roles.length ? (
                              <span className="chips">
                                {e.roles.map((role) => (
                                  <XRef key={role} view="versions" focus={{ role }}>
                                    <span className="chip">{role}</span>
                                  </XRef>
                                ))}
                              </span>
                            ) : (
                              <span className="muted">None the policy defines</span>
                            )
                          ) : (
                            <span className="muted">—</span>
                          )}
                        </td>
                        <td>
                          {e ? (
                            <>
                              <code className="soft">
                                <span className="method">{e.method}</span> {e.path}
                              </code>
                              <span className={`cell-sub ${e.decision === 'allowed' ? '' : 'refused-word'}`}>
                                {e.decision === 'allowed' ? 'Allowed' : reasonOf(e.code)}
                              </span>
                            </>
                          ) : (
                            <span className="muted">Not in the latest 100 requests</span>
                          )}
                        </td>
                        <td className="nowrap muted">{e ? <TimeOfDay value={e.time} seconds /> : '—'}</td>
                        {manage ? (
                          <td className="row-actions">
                            {b ? (
                              <button type="button" className="btn btn-small" onClick={() => setConfirming(account)} disabled={unblock.pending}>
                                Unblock
                                <span className="visually-hidden"> {account}</span>
                              </button>
                            ) : (
                              <button type="button" className="btn btn-small" onClick={() => openBlock(account)}>
                                Block…
                                <span className="visually-hidden"> {account}</span>
                              </button>
                            )}
                          </td>
                        ) : null}
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </TableScroll>
          ) : null}
        </div>
      </section>
      {manage ? (
        <Confirm
          open={blocking}
          title="Block an Account"
          confirmLabel="Block Now"
          danger
          disabled={block.pending || !subject.trim() || !reason.trim()}
          onCancel={() => setBlocking(false)}
          onConfirm={submit}
        >
          <p>Every request from this account is refused at once, at every endpoint, until it is unblocked.</p>
          <Field id={`${id}-subject`} label="Account ID" hint="As shown in requests, for example cust-002.">
            <input
              id={`${id}-subject`}
              className="input mono"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              maxLength={128}
              spellCheck={false}
              autoComplete="off"
              aria-describedby={`${id}-subject-hint`}
            />
          </Field>
          <Field id={`${id}-reason`} label="Reason" hint="Recorded in the history with your name.">
            <input
              id={`${id}-reason`}
              className="input"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              maxLength={200}
              aria-describedby={`${id}-reason-hint`}
            />
          </Field>
        </Confirm>
      ) : null}
      <Confirm
        open={confirming !== null}
        title="Unblock this account?"
        confirmLabel="Unblock"
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          if (confirming) void unblock.run({ method: 'DELETE', path: `/admin/blocked-accounts/${encodeURIComponent(confirming)}`, key: newKey() })
          setConfirming(null)
        }}
      >
        <p>
          <code>{confirming}</code> can use Fetch again at once, with whatever roles its token carries and the policy allows.
        </p>
      </Confirm>
    </>
  )
}
