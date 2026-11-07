import { useContext, useEffect, useId, useState } from 'react'
import { newKey } from '../shared/http'
import { useMutation, useResource, ViewMemory } from '../shared/hooks'
import { Badge, Field, TableScroll } from '../shared/ui'
import { Confirm } from '../shared/dialogs'
import { MutationResult, Problem } from '../shared/replies'
import { Time, Zone } from '../shared/time'
import type { ViewProps } from '../app/views'

// The Addresses view of the Accounts page: client address ranges the
// gateway refuses at once, before any other check, with no policy change.

// addressHandoff is the page memory's key for an address another page asks
// to block; the Accounts page then opens this view and its block dialog.
export const addressHandoff = 'block-address'

interface BlockedAddress {
  range: string
  reason: string
  blocked_by: string
  blocked_at: string
  expires_at: string | null
}

const expiries: [string, string, number | null][] = [
  ['never', 'Never', null],
  ['1h', '1 Hour', 60 * 60_000],
  ['24h', '24 Hours', 24 * 60 * 60_000],
  ['7d', '7 Days', 7 * 24 * 60 * 60_000],
  ['custom', 'Custom', null],
]

// asRange turns a single address into its one-address range; anything else
// is sent as typed, and the gateway decides whether it is a valid range.
export function asRange(text: string): string {
  const value = text.trim()
  if (!value || value.includes('/')) return value
  return value.includes(':') ? `${value}/128` : `${value}/32`
}

// useHandoff takes a value another page left under key in the page memory,
// when the Accounts page opens and whenever its address changes: a request
// drawer over this page hands off without the page being opened again.
export function useHandoff(key: string, take: (value: string) => void) {
  const memory = useContext(ViewMemory)
  useEffect(() => {
    const read = () => {
      const value = memory?.get(key)
      memory?.delete(key)
      if (typeof value === 'string') take(value)
    }
    read()
    window.addEventListener('hashchange', read)
    return () => window.removeEventListener('hashchange', read)
  }, [])
}

// BlockedSwitch chooses what the Accounts page lists: accounts or addresses.
export function BlockedSwitch({ view, onView }: { view: 'accounts' | 'addresses'; onView: (view: 'accounts' | 'addresses') => void }) {
  return (
    <div className="segmented page-switch" role="group" aria-label="Show Blocked">
      <button type="button" className="btn" aria-pressed={view === 'accounts'} onClick={() => onView('accounts')}>
        Accounts
      </button>
      <button type="button" className="btn" aria-pressed={view === 'addresses'} onClick={() => onView('addresses')}>
        Addresses
      </button>
    </div>
  )
}

export function AddressesPanel({ admin, can, handed }: ViewProps & { handed: { address: string } | null }) {
  const id = useId()
  const list = useResource<{ items: BlockedAddress[] }>((signal) => admin.get('/admin/blocked-addresses', signal), [])
  const block = useMutation<BlockedAddress>(admin, list.refresh)
  const unblock = useMutation<{ range: string; unblocked: boolean }>(admin, list.refresh)
  const manage = can('manage_subjects')
  const [blocking, setBlocking] = useState(false)
  const [address, setAddress] = useState('')
  const [reason, setReason] = useState('')
  const [expiry, setExpiry] = useState('never')
  const [until, setUntil] = useState('')
  const [confirming, setConfirming] = useState<string | null>(null)
  const items = list.reply?.kind === 'ok' ? list.reply.body.items : []
  const openBlock = (value = '') => {
    setAddress(value)
    setReason('')
    setExpiry('never')
    setBlocking(true)
  }
  useEffect(() => {
    if (manage && handed) openBlock(handed.address)
  }, [handed])
  const submit = () => {
    if (!address.trim() || !reason.trim()) return
    setBlocking(false)
    const ahead = expiries.find(([value]) => value === expiry)?.[2]
    const end = expiry === 'custom' && until ? new Date(until) : ahead ? new Date(Date.now() + ahead) : null
    const body = end ? { reason: reason.trim(), expires_at: end.toISOString() } : { reason: reason.trim() }
    void block.run({ method: 'PUT', path: `/admin/blocked-addresses/${encodeURIComponent(asRange(address))}`, body, key: newKey() })
  }

  return (
    <>
      {list.reply && list.reply.kind !== 'ok' ? <Problem reply={list.reply} /> : null}
      <MutationResult
        mutation={block}
        onRefresh={list.refresh}
        success={(b) => (
          <p className="notice-title">
            <code>{b.range}</code> is blocked: its requests are refused at every endpoint from now on
          </p>
        )}
      />
      <MutationResult
        mutation={unblock}
        onRefresh={list.refresh}
        success={(u) => (
          <p className="notice-title">
            <code>{u.range}</code> is unblocked; its requests are checked as usual again
          </p>
        )}
      />
      <section className="section card" aria-labelledby={`${id}-list`}>
        <div className="card-head card-head-pad accounts-head">
          <h2 id={`${id}-list`}>Blocked Addresses</h2>
          {manage ? (
            <button type="button" className="btn btn-primary btn-small" onClick={() => openBlock()}>
              Block an Address
            </button>
          ) : null}
        </div>
        <div className="card-body">
          {list.reply?.kind === 'ok' && items.length === 0 ? (
            <p className="hint card-empty">No address is blocked. Block one from here or from a request's client address.</p>
          ) : null}
          {items.length > 0 ? (
            <TableScroll label="Blocked Address List">
              <table className="accounts-table">
                <thead>
                  <tr>
                    <th scope="col">Range</th>
                    <th scope="col">Reason</th>
                    <th scope="col">Blocked By</th>
                    <th scope="col">
                      Blocked At (<Zone />)
                    </th>
                    <th scope="col">Expires</th>
                    {manage ? (
                      <th scope="col">
                        <span className="visually-hidden">Actions</span>
                      </th>
                    ) : null}
                  </tr>
                </thead>
                <tbody>
                  {items.map((entry) => (
                    <tr key={entry.range}>
                      <td>
                        <code>{entry.range}</code>
                      </td>
                      <td>{entry.reason}</td>
                      <td>{entry.blocked_by}</td>
                      <td className="nowrap muted">
                        <Time value={entry.blocked_at} compact />
                      </td>
                      <td className="nowrap">{entry.expires_at ? <Time value={entry.expires_at} compact /> : <Badge tone="refused">Never</Badge>}</td>
                      {manage ? (
                        <td className="row-actions">
                          <button type="button" className="btn btn-small" onClick={() => setConfirming(entry.range)} disabled={unblock.pending}>
                            Unblock
                            <span className="visually-hidden"> {entry.range}</span>
                          </button>
                        </td>
                      ) : null}
                    </tr>
                  ))}
                </tbody>
              </table>
            </TableScroll>
          ) : null}
        </div>
      </section>
      {manage ? (
        <Confirm
          open={blocking}
          title="Block an Address"
          confirmLabel="Block Now"
          danger
          disabled={block.pending || !address.trim() || !reason.trim() || (expiry === 'custom' && !until)}
          onCancel={() => setBlocking(false)}
          onConfirm={submit}
        >
          <p>Every request from this address or range is refused at once, at every endpoint, until it is unblocked or expires.</p>
          <Field id={`${id}-address`} label="Address or Range" hint="For example 203.0.113.7 or 203.0.113.0/24.">
            <input id={`${id}-address`} className="input mono" value={address} onChange={(e) => setAddress(e.target.value)} maxLength={43}
              spellCheck={false} autoComplete="off" aria-describedby={`${id}-address-hint`} />
          </Field>
          <Field id={`${id}-reason`} label="Reason" hint="Recorded in the history with your name.">
            <input id={`${id}-reason`} className="input" value={reason} onChange={(e) => setReason(e.target.value)} maxLength={200}
              aria-describedby={`${id}-reason-hint`} />
          </Field>
          <Field id={`${id}-expiry`} label="Expires">
            <select id={`${id}-expiry`} className="input" value={expiry} onChange={(e) => setExpiry(e.target.value)}>
              {expiries.map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </select>
          </Field>
          {expiry === 'custom' ? (
            <Field id={`${id}-until`} label="Expires At" hint="Your local time; between 1 minute and 30 days from now.">
              <input id={`${id}-until`} className="input" type="datetime-local" value={until} onChange={(e) => setUntil(e.target.value)}
                aria-describedby={`${id}-until-hint`} />
            </Field>
          ) : null}
        </Confirm>
      ) : null}
      <Confirm
        open={confirming !== null}
        title="Unblock this address?"
        confirmLabel="Unblock"
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          if (confirming) void unblock.run({ method: 'DELETE', path: `/admin/blocked-addresses/${encodeURIComponent(confirming)}`, key: newKey() })
          setConfirming(null)
        }}
      >
        <p>
          Requests from <code>{confirming}</code> are checked as usual again at once.
        </p>
      </Confirm>
    </>
  )
}
