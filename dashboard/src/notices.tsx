import { useEffect, useRef, useState, type CSSProperties } from 'react'
import type { Status } from './api'
import { Glyph, time } from './ui'
import type { View } from './views'

type NoticeTone = 'ok' | 'pending' | 'neutral' | 'refused'

export interface Notice {
  tone: NoticeTone
  text: string
}

interface Shown extends Notice {
  id: number
  // view is the page the notice was shown on.
  view: View
}

const noticeMs = 8000

const words: Record<NoticeTone, string> = { ok: 'Done', pending: 'In progress', neutral: 'Ended', refused: 'Problem' }

// statusNotices names what changed between two status reads: a trial that
// started or ended, temporary rules added or ended, signing keys reloaded,
// and a security log that stopped working. It only reports what the gateway
// said; it decides nothing.
export function statusNotices(before: Status, after: Status): Notice[] {
  const notices: Notice[] = []
  if (before.provisional === undefined && after.provisional !== undefined) {
    notices.push({ tone: 'pending', text: `Version ${after.provisional} is on trial${after.deadline ? ` until ${time(after.deadline)}` : ''}.` })
  }
  if (before.provisional !== undefined && after.provisional === undefined && after.confirmed !== undefined) {
    notices.push(
      after.confirmed === before.provisional
        ? { tone: 'ok', text: `Version ${before.provisional} was kept: it is the active policy.` }
        : { tone: 'neutral', text: `The trial of version ${before.provisional} ended: version ${after.confirmed} is active again.` },
    )
  }
  const earlier = before.overrides ?? []
  const later = after.overrides ?? []
  for (const id of later) if (!earlier.includes(id)) notices.push({ tone: 'pending', text: `Temporary rule ${id} is active.` })
  for (const id of earlier) if (!later.includes(id)) notices.push({ tone: 'neutral', text: `Temporary rule ${id} ended.` })
  if (before.jwt_key_sha256 && after.jwt_key_sha256 && before.jwt_key_sha256 !== after.jwt_key_sha256) {
    notices.push({ tone: 'neutral', text: 'The signing keys were reloaded.' })
  }
  if (!before.audit_failed && after.audit_failed) {
    notices.push({ tone: 'refused', text: 'The security log cannot be written, so every protected request is refused.' })
  }
  return notices
}

// Notifications is the stack of recent events floating over every page,
// announced without moving focus. It reads nothing itself: it compares the
// status reads the Dashboard already makes. A notice leaves by itself after
// noticeMs (the countdown pauses while pointed at or focused) or when
// another page opens; a problem stays on every page until it is dismissed.
export function Notifications(props: { status: Status | undefined; view: View }) {
  const [shown, setShown] = useState<Shown[]>([])
  const next = useRef(0)
  const previous = useRef<Status | undefined>(undefined)

  useEffect(() => {
    const before = previous.current
    previous.current = props.status
    if (!before || !props.status) return
    const fresh = statusNotices(before, props.status).map((notice) => ({ ...notice, id: ++next.current, view: props.view }))
    if (fresh.length > 0) setShown((list) => [...list, ...fresh].slice(-5))
  }, [props.status])

  // Opening another page forgets the earlier page's notices; the render below
  // already hides them, so the page never moves after it appears.
  useEffect(() => {
    setShown((list) => list.filter((notice) => notice.tone === 'refused' || notice.view === props.view))
  }, [props.view])
  const visible = shown.filter((notice) => notice.tone === 'refused' || notice.view === props.view)

  return (
    <div className="notices" role="status" aria-live="polite">
      {visible.map((notice) => (
        <p key={notice.id} className={`notice notice-bar tone-${notice.tone}`} style={{ '--toast-ms': `${noticeMs}ms` } as CSSProperties}>
          <Glyph name={notice.tone} />
          <span className="visually-hidden">{words[notice.tone]}: </span>
          <span>{notice.text}</span>
          <button type="button" className="btn btn-small" aria-label={`Dismiss: ${notice.text}`} onClick={() => setShown((list) => list.filter((item) => item.id !== notice.id))}>
            Dismiss
          </button>
          {notice.tone === 'refused' ? null : (
            <span className="notice-timer" aria-hidden="true" onAnimationEnd={() => setShown((list) => list.filter((item) => item.id !== notice.id))} />
          )}
        </p>
      ))}
    </div>
  )
}
