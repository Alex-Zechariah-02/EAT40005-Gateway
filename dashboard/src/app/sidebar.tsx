import { useState, type Dispatch, type SetStateAction } from 'react'
import { CaretDown, CaretRight, SidebarSimple, SignOut } from '@phosphor-icons/react'
import type { Permission, SessionInfo, Status } from '../shared/api'
import { Icon, type IconName } from './icons'
import { Brand } from './state'
import { setTheme, useTheme, type ThemeChoice } from './theme'
import type { View } from './views'

// The sidebar: page navigation by group, width, appearance and the signed-in administrator.

interface NavItem {
  id: View
  label: string
  icon: IconName
  needs: Permission
}

// The views by job, each group opening and closing like a folder: watch
// the gateway, shape its policy, put changes live, control access, and
// administer it.
const navigation: { label: string; items: NavItem[] }[] = [
  {
    label: 'Monitor',
    items: [
      { id: 'overview', label: 'Overview', icon: 'overview', needs: 'read' },
      { id: 'live-traffic', label: 'Live Traffic', icon: 'traffic', needs: 'read' },
      { id: 'request-lookup', label: 'Request Lookup', icon: 'lookup', needs: 'read' },
      { id: 'security-log', label: 'Security Log', icon: 'log', needs: 'read_history' },
      { id: 'apis', label: 'APIs', icon: 'apis', needs: 'read' },
    ],
  },
  {
    label: 'Policy',
    items: [
      { id: 'policy', label: 'Policy', icon: 'permit', needs: 'read' },
      { id: 'versions', label: 'Versions', icon: 'versions', needs: 'read' },
      { id: 'advice', label: 'Advice', icon: 'advice', needs: 'read' },
    ],
  },
  {
    label: 'Deploy',
    items: [
      { id: 'changes', label: 'Changes', icon: 'candidates', needs: 'read' },
      { id: 'temporary-rules', label: 'Temporary Rules', icon: 'overrides', needs: 'read' },
    ],
  },
  {
    label: 'Access',
    items: [
      { id: 'accounts', label: 'Accounts', icon: 'accounts', needs: 'read' },
      { id: 'access-checker', label: 'Access Checker', icon: 'access', needs: 'read' },
    ],
  },
  {
    label: 'Administration',
    items: [
      { id: 'history', label: 'History', icon: 'history', needs: 'read_history' },
      { id: 'team', label: 'Team', icon: 'team', needs: 'read' },
      { id: 'limits', label: 'Limits', icon: 'limits', needs: 'read' },
      { id: 'my-account', label: 'My Account', icon: 'account', needs: 'read' },
    ],
  },
]

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

export function readSidebar(): boolean {
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

// Sidebar lists the pages the administrator may open, by group, with the
// width switch, the appearance switch and the signed-in administrator.
export function Sidebar(props: {
  session: SessionInfo
  can: (permission: Permission) => boolean
  current: View
  facts: Status | undefined
  menuOpen: boolean
  setMenuOpen: Dispatch<SetStateAction<boolean>>
  wide: boolean
  setWide: Dispatch<SetStateAction<boolean>>
  signOut: () => void
}) {
  const { session, can, current, facts, menuOpen, setMenuOpen, wide, setWide, signOut } = props
  // closedGroups are the sidebar groups the administrator has closed.
  const [closedGroups, setClosedGroups] = useState<Set<string>>(() => new Set())
  return (
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
                      {item.id === 'changes' && facts?.provisional !== undefined ? <span className="nav-flag tone-pending">On Trial</span> : null}
                      {item.id === 'temporary-rules' && facts?.overrides && facts.overrides.length > 0 ? (
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
  )
}
