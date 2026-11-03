import { X } from '@phosphor-icons/react'
import { useContext, useEffect, useId, useRef } from 'react'
import type { Page, Reply } from '../shared/api'
import type { Admin } from '../shared/http'
import { Can, Navigate, ViewMemory, useResource } from '../shared/hooks'
import { AccountLink, useFocus, XRef } from '../shared/links'
import { Badge, CopyButton } from '../shared/ui'
import { Time } from '../shared/time'
import { Problem } from '../shared/replies'
import { keepTabInside } from '../shared/dialogs'
import { refAddress } from '../app/views'
import { blockHandoff } from '../access/blocking'
import { CheckList, decisionTone, outcomeOf, reasonOf, type RequestEvent } from './requests'

// The request drawer: one request's facts and checks over the page it was
// opened from, at that page's address plus ?ref=.

const fullRef = /^[0-9a-f]{32}$/

interface Found {
  event: RequestEvent | null
  // log says the Security Log was searched as well as the latest requests.
  log: boolean
}

const nothing: Reply<null> = { kind: 'ok', status: 200, body: null, etag: null }

// findRequest reads a request by its Ref from the latest requests and, for an
// administrator who may read it, then from the Security Log.
async function findRequest(admin: Admin, ref: string, history: boolean, signal: AbortSignal): Promise<Reply<Found>> {
  const recent = await admin.get<Page<RequestEvent>>(`/admin/events?ref=${ref}&limit=1`, signal)
  if (recent.kind !== 'ok') return recent
  const [event] = recent.body.items
  if (event || !history) return { ...recent, body: { event: event ?? null, log: false } }
  const log = await admin.get<Page<RequestEvent>>(`/admin/security-log?ref=${ref}&limit=1`, signal)
  if (log.kind !== 'ok') return log
  return { ...log, body: { event: log.body.items[0] ?? null, log: true } }
}

// versionOf reads the version and temporary rules a generation was built
// from ("r6+o1.2-…"); the generation itself stays secondary text.
function versionOf(generation: string): { version: string; rules: string[] } | null {
  const match = /^r(\d+)(?:\+o([\d.]+))?-/.exec(generation)
  return match ? { version: match[1], rules: match[2]?.split('.') ?? [] } : null
}

export function RequestDrawer({ admin }: { admin: Admin }) {
  const can = useContext(Can)
  const memory = useContext(ViewMemory)
  const navigate = useContext(Navigate)
  const titleId = useId()
  const dialog = useRef<HTMLDialogElement>(null)
  const opener = useRef<Element | null>(null)
  // Back closes a drawer opened from its page; a pasted or reloaded address
  // has no earlier entry to go back to, so Close replaces it instead.
  const pushed = useRef(false)
  const ref = useFocus('ref')?.toLowerCase() ?? null
  const valid = ref !== null && fullRef.test(ref)
  const history = can('read_history')
  const manage = can('manage_subjects')
  const found = useResource<Found | null>((signal) => (valid ? findRequest(admin, ref, history, signal) : Promise.resolve(nothing)), [ref, history])
  const blocked = useResource<{ items: { subject: string }[] } | null>(
    (signal) => (valid && manage ? admin.get('/admin/blocked-accounts', signal) : Promise.resolve(nothing)),
    [ref, manage],
  )

  useEffect(() => {
    const changed = (event: HashChangeEvent) => {
      const before = new URL(event.oldURL).hash
      const after = new URL(event.newURL).hash
      if (!new URLSearchParams(after.split('?')[1] ?? '').has('ref')) return
      pushed.current = !new URLSearchParams(before.split('?')[1] ?? '').has('ref') && refAddress(null, before) === refAddress(null, after)
    }
    window.addEventListener('hashchange', changed)
    return () => window.removeEventListener('hashchange', changed)
  }, [])

  const open = ref !== null
  useEffect(() => {
    const element = dialog.current
    if (!element) return
    if (open && !element.open) {
      opener.current = document.activeElement
      element.showModal()
    }
    if (!open && element.open) {
      element.close()
      // Focus returns to the row or search that opened the drawer, if it is still there.
      const back = opener.current
      opener.current = null
      if (back instanceof HTMLElement && back.isConnected) back.focus()
    }
  }, [open])

  const close = () => {
    if (pushed.current) {
      pushed.current = false
      window.history.back()
    } else window.location.replace(refAddress(null))
  }

  const answer = found.reply
  const event = answer?.kind === 'ok' ? answer.body?.event : undefined
  const blockedList = blocked.reply?.kind === 'ok' ? blocked.reply.body?.items : undefined
  const isBlocked = !!event?.subject && !!blockedList?.some((b) => b.subject === event.subject)
  const title = event ? `${event.method} ${event.path}` : !valid ? 'Not a Full Ref' : answer?.kind === 'ok' && answer.body ? 'No Request with This Ref' : 'Request'

  return (
    <dialog
      ref={dialog}
      className="drawer"
      aria-labelledby={titleId}
      onKeyDown={keepTabInside}
      onCancel={(e) => {
        e.preventDefault()
        close()
      }}
      onClick={(e) => {
        // A click on the backdrop beside the drawer closes it.
        if (e.target === e.currentTarget) close()
      }}
    >
      {open ? (
        <div className="drawer-panel">
          <div className="drawer-head">
            <h2 id={titleId} className={event ? 'drawer-request' : undefined}>
              {event ? <code>{title}</code> : title}
            </h2>
            <button type="button" className="btn btn-small" onClick={close}>
              <X size={14} aria-hidden="true" /> Close
            </button>
          </div>
          <div className="drawer-body">
            {!valid ? (
              <p>
                <code>{ref}</code> is not a full Ref. A full Ref is 32 letters and digits, as shown in an error message or in a
                request list.
              </p>
            ) : answer && answer.kind !== 'ok' ? (
              <Problem reply={answer} />
            ) : !answer || found.loading ? (
              <p role="status" className="hint">
                Reading the request…
              </p>
            ) : event ? (
              <RequestFacts
                event={event}
                blocked={isBlocked}
                block={
                  manage && event.subject && blockedList && !isBlocked
                    ? () => {
                        memory?.set(blockHandoff, event.subject)
                        navigate?.('accounts')
                      }
                    : undefined
                }
              />
            ) : (
              <>
                <p>
                  No request has the Ref <code>{ref}</code>.
                </p>
                <p className="hint">
                  {answer.body?.log
                    ? 'Searched the latest 10,000 requests since the gateway started, then the Security Log files the gateway still holds. Files moved away from the gateway are not searched.'
                    : 'Only the latest 10,000 requests since the gateway started are searched. Older requests are in the Security Log, which needs the View History permission.'}
                </p>
              </>
            )}
          </div>
        </div>
      ) : null}
    </dialog>
  )
}

// RequestFacts is what the gateway recorded about one request, each fact
// linked to the page where it lives.
function RequestFacts({ event, blocked, block }: { event: RequestEvent; blocked: boolean; block?: () => void }) {
  const policy = versionOf(event.generation)
  return (
    <>
      <p className="drawer-outcome">
        <Badge tone={decisionTone[event.decision]}>{outcomeOf(event)}</Badge>
        <span>{event.decision === 'allowed' ? 'Forwarded to the service' : reasonOf(event.code)}</span>
      </p>
      <dl className="facts drawer-facts">
        <div className="fact">
          <dt>Time</dt>
          <dd>
            <Time value={event.time} />
          </dd>
        </div>
        <div className="fact">
          <dt>Method and Path</dt>
          <dd>
            {event.operation ? (
              <XRef view="policy" focus={{ select: `operation:${event.operation}` }}>
                <code>
                  {event.method} {event.path}
                </code>
              </XRef>
            ) : (
              <code>
                {event.method} {event.path}
              </code>
            )}
            <span className="cell-sub">{event.operation ? <>Endpoint <code>{event.operation}</code></> : 'No endpoint matched'}</span>
          </dd>
        </div>
        <div className="fact">
          <dt>Response</dt>
          <dd>
            {outcomeOf(event)} <span className="muted">in {event.duration_ms.toFixed(1)} ms</span>
          </dd>
        </div>
        <div className="fact">
          <dt>Policy Version</dt>
          <dd>
            {policy ? (
              <>
                <XRef view="versions" focus={{ version: policy.version }}>
                  Version {policy.version}
                </XRef>
                {policy.rules.length ? ` with temporary rule${policy.rules.length > 1 ? 's' : ''} ${policy.rules.join(', ')}` : ''}
                <code className="cell-sub soft">{event.generation}</code>
              </>
            ) : (
              'None (no active policy)'
            )}
          </dd>
        </div>
        <div className="fact">
          <dt>Account</dt>
          <dd>
            {event.subject ? (
              <>
                <AccountLink account={event.subject}>
                  <code>{event.subject}</code>
                </AccountLink>
                {event.roles.length ? <span className="muted"> as {event.roles.join(', ')}</span> : null}
                {blocked ? <> <Badge tone="refused">Blocked</Badge></> : null}
                {block ? (
                  <button type="button" className="btn btn-small drawer-block" onClick={block}>
                    Block Account…
                  </button>
                ) : null}
              </>
            ) : (
              'None (no token)'
            )}
          </dd>
        </div>
        <div className="fact">
          <dt>Client Address</dt>
          <dd>
            <code>{event.client_address}</code>
          </dd>
        </div>
      </dl>
      <h3 className="drawer-section">Checks</h3>
      <CheckList event={event} />
      <dl className="facts drawer-facts">
        <div className="fact">
          <dt>Reason</dt>
          <dd>
            {event.code ? (
              <>
                {reasonOf(event.code)} <code className="soft">{event.code}</code>
              </>
            ) : (
              'None: every check passed'
            )}
          </dd>
        </div>
        <div className="fact">
          <dt>Ref</dt>
          <dd className="drawer-ref">
            <code>{event.request_id}</code>
            <CopyButton value={event.request_id} label="Ref" />
          </dd>
        </div>
      </dl>
      <p className="hint">Tokens, passwords and request bodies are never logged.</p>
    </>
  )
}
