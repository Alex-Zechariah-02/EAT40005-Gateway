import { lazy, Suspense, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { CaretDown, CaretRight, SidebarSimple, SignOut } from '@phosphor-icons/react'
import { adminClient, request, type Admin, type Permission, type Reply, type SessionInfo, type Status } from './api'
import { GatewayState, inactive, Navigate, Outcomes, liveMs, usePoll, useRereadWhen, useResource, ViewMemory, type Gateway } from './hooks'
import { Icon, type IconName } from './icons'
import { UnresolvedChanges, useOutcomeRegister } from './outcomes'
import { Problem, time, TimeOfDay, TooltipLayer, usePageScrollbar } from './ui'
import { admissionText, Brand } from './state'
import { SignIn } from './signin'
import { guideHidden, Overview } from './overview'
import { Notifications } from './notices'
import { PolicyView } from './policy'
import { Candidates } from './candidates'
import { Overrides } from './overrides'
import { History } from './history'
import { Account } from './account'
import { AccountsView, ApisView, LiveTraffic, RequestLookup, TeamView } from './monitor'
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

// The views by job, each group opening and closing like a folder: watch
// the gateway, shape its policy, put changes live, control access, and
// administer it.
const navigation: { label: string; items: NavItem[] }[] = [
  {
    label: 'Monitor',
    items: [
      { id: 'overview', label: 'Overview', icon: 'overview', needs: 'read' },
      { id: 'traffic', label: 'Live Traffic', icon: 'traffic', needs: 'read' },
      { id: 'lookup', label: 'Request Lookup', icon: 'lookup', needs: 'read' },
      { id: 'apis', label: 'APIs', icon: 'apis', needs: 'read' },
    ],
  },
  {
    label: 'Policy',
    items: [
      { id: 'designer', label: 'Policy', icon: 'permit', needs: 'read' },
      { id: 'policy', label: 'Versions', icon: 'versions', needs: 'read' },
    ],
  },
  {
    label: 'Deploy',
    items: [
      { id: 'candidates', label: 'Changes', icon: 'candidates', needs: 'read' },
      { id: 'overrides', label: 'Temporary Rules', icon: 'overrides', needs: 'read' },
    ],
  },
  {
    label: 'Access',
    items: [{ id: 'accounts', label: 'Accounts', icon: 'accounts', needs: 'read' }],
  },
  {
    label: 'Administration',
    items: [
      { id: 'history', label: 'History', icon: 'history', needs: 'read_history' },
      { id: 'team', label: 'Team', icon: 'team', needs: 'read' },
      { id: 'account', label: 'My Account', icon: 'account', needs: 'read' },
    ],
  },
]

const titles: Record<View, string> = {
  overview: 'Overview',
  traffic: 'Live Traffic',
  lookup: 'Request Lookup',
  apis: 'APIs',
  designer: 'Policy',
  policy: 'Versions',
  candidates: 'Changes',
  overrides: 'Temporary Rules',
  accounts: 'Accounts',
  history: 'History',
  team: 'Team',
  account: 'My Account',
}

// Former pages now part of another one, so an old bookmark still opens it.
const moved: Record<string, View> = { provisional: 'candidates', roles: 'policy', code: 'designer' }

// The open view is kept in the address fragment, so a reload or the
// browser's back button returns to it. The fragment is never sent to the
// server; a view may keep its search there (#/lookup?ref=...).
function viewFromHash(): View {
  const id = window.location.hash.replace(/^#\/?/, '').split('?')[0]
  if (Object.hasOwn(moved, id)) return moved[id]
  // Only the views' own names: "constructor" and the like are inherited, not views.
  return Object.hasOwn(titles, id) ? (id as View) : 'overview'
}

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

// The sidebar starts as a narrow strip of icons; widening it is remembered
// in one localStorage key, a display preference that grants nothing.
const sidebarKey = 'gateway.sidebar'

function readSidebar(): boolean {
  try {
    return localStorage.getItem(sidebarKey) === 'open'
  } catch {
    return false
  }
}

function saveSidebar(open: boolean) {
  try {
    if (open) localStorage.setItem(sidebarKey, 'open')
    else localStorage.removeItem(sidebarKey)
  } catch {
    // Storage refused: the choice lasts for this page only.
  }
}

// accessWord sums up an administrator's permissions in two words.
function accessWord(permissions: Permission[]): string {
  if (permissions.includes('manage_policy') && permissions.includes('manage_subjects') && permissions.includes('read_history')) return 'Full access'
  if (permissions.includes('manage_policy') || permissions.includes('manage_subjects')) return 'Can make changes'
  return 'Read only'
}

function Shell(props: {
  admin: Admin
  session: SessionInfo
  signedOut: (message: string, explicit?: boolean) => void
  onStatus: (facts: Status | undefined) => void
}) {
  const { admin, session, signedOut, onStatus } = props
  const [view, setView] = useState<View>(viewFromHash)
  const [draft, setDraft] = useState<string | undefined>(undefined)
  const [candidate, setCandidate] = useState<number | undefined>(undefined)
  const [designerDraft, setDesignerDraft] = useState<DesignerDraft | null>(null)
  const [menuOpen, setMenuOpen] = useState(false)
  const [wide, setWide] = useState(readSidebar)
  // closedGroups are the sidebar groups the administrator has closed.
  const [closedGroups, setClosedGroups] = useState<Set<string>>(() => new Set())
  // pendingDraft carries a draft from open() to the view change it causes;
  // any other change of view, such as the back button, opens no draft.
  const pendingDraft = useRef<Handoff | undefined>(undefined)
  const firstView = useRef(true)
  usePageScrollbar()

  const status = useResource<Status>((signal) => admin.get('/admin/status', signal), [])
  const can = (permission: Permission) => session.permissions.includes(permission)
  // Each change of view rereads the status.
  useRereadWhen(view, status.refresh)

  // While a trial or temporary rule is active the status is reread every
  // 3 s, so a change made elsewhere or a deadline the gateway enforces shows
  // within seconds; otherwise only the Overview rereads it, every 10 s.
  const facts = status.last?.body
  const critical = facts !== undefined && (facts.provisional !== undefined || (facts.overrides?.length ?? 0) > 0)
  usePoll(status.refresh, liveMs, critical)
  const factsKey = facts ? JSON.stringify(facts) : ''
  const [version, setVersion] = useState(0)
  useEffect(() => {
    setVersion((v) => v + 1)
    onStatus(facts)
  }, [factsKey])
  const gateway: Gateway = {
    ...status,
    facts,
    critical,
    version,
    refresh: () => {
      status.refresh()
      setVersion((v) => v + 1)
    },
  }
  const memory = useRef(new Map<string, unknown>()).current
  const paused = usePaused(critical)
  const outcomes = useContext(Outcomes)

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
      signedOut('Signed out.', true)
    } else if (reply.kind === 'unknown') {
      signedOut('Signed out of this page, but the gateway did not confirm it; the session ends at its idle timeout.', true)
    }
  }

  const current = view
  const viewProps = { admin, can, open }
  const sessionEnds = new Date(session.expires_at).getTime()
  const trialOutlivesSession = facts?.provisional !== undefined && facts.deadline !== undefined && sessionEnds < new Date(facts.deadline).getTime()

  return (
    <GatewayState.Provider value={gateway}>
    <ViewMemory.Provider value={memory}>
    <Navigate.Provider value={open}>
      <a className="visually-hidden" href="#content">
        Skip to Content
      </a>
      <div className={wide ? 'shell' : 'shell is-rail'}>
        <TooltipLayer />
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
              const open = !closedGroups.has(group.label)
              const listId = `nav-${group.label.toLowerCase()}`
              return (
                <div className="nav-group" key={group.label}>
                  <button
                    type="button"
                    className="nav-group-label"
                    aria-expanded={open}
                    aria-controls={listId}
                    onClick={() =>
                      setClosedGroups((previous) => {
                        const next = new Set(previous)
                        if (next.has(group.label)) next.delete(group.label)
                        else next.add(group.label)
                        return next
                      })
                    }
                  >
                    {group.label}
                    {open ? <CaretDown size={12} aria-hidden="true" /> : <CaretRight size={12} aria-hidden="true" />}
                  </button>
                  <ul id={listId} hidden={!open}>
                    {items.map((item) => (
                      <li key={item.id}>
                        <a href={`#/${item.id}`} aria-current={current === item.id ? 'page' : undefined} data-tip={item.label}>
                          <Icon name={item.icon} />
                          <span className="nav-label">{item.label}</span>
                          {item.id === 'candidates' && facts?.provisional !== undefined ? <span className="nav-flag tone-pending">On Trial</span> : null}
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
            <button
              type="button"
              className="sidebar-toggle"
              data-tip={wide ? 'Collapse Sidebar' : 'Expand Sidebar'}
              onClick={() => {
                setWide((open) => {
                  saveSidebar(!open)
                  return !open
                })
              }}
            >
              <SidebarSimple size={20} aria-hidden="true" />
              <span className="nav-label">{wide ? 'Collapse Sidebar' : 'Expand Sidebar'}</span>
            </button>
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
              <button type="button" className="icon-button" onClick={signOut} aria-label="Sign Out" data-tip="Sign out">
                <SignOut size={18} aria-hidden="true" />
              </button>
            </div>
          </div>
        </aside>
        <main id="content" className="page" tabIndex={-1}>
          <Notifications status={facts} view={view} />
          {/* The Overview shows the gateway's state in full; every other page
              says so at the top when the gateway refuses everything. */}
          {facts && view !== 'overview' && (facts.admission !== 'ready' || facts.audit_failed) ? (
            <p className="banner tone-refused" role="status">
              <strong>The gateway is not accepting requests.</strong> {admissionText[facts.admission] ?? 'Every protected request is refused.'}{' '}
              <a href="#/overview">Open the Overview</a>
            </p>
          ) : null}
          {facts?.stored_authority === 'unreadable' ? (
            <p className="banner tone-unknown" role="status">
              <strong>The gateway could not read its stored policy.</strong> Which version is active or on trial is not known from its latest
              answer; nothing here means there is none.
            </p>
          ) : null}
          {trialOutlivesSession ? (
            <p className="banner tone-pending">
              <strong>Your session ends at <TimeOfDay value={session.expires_at} />, before version {facts?.provisional}'s trial deadline.</strong>{' '}
              Sign in again before then to keep or undo it; otherwise the gateway undoes it by itself at the deadline.
            </p>
          ) : null}
          {paused && (view === 'overview' || view === 'traffic' || view === 'apis') ? (
            <p className="banner tone-neutral">
              Auto-refresh paused after 10 minutes of inactivity, so an unattended page does not keep the session open. It resumes when you use
              the page.
            </p>
          ) : null}
          {outcomes ? <UnresolvedChanges admin={admin} name={session.name} register={outcomes} canReadHistory={can('read_history')} /> : null}
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
          {view === 'history' ? <History {...viewProps} /> : null}
          {view === 'team' ? <TeamView {...viewProps} /> : null}
          {view === 'account' ? <Account
              admin={admin}
              session={session}
              signedOut={signedOut}
              showGuide={() => {
                memory.set(guideHidden, false)
                open('overview')
              }}
            /> : null}
        </main>
      </div>
    </Navigate.Provider>
    </ViewMemory.Provider>
    </GatewayState.Provider>
  )
}

// usePaused says, rechecked every 5 s, that the page's background reads are
// paused because nobody has used it for a while and no trial or temporary
// rule is active.
function usePaused(critical: boolean): boolean {
  const [paused, setPaused] = useState(false)
  useEffect(() => {
    const check = () => setPaused(!critical && inactive())
    check()
    const timer = setInterval(check, 5_000)
    return () => clearInterval(timer)
  }, [critical])
  return paused
}
