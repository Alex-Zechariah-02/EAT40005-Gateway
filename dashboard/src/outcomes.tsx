import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { committedAnyway, describe, explain, type Admin, type HistoryEntry, type MutationRequest, type Reply } from './api'
import { useGatewayState, type OutcomeRegister, type Unresolved } from './hooks'
import { Glyph, clock } from './ui'

// transitionOf names the transition the gateway records a keyed request
// under: the administrator's name and the request key.
function transitionOf(admin: string, request: MutationRequest): string {
  return `admin:${admin}:${request.key}`
}

// settlement says what a definite answer to a tracked change means.
function settlement(reply: Reply<unknown>): Unresolved['settled'] {
  if (reply.kind === 'ok') return { tone: 'ok', text: 'Applied: the gateway answered with its receipt.' }
  if (reply.kind !== 'refused') return undefined
  const code = reply.outcome?.reason ?? reply.error
  if (committedAnyway(reply)) return { tone: 'pending', text: `Applied, with a problem (${code}): ${explain(code)}` }
  return { tone: 'refused', text: `Not applied: refused with ${code}. ${explain(code)}` }
}

// useOutcomeRegister keeps the changes whose answer was lost for the signed-in
// administrator, in this page's memory only. A new sign-in by the same
// administrator, after the session ended, still sees them; signing out
// clears them.
export function useOutcomeRegister(admin: string | null): OutcomeRegister & { clear: () => void } {
  const [items, setItems] = useState<Unresolved[]>([])
  const add = useCallback(
    (request: MutationRequest) => {
      if (!admin) return
      const id = transitionOf(admin, request)
      setItems((list) => [
        ...list.filter((item) => item.id !== id),
        { id, admin, label: describe(request), request, sentAt: new Date() },
      ])
    },
    [admin],
  )
  const update = useCallback((id: string, change: Partial<Unresolved>) => {
    setItems((list) => list.map((item) => (item.id === id ? { ...item, ...change } : item)))
  }, [])
  const forget = useCallback((id: string) => setItems((list) => list.filter((item) => item.id !== id)), [])
  // A definite answer to a resend made in the view settles the change there,
  // where the answer is shown, so it is no longer listed.
  const settle = useCallback(
    (request: MutationRequest) => {
      if (admin) forget(transitionOf(admin, request))
    },
    [admin, forget],
  )
  const clear = useCallback(() => setItems([]), [])
  return useMemo(() => ({ items, add, settle, update, forget, clear }), [items, add, settle, update, forget, clear])
}

// UnresolvedChanges lists, at the top of every page, the administrator's
// changes whose outcome is unknown. It looks each one up in the gateway's
// history by its exact transition, never by a matching current state, and
// never sends anything again by itself: resending is a deliberate choice.
export function UnresolvedChanges(props: { admin: Admin; name: string; register: OutcomeRegister; canReadHistory: boolean }) {
  const { admin, register, canReadHistory } = props
  const state = useGatewayState()
  const items = register.items.filter((item) => item.admin === props.name)
  const [busy, setBusy] = useState<string | null>(null)
  const looked = useRef(new Set<string>())

  const check = useCallback(
    async (item: Unresolved) => {
      if (!canReadHistory) {
        register.update(item.id, {
          checked: { at: new Date(), text: 'Looking it up needs the Read history permission. Sending the identical request again is safe.' },
        })
        return
      }
      setBusy(item.id)
      const reply = await admin.get<HistoryEntry>(`/admin/history/${encodeURIComponent(item.id)}`)
      setBusy(null)
      if (reply.kind === 'ok') {
        const entry = reply.body
        const at = clock(entry.recorded_at, true)
        if (entry.outcome === 'committed') {
          register.update(item.id, {
            settled: entry.audited_at
              ? { tone: 'ok', text: `Applied: the gateway recorded it at ${at}.` }
              : { tone: 'pending', text: `Applied at ${at}, but its audit record is pending, so the gateway admits nothing until it restarts.` },
          })
        } else {
          register.update(item.id, { settled: { tone: 'refused', text: `Not applied: the gateway refused it at ${at} (${entry.reason}). ${explain(entry.reason ?? '')}` } })
        }
        state.refresh()
      } else if (reply.kind === 'refused' && reply.status === 404) {
        register.update(item.id, {
          checked: {
            at: new Date(),
            text: 'The gateway has no record of it yet. It may still arrive, or it never reached the gateway; neither is proven.',
          },
        })
      } else if (reply.kind !== 'aborted') {
        const why = reply.kind === 'unknown' ? reply.message : `HTTP ${reply.status} ${reply.error}`
        register.update(item.id, { checked: { at: new Date(), text: `It could not be looked up (${why}).` } })
      }
    },
    [admin, canReadHistory, register, state],
  )

  async function resend(item: Unresolved) {
    setBusy(item.id)
    const reply = await admin.send(item.request)
    setBusy(null)
    if (reply.kind === 'unknown') {
      register.update(item.id, { checked: { at: new Date(), text: `Sent again, and again no answer came (${reply.message}).` } })
    } else if (reply.kind !== 'aborted') {
      register.update(item.id, { settled: settlement(reply) })
      state.refresh()
    }
  }

  // Each change is looked up once by itself when it is listed, including
  // after signing in again; further look-ups are the administrator's.
  useEffect(() => {
    for (const item of items) {
      if (!item.settled && !looked.current.has(item.id)) {
        looked.current.add(item.id)
        void check(item)
      }
    }
  }, [items, check])

  if (items.length === 0) return null
  const open = items.filter((item) => !item.settled).length
  return (
    <section className="banner tone-unknown unresolved" aria-labelledby="unresolved-title">
      <h2 id="unresolved-title" className="unresolved-title">
        <Glyph name="unknown" />
        {open > 0 ? `${open === 1 ? 'A change has' : `${open} changes have`} an unknown outcome` : 'Outcome found'}
      </h2>
      <ul className="unresolved-list">
        {items.map((item) => (
          <li key={item.id} className="unresolved-item">
            <p>
              <strong>{item.label}</strong> <span className="muted">sent {clock(item.sentAt.toISOString(), true)}, no answer arrived</span>
            </p>
            {item.settled ? (
              <p className={`unresolved-result tone-${item.settled.tone}`} role="status">
                {item.settled.text}
              </p>
            ) : (
              <p className="hint" role="status">
                {busy === item.id ? 'Asking the gateway…' : item.checked ? `${item.checked.text} Checked ${clock(item.checked.at.toISOString(), true)}.` : ''}
              </p>
            )}
            <div className="actions">
              {item.settled ? (
                <button type="button" className="btn btn-small" onClick={() => register.forget(item.id)}>
                  Dismiss<span className="visually-hidden"> {item.label}</span>
                </button>
              ) : (
                <>
                  <button type="button" className="btn btn-small" onClick={() => void check(item)} disabled={busy !== null || !canReadHistory}>
                    Check its outcome<span className="visually-hidden"> ({item.label})</span>
                  </button>
                  <button type="button" className="btn btn-small" onClick={() => void resend(item)} disabled={busy !== null}>
                    Send the identical request again<span className="visually-hidden"> ({item.label})</span>
                  </button>
                  <button type="button" className="btn btn-small btn-danger-quiet" onClick={() => register.forget(item.id)} disabled={busy !== null}>
                    Stop tracking<span className="visually-hidden"> {item.label}</span>
                  </button>
                </>
              )}
            </div>
            {item.settled ? null : <p className="hint">Stop tracking leaves its outcome unknown; History shows it if the gateway recorded it.</p>}
          </li>
        ))}
      </ul>
    </section>
  )
}
