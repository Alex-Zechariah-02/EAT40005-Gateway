import { useSyncExternalStore } from 'react'
import { send, type Answer, type Request } from './api'
import type { Role, SignedIn, User } from './types'

// The signed-in person for this browser tab. The token lives only in
// this tab's sessionStorage: it survives a reload, disappears when the tab
// closes, and each tab can be a different person. Fetch Accounts issues it;
// the gateway alone decides what it allows. The roles shown here only choose
// which screens to offer; they never grant anything.

interface Session {
  token: string
  expiresAt: number
  user: User
}

interface State {
  session: Session | null
  // generation changes whenever the person changes, so answers that arrive
  // for the previous person are dropped.
  generation: number
  notice: string | null
}

const storageKey = 'fetch.session'
const refreshAhead = 90_000
const retryAfter = 15_000

export const roleNames: Record<Role, string> = {
  CUSTOMER: 'Customer',
  MERCHANT: 'Merchant',
  RIDER: 'Rider',
  SUPPORT: 'Support agent',
  OPERATIONS: 'Operations',
}

let state: State = { session: readStored(), generation: 0, notice: null }
const listeners = new Set<() => void>()
const identityListeners = new Set<() => void>()
let controller = new AbortController()
let refreshTimer: ReturnType<typeof setTimeout> | undefined

function readStored(): Session | null {
  try {
    const raw = sessionStorage.getItem(storageKey)
    if (!raw) return null
    const value = JSON.parse(raw) as Partial<Session> | null
    const user = value?.user
    if (
      !value || typeof value.token !== 'string' || !value.token ||
      typeof value.expiresAt !== 'number' || !Number.isFinite(value.expiresAt) || !user ||
      typeof user.id !== 'string' || !user.id || typeof user.name !== 'string' || typeof user.email !== 'string' ||
      !Array.isArray(user.roles) || !user.roles.every((role) => typeof role === 'string' && Object.hasOwn(roleNames, role)) ||
      (user.merchant !== null && typeof user.merchant !== 'string') ||
      !['/', '/merchant', '/rider', '/support', '/ops'].includes(user.home)
    ) return null
    if (value.expiresAt <= Date.now()) {
      sessionStorage.removeItem(storageKey)
      return null
    }
    return value as Session
  } catch {
    return null
  }
}

function store(session: Session | null) {
  try {
    if (session) sessionStorage.setItem(storageKey, JSON.stringify(session))
    else sessionStorage.removeItem(storageKey)
  } catch {
    // Storage refused (private mode limits): the session lasts for this page only.
  }
}

function update(next: Partial<State>) {
  state = { ...state, ...next }
  for (const listener of listeners) listener()
}

function fromAnswer(data: SignedIn): Session {
  return { token: data.token, expiresAt: Date.parse(data.expires_at), user: data.user }
}

// begin switches to another person (or to nobody): pending requests are
// cancelled and everything shown for the previous person is cleared.
function begin(session: Session | null, notice: string | null) {
  controller.abort()
  controller = new AbortController()
  store(session)
  update({ session, generation: state.generation + 1, notice })
  for (const listener of identityListeners) listener()
  schedule()
}

function schedule() {
  clearTimeout(refreshTimer)
  if (!state.session) return
  const wait = Math.max(0, state.session.expiresAt - Date.now() - refreshAhead)
  refreshTimer = setTimeout(() => void refresh(), wait)
}

async function refresh() {
  const current = state.session
  if (!current) return
  const answer = await send<SignedIn>({ method: 'POST', path: '/accounts/refresh', token: current.token, signal: controller.signal })
  if (state.session !== current) return
  if (answer.kind === 'ok') {
    const next = fromAnswer(answer.data)
    if (next.user.id !== current.user.id) {
      begin(next, null)
      return
    }
    store(next)
    update({ session: next })
    schedule()
    return
  }
  if (answer.kind === 'refused' && answer.status === 401) {
    signOut('Your session ended. Sign in again to continue.')
    return
  }
  // No answer, a rate limit or a server error: try again while the token is
  // still valid; the gateway refuses it once it expires.
  if (Date.now() < current.expiresAt - retryAfter) {
    refreshTimer = setTimeout(() => void refresh(), retryAfter)
  }
}

if (state.session) schedule()

export async function signIn(email: string, password: string): Promise<Answer<SignedIn>> {
  const answer = await send<SignedIn>({ method: 'POST', path: '/accounts/login', body: { email, password } })
  if (answer.kind === 'ok') begin(fromAnswer(answer.data), null)
  return answer
}

export function signOut(notice: string | null = null) {
  begin(null, notice)
}

export function dismissNotice() {
  update({ notice: null })
}

// call sends a request as the signed-in person. A change of person cancels
// it. A 401 for a token the gateway no longer accepts ends the session.
export async function call<T>(request: Omit<Request, 'token' | 'signal'> & { signal?: AbortSignal }): Promise<Answer<T>> {
  const session = state.session
  const signals = [controller.signal, ...(request.signal ? [request.signal] : [])]
  const answer = await send<T>({ ...request, token: session?.token ?? null, signal: AbortSignal.any(signals) })
  if (session && state.session === session && answer.kind === 'refused' && answer.status === 401 && !answer.service) {
    signOut('Your session ended. Sign in again to continue.')
  }
  return answer
}

export function onIdentityChange(listener: () => void): () => void {
  identityListeners.add(listener)
  return () => identityListeners.delete(listener)
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function useSession(): State {
  return useSyncExternalStore(subscribe, () => state)
}

export function currentGeneration(): number {
  return state.generation
}

export function hasRole(user: User | null | undefined, role: Role): boolean {
  return !!user?.roles.includes(role)
}
