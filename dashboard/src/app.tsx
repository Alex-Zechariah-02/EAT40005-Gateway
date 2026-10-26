import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { SignOut } from '@phosphor-icons/react'
import { adminClient, request, type Admin, type Permission, type Reply, type SessionInfo, type Status } from './api'
import { GatewayState, useResource } from './hooks'
import { Icon, type IconName } from './icons'
import { Problem } from './ui'
import { admissionText, Brand } from './state'
import { SignIn } from './signin'
import { Overview } from './overview'
import { PolicyView } from './policy'
import { Subjects } from './subjects'
import { Candidates } from './candidates'
import { Overrides } from './overrides'
import { History } from './history'
import { Account } from './account'
import { AccountsView, ApisView, LiveTraffic, PolicyCode, RequestLookup, TeamView } from './monitor'
import type { DesignerDraft } from './designer-model'
import { setTheme, useTheme, type ThemeChoice } from './theme'
import type { Handoff, View } from './views'

interface NavItem {
  id: View
  label: string
  icon: IconName
  needs: Permission
}

const Designer = lazy(() => import('./designer').then((module) => ({ default: module.Designer })))

// The views in three groups by job: watch the gateway, change its
// policy, administer it.
const navigation: { label: string; items: NavItem[] }[] = [
  {
    label: 'Monitor',
    items: [
      { id: 'overview', label: 'Overview', icon: 'overview', needs: 'read' },
      { id: 'traffic', label: 'Live traffic', icon: 'traffic', needs: 'read' },
      { id: 'lookup', label: 'Request lookup', icon: 'lookup', needs: 'read' },
      { id: 'apis', label: 'APIs', icon: 'apis', needs: 'read' },
    ],
  },
  {
    label: 'Policy',
    items: [
      { id: 'designer', label: 'Policy', icon: 'permit', needs: 'read' },
      { id: 'candidates', label: 'Changes', icon: 'candidates', needs: 'read' },
      { id: 'overrides', label: 'Temporary rules', icon: 'overrides', needs: 'read' },
      { id: 'accounts', label: 'Accounts', icon: 'accounts', needs: 'read' },
    ],
  },
  {
    label: 'Admin',
    items: [
      { id: 'history', label: 'History', icon: 'history', needs: 'read_history' },
      { id: 'code', label: 'Policy as code', icon: 'code', needs: 'read' },
      { id: 'team', label: 'Team', icon: 'team', needs: 'read' },
      { id: 'account', label: 'My account', icon: 'account', needs: 'read' },
    ],
  },
]

const titles: Record<View, string> = {
  overview: 'Overview',
  traffic: 'Live traffic',
  lookup: 'Request lookup',
  apis: 'APIs',
  designer: 'Policy',
  policy: 'Policy revisions',
  candidates: 'Changes',
  provisional: 'Changes',
  overrides: 'Temporary rules',
  accounts: 'Accounts',
  subjects: 'Subjects',
  history: 'History',
  code: 'Policy as code',
  team: 'Team',
  account: 'My account',
}

// Views reached from another page rather than the navigation light up the
// navigation item they belong to.
const section: Partial<Record<View, View>> = { policy: 'designer', provisional: 'candidates', subjects: 'accounts' }

// The open view is kept in the address fragment, so a reload or the
// browser's back button returns to it. The fragment is never sent to the
// server; a view may keep its search there (#/lookup?ref=...).
function viewFromHash(): View {
  const id = window.location.hash.replace(/^#\/?/, '').split('?')[0]
  if (id === 'provisional') return 'candidates'
  return id in titles ? (id as View) : 'overview'
}

export function App() {
  const [session, setSession] = useState<SessionInfo | null>(null)
  const [resumed, setResumed] = useState<Reply<SessionInfo> | null>(null)
  const [notice, setNotice] = useState('')
  // epoch changes on every sign-in and sign-out, which remounts the views and
  // so aborts their pending reads: nothing from an ended session can appear.
  const [epoch, setEpoch] = useState(0)

  const signedOut = useCallback((message: string) => {
    setSession(null)
    setNotice(message)
    setEpoch((e) => e + 1)
    window.history.replaceState(null, '', window.location.pathname)
  }, [])

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
    return <Shell key={epoch} admin={admin} session={session} signedOut={signedOut} />
  }

  return (
    <div className="entry">
      <a className="visually-hidden" href="#content">
        Skip to content
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
              Try again
            </button>
          </section>
        ) : (
          <SignIn key={epoch} signedIn={signedIn} />
        )}
      </main>
    </div>
  )
}

const themeChoices: { value: ThemeChoice; label: string }[] = [
  { value: 'system', label: 'Auto' },
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
]

function ThemeSwitch() {
  const choice = useTheme()
  return (
    <div className="theme-switch" role="group" aria-label="Appearance">
      {themeChoices.map((option) => (
        <button key={option.value} type="button" aria-pressed={choice === option.value} onClick={() => setTheme(option.value)} title={option.value === 'system' ? 'Match this computer' : undefined}>
          {option.label}
        </button>
      ))}
    </div>
  )
}

// accessWord sums up an administrator's permissions in two words.
function accessWord(permissions: Permission[]): string {
  if (permissions.includes('manage_policy') && permissions.includes('manage_subjects') && permissions.includes('read_history')) return 'Full access'
  if (permissions.includes('manage_policy') || permissions.includes('manage_subjects')) return 'Can make changes'
  return 'Read only'
}

function Shell(props: { admin: Admin; session: SessionInfo; signedOut: (message: string) => void }) {
  const { admin, session, signedOut } = props
  const [view, setView] = useState<View>(viewFromHash)
  const [draft, setDraft] = useState<string | undefined>(undefined)
  const [candidate, setCandidate] = useState<number | undefined>(undefined)
  const [designerDraft, setDesignerDraft] = useState<DesignerDraft | null>(null)
  const [menuOpen, setMenuOpen] = useState(false)
  // pendingDraft carries a draft from open() to the view change it causes;
  // any other change of view, such as the back button, opens no draft.
  const pendingDraft = useRef<Handoff | undefined>(undefined)
  const firstView = useRef(true)

  const status = useResource<Status>((signal) => admin.get('/admin/status', signal), [view])
  const can = (permission: Permission) => session.permissions.includes(permission)

  useEffect(() => {
    const onHash = () => {
      setDraft(pendingDraft.current?.draft)
      setCandidate(pendingDraft.current?.candidate)
      pendingDraft.current = undefined
      setView(viewFromHash())
      setMenuOpen(false)
    }
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  // After a change of view, focus moves to its heading, so keyboard and
  // screen reader users start at the top of the new view.
  useEffect(() => {
    document.title = `${titles[view]} – Fetch Gateway`
    if (firstView.current) {
      firstView.current = false
      return
    }
    document.getElementById('page-title')?.focus()
  }, [view])

  function open(next: View, handoff?: Handoff) {
    pendingDraft.current = handoff
    if (next === view) {
      setDraft(handoff?.draft)
      setCandidate(handoff?.candidate)
      pendingDraft.current = undefined
    }
    window.location.hash = `#/${next}`
  }

  async function signOut() {
    const reply = await admin.send({ method: 'DELETE', path: '/admin/session' })
    if (reply.kind === 'ok') {
      signedOut('Signed out.')
    } else if (reply.kind === 'unknown') {
      signedOut('Signed out of this page, but the gateway did not confirm it; the session ends at its idle timeout.')
    }
  }

  const facts = status.reply?.kind === 'ok' ? status.reply.body : undefined
  const current = section[view] ?? view
  const viewProps = { admin, can, open }

  return (
    <GatewayState.Provider value={status}>
      <a className="visually-hidden" href="#content">
        Skip to content
      </a>
      <div className="shell">
        {/* On a narrow screen the views and the account move behind the menu
            button. */}
        <aside className="sidebar" data-open={menuOpen}>
          <div className="sidebar-head">
            <Brand />
            <button
              type="button"
              className="btn menu-toggle"
              aria-expanded={menuOpen}
              aria-controls="sidebar-nav sidebar-who"
              onClick={() => setMenuOpen((o) => !o)}
            >
              Menu
            </button>
          </div>
          <nav className="sidebar-nav" id="sidebar-nav" aria-label="Administration">
            {navigation.map((group) => {
              const items = group.items.filter((item) => can(item.needs))
              if (items.length === 0) return null
              return (
                <div className="nav-group" key={group.label}>
                  <p className="nav-group-label">{group.label}</p>
                  <ul>
                    {items.map((item) => (
                      <li key={item.id}>
                        <a href={`#/${item.id}`} aria-current={current === item.id ? 'page' : undefined}>
                          <Icon name={item.icon} />
                          <span className="nav-label">{item.label}</span>
                          {item.id === 'candidates' && facts?.provisional !== undefined ? <span className="nav-flag tone-pending">On trial</span> : null}
                          {item.id === 'overrides' && facts?.overrides && facts.overrides.length > 0 ? (
                            <span className="nav-count" aria-label={`${facts.overrides.length} active`}>
                              {facts.overrides.length}
                            </span>
                          ) : null}
                        </a>
                      </li>
                    ))}
                  </ul>
                </div>
              )
            })}
          </nav>
          <div className="who" id="sidebar-who">
            <ThemeSwitch />
            <div className="who-row">
              <span className="who-avatar" aria-hidden="true">
                {session.name.slice(0, 1).toUpperCase()}
              </span>
              <p className="who-text">
                <span className="who-name">
                  <span className="visually-hidden">Signed in as </span>
                  {session.name}
                </span>
                <span className="who-role">{accessWord(session.permissions)}</span>
              </p>
              <button type="button" className="icon-button" onClick={signOut} aria-label="Sign out" title="Sign out">
                <SignOut size={18} aria-hidden="true" />
              </button>
            </div>
          </div>
        </aside>
        <main id="content" className="page" tabIndex={-1}>
          {/* The Overview shows the gateway's state in full; every other page
              says so at the top when the gateway refuses everything. */}
          {facts && view !== 'overview' && (facts.admission !== 'ready' || facts.audit_failed) ? (
            <p className="banner tone-refused" role="status">
              <strong>The gateway is not admitting requests.</strong> {admissionText[facts.admission] ?? 'Every protected request is refused.'}{' '}
              <a href="#/overview">Open the Overview</a>
            </p>
          ) : null}
          {view === 'overview' ? <Overview {...viewProps} /> : null}
          {view === 'traffic' ? <LiveTraffic {...viewProps} /> : null}
          {view === 'lookup' ? <RequestLookup key={window.location.hash} {...viewProps} /> : null}
          {view === 'apis' ? <ApisView {...viewProps} /> : null}
          {view === 'designer' ? (
            <Suspense fallback={<p role="status" className="hint">Loading the Policy Designer…</p>}>
              <Designer {...viewProps} draft={designerDraft} setDraft={setDesignerDraft} />
            </Suspense>
          ) : null}
          {view === 'policy' ? <PolicyView {...viewProps} /> : null}
          {view === 'candidates' ? <Candidates {...viewProps} draft={draft} candidate={candidate} /> : null}
          {view === 'overrides' ? <Overrides {...viewProps} /> : null}
          {view === 'accounts' ? <AccountsView {...viewProps} /> : null}
          {view === 'subjects' ? <Subjects {...viewProps} /> : null}
          {view === 'history' ? <History {...viewProps} /> : null}
          {view === 'code' ? <PolicyCode {...viewProps} /> : null}
          {view === 'team' ? <TeamView {...viewProps} /> : null}
          {view === 'account' ? <Account admin={admin} session={session} signedOut={signedOut} /> : null}
        </main>
      </div>
    </GatewayState.Provider>
  )
}
