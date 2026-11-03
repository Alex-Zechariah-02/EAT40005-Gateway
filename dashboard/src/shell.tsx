import { lazy, Suspense, useContext, useEffect, useRef, useState } from 'react'
import { type Admin } from './http'
import type { Permission, SessionInfo, Status } from './api'
import {
  GatewayState,
  inactive,
  Navigate,
  Outcomes,
  liveMs,
  usePoll,
  useRereadWhen,
  useResource,
  ViewMemory,
  type Gateway,
} from './hooks'
import { UnresolvedChanges } from './outcomes'
import { TimeOfDay } from './time'
import { TooltipLayer } from './overlays'
import { usePageScrollbar } from './ui'
import { admissionText } from './state'
import { guideHidden, Overview } from './overview'
import { Notifications } from './notices'
import { PolicyView } from './policy'
import { Candidates } from './candidates'
import { Overrides } from './overrides'
import { History } from './history'
import { Account } from './account'
import { AccountsView } from './blocking'
import { ApisView } from './catalog'
import { LiveTraffic } from './traffic'
import { RequestLookup } from './lookup'
import { TeamView } from './team'
import { AccessCheckView } from './checker'
import { AdviceView, LimitsView } from './tools'
import { SecurityLogView } from './log'
import type { DesignerDraft } from './designer-model'
import type { Handoff, View } from './views'
import { readSidebar, Sidebar } from './sidebar'

// The signed-in shell: the open page, its address, the gateway's status and the page banners.

const Designer = lazy(() => import('./designer').then((module) => ({ default: module.Designer })))

const titles: Record<View, string> = {
  overview: 'Overview',
  'live-traffic': 'Live Traffic',
  'request-lookup': 'Request Lookup',
  'security-log': 'Security Log',
  'access-checker': 'Access Checker',
  limits: 'Limits',
  advice: 'Advice',
  apis: 'APIs',
  policy: 'Policy',
  versions: 'Versions',
  changes: 'Changes',
  'temporary-rules': 'Temporary Rules',
  accounts: 'Accounts',
  history: 'History',
  team: 'Team',
  'my-account': 'My Account',
}

// Former pages now part of another one, so an old bookmark still opens it.
const moved: Record<string, View> = { provisional: 'changes', roles: 'versions', code: 'policy' }

// The open view is kept in the address fragment, so a reload or the
// browser's back button returns to it. The fragment is never sent to the
// server; a view may keep its search there (#/request-lookup?ref=...).
function viewFromHash(): View {
  const id = window.location.hash.replace(/^#\/?/, '').split('?')[0]
  if (Object.hasOwn(moved, id)) return moved[id]
  // Only the views' own names: "constructor" and the like are inherited, not views.
  return Object.hasOwn(titles, id) ? (id as View) : 'overview'
}

export function Shell(props: {
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
        <Sidebar
          session={session}
          can={can}
          current={current}
          facts={facts}
          menuOpen={menuOpen}
          setMenuOpen={setMenuOpen}
          wide={wide}
          setWide={setWide}
          signOut={signOut}
        />
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
          {paused && (view === 'overview' || view === 'live-traffic' || view === 'apis') ? (
            <p className="banner tone-neutral">
              Auto-refresh paused after 10 minutes of inactivity, so an unattended page does not keep the session open. It resumes when you use
              the page.
            </p>
          ) : null}
          {outcomes ? <UnresolvedChanges admin={admin} name={session.name} register={outcomes} canReadHistory={can('read_history')} /> : null}
          {view === 'overview' ? <Overview {...viewProps} /> : null}
          {view === 'live-traffic' ? <LiveTraffic {...viewProps} /> : null}
          {view === 'request-lookup' ? <RequestLookup key={window.location.hash} {...viewProps} /> : null}
          {view === 'apis' ? <ApisView {...viewProps} /> : null}
          {view === 'policy' ? (
            <Suspense fallback={<p role="status" className="hint">Loading the Policy Page…</p>}>
              <Designer {...viewProps} draft={designerDraft} setDraft={setDesignerDraft} />
            </Suspense>
          ) : null}
          {view === 'versions' ? <PolicyView {...viewProps} /> : null}
          {view === 'changes' ? <Candidates {...viewProps} draft={draft} candidate={candidate} /> : null}
          {view === 'temporary-rules' ? <Overrides {...viewProps} /> : null}
          {view === 'accounts' ? <AccountsView {...viewProps} /> : null}
          {view === 'history' ? <History {...viewProps} /> : null}
          {view === 'team' ? <TeamView {...viewProps} /> : null}
          {view === 'security-log' ? <SecurityLogView {...viewProps} /> : null}
          {view === 'access-checker' ? <AccessCheckView {...viewProps} /> : null}
          {view === 'limits' ? <LimitsView {...viewProps} /> : null}
          {view === 'advice' ? <AdviceView {...viewProps} /> : null}
          {view === 'my-account' ? <Account
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
