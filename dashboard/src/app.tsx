import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { adminClient, request } from './http'
import type { Reply, SessionInfo, Status } from './api'
import { Outcomes } from './hooks'
import { useOutcomeRegister } from './outcomes'
import { Problem } from './replies'
import { time } from './time'
import { Brand } from './state'
import { SignIn } from './signin'
import { Shell } from './shell'

// The Dashboard's root: it resumes or starts a session, then shows sign-in or the signed-in shell.
export function App() {
  const [session, setSession] = useState<SessionInfo | null>(null)
  const [resumed, setResumed] = useState<Reply<SessionInfo> | null>(null)
  const [notice, setNotice] = useState('')
  // epoch changes on every sign-in and sign-out, which remounts the views and
  // so aborts their pending reads: nothing from an ended session can appear.
  const [epoch, setEpoch] = useState(0)
  // Changes whose answer was lost outlive an ended session, so the same
  // administrator signing in again still sees them; signing out clears them.
  const outcomes = useOutcomeRegister(session?.name ?? null)
  const clearOutcomes = outcomes.clear
  // lastStatus is the gateway's status as the shell last read it, so the
  // sign-in page can say that a trial goes on when a session ends.
  const lastStatus = useRef<Status | undefined>(undefined)

  const signedOut = useCallback(
    (message: string, explicit = false) => {
      const facts = lastStatus.current
      const trial =
        !explicit && facts?.provisional !== undefined && facts.deadline
          ? ` Version ${facts.provisional} was on trial when this page last read the gateway: unless someone keeps it, the gateway undoes it and returns to version ${facts.confirmed} at ${time(facts.deadline)} by itself. Sign in again to see its current state.`
          : ''
      if (explicit) clearOutcomes()
      lastStatus.current = undefined
      setSession(null)
      setNotice(message + trial)
      setEpoch((e) => e + 1)
      window.history.replaceState(null, '', window.location.pathname)
    },
    [clearOutcomes],
  )

  const signedIn = useCallback((info: SessionInfo) => {
    setNotice('')
    setSession(info)
    setEpoch((e) => e + 1)
  }, [])

  const admin = useMemo(
    () => (session ? adminClient(session.csrf_token, () => signedOut('Your session has ended. Sign in again.')) : null),
    [session, signedOut],
  )

  // resume asks the gateway whether this browser still holds a live session;
  // the browser cannot read the HttpOnly cookie itself.
  const resume = useCallback(async () => {
    setResumed(null)
    const reply = await request<SessionInfo>('GET', '/admin/session')
    setResumed(reply)
    if (reply.kind === 'ok') {
      signedIn(reply.body)
    } else {
      setSession(null)
    }
  }, [signedIn])

  useEffect(() => {
    resume()
    // A page restored from the browser's back-forward cache may show a
    // session that has since ended, so it asks the gateway again.
    const onShow = (event: PageTransitionEvent) => {
      if (event.persisted) resume()
    }
    window.addEventListener('pageshow', onShow)
    return () => window.removeEventListener('pageshow', onShow)
  }, [resume])

  if (session && admin) {
    return (
      <Outcomes.Provider value={outcomes}>
        <Shell key={epoch} admin={admin} session={session} signedOut={signedOut} onStatus={(facts) => (lastStatus.current = facts)} />
      </Outcomes.Provider>
    )
  }

  return (
    <div className="entry">
      <a className="visually-hidden" href="#content">
        Skip to Content
      </a>
      <main id="content" className="entry-main" tabIndex={-1}>
        <Brand />
        <p className="entry-intro">
          Policy administration for the gateway. The gateway decides every sign-in and every change; this page holds no authority of its own.
        </p>
        {notice ? (
          <p role="status" className="notice tone-pending entry-notice">
            {notice}
          </p>
        ) : null}
        {resumed === null ? (
          <p role="status" className="hint">
            Checking for a session…
          </p>
        ) : resumed.kind === 'unknown' ? (
          <section className="entry-card">
            <Problem reply={resumed} />
            <button type="button" className="btn" onClick={resume}>
              Try Again
            </button>
          </section>
        ) : (
          <SignIn key={epoch} signedIn={signedIn} />
        )}
      </main>
    </div>
  )
}
