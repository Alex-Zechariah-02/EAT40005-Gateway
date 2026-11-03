import { createContext, useContext, useEffect, useRef, useState } from 'react'
import type { Admin } from './http'
import type { MutationRequest, Outcome, Reply, Status } from './api'
import type { Handoff, View } from './views'

// useResource loads one server read when the view opens and on refresh. A
// newer load or leaving the view aborts the older one, so an earlier answer
// can never replace a later one. It also keeps the last successful answer
// and when it arrived, so a view can go on showing it, marked with its age,
// after a later read fails. A change of deps (another window, another item)
// forgets it: an answer about something else is never shown as this one.
export function useResource<T>(load: (signal: AbortSignal) => Promise<Reply<T>>, deps: unknown[]) {
  const [reply, setReply] = useState<Reply<T> | null>(null)
  const [last, setLast] = useState<{ body: T; at: Date; date?: number } | null>(null)
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)
  const scope = useRef(deps)

  useEffect(() => {
    const controller = new AbortController()
    if (scope.current.length !== deps.length || scope.current.some((value, i) => !Object.is(value, deps[i]))) {
      scope.current = deps
      setLast(null)
    }
    setLoading(true)
    load(controller.signal).then((r) => {
      if (controller.signal.aborted || r.kind === 'aborted') return
      setReply(r)
      if (r.kind === 'ok') setLast({ body: r.body, at: new Date(), date: r.date })
      setLoading(false)
    })
    return () => controller.abort()
  }, [...deps, tick])

  return { reply, last, loading, failed: reply !== null && reply.kind !== 'ok', refresh: () => setTick((t) => t + 1) }
}

export type Resource<T> = ReturnType<typeof useResource<T>>

// useRereadWhen rereads a resource each time key changes after the first
// render. Unlike a change of the resource's deps, it keeps the last answer
// shown until the new one arrives.
export function useRereadWhen(key: unknown, refresh: () => void) {
  const seen = useRef(key)
  useEffect(() => {
    if (Object.is(seen.current, key)) return
    seen.current = key
    refresh()
  }, [key])
}

// usePaged keeps a growing list read with the server's cursor.
export function usePaged<T>(admin: Admin, path: string, deps: unknown[]) {
  const [items, setItems] = useState<T[]>([])
  const [next, setNext] = useState<string | undefined>(undefined)
  const [paging, setPaging] = useState(false)
  // pageProblem is the answer of a later page that could not be read.
  const [pageProblem, setPageProblem] = useState<Reply<unknown> | null>(null)
  // first counts first-page reads, so a later page that arrives after a
  // refresh is not appended to the new list.
  const first = useRef(0)
  const resource = useResource<{ items: T[]; next?: string }>((signal) => admin.get(path, signal), deps)
  useEffect(() => {
    first.current++
    setPageProblem(null)
    if (resource.reply?.kind === 'ok') {
      setItems(resource.reply.body.items)
      setNext(resource.reply.body.next)
    }
  }, [resource.reply])

  // read appends up to pages further pages, stopping at the end of the list,
  // at a failed read or when a refresh starts a new list.
  async function read(pages: number) {
    let cursor = next
    const list = first.current
    const separator = path.includes('?') ? '&' : '?'
    setPaging(true)
    setPageProblem(null)
    for (let page = 0; cursor && page < pages; page++) {
      const reply = await admin.get<{ items: T[]; next?: string }>(`${path}${separator}after=${encodeURIComponent(cursor)}`)
      if (list !== first.current) break
      if (reply.kind !== 'ok') {
        if (reply.kind !== 'aborted') setPageProblem(reply)
        break
      }
      setItems((current) => [...current, ...reply.body.items])
      cursor = reply.body.next
      setNext(cursor)
    }
    setPaging(false)
  }

  return { ...resource, items, next, paging, pageProblem, more: () => read(1), rest: (pages: number) => read(pages) }
}

// Every read renews the administrator's session, so a page left open would
// stay signed in until the session's lifetime ends. Background reads
// therefore pause after this long without keyboard, pointer or touch input,
// and resume at the next input; while a trial or temporary rule is active
// they go on regardless.
const inactiveAfterMs = 10 * 60_000
let lastInput = Date.now()
for (const type of ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart']) {
  window.addEventListener(type, () => (lastInput = Date.now()), { capture: true, passive: true })
}

export function inactive(): boolean {
  return Date.now() - lastInput >= inactiveAfterMs
}

// liveMs is how often a visible page rereads what it shows: every second,
// so administrators never act on figures several seconds old. Each read is
// a small JSON answer from the admin listener; the 10-minute inactivity
// pause above still stops them on an unattended page.
export const liveMs = 1_000

// usePoll rereads every ms while the page is visible (ms null: no timer),
// and at once whenever the page becomes visible again, so a tab coming back
// to the front never goes on showing what it read before it was hidden.
// Unless keepAwake (a trial or temporary rule is active), it skips its reads
// while the page is inactive.
export function usePoll(refresh: () => void, ms: number | null, keepAwake: boolean) {
  const latest = useRef(refresh)
  latest.current = refresh
  const awake = useRef(keepAwake)
  awake.current = keepAwake
  useEffect(() => {
    const due = () => document.visibilityState === 'visible' && (awake.current || !inactive())
    const onVisible = () => {
      if (due()) latest.current()
    }
    document.addEventListener('visibilitychange', onVisible)
    const timer = ms === null ? undefined : setInterval(onVisible, ms)
    return () => {
      clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [ms])
}

type Phase<T> =
  | { state: 'idle' }
  | { state: 'pending'; request: MutationRequest }
  | { state: 'replied'; request: MutationRequest; reply: Reply<T> }

// useMutation sends one state-changing request at a time. The result is the
// server's reply only; after any definite reply the view rereads its data.
// An unanswered request stays "outcome unknown" until the administrator
// refreshes or deliberately resends the identical request, and one that
// carries a request key is also listed on every page until its outcome is
// known (Outcomes).
export function useMutation<T = Outcome>(admin: Admin, onSettled: () => void) {
  const [phase, setPhase] = useState<Phase<T>>({ state: 'idle' })
  const busy = useRef(false)
  const outcomes = useContext(Outcomes)

  async function run(request: MutationRequest) {
    if (busy.current) return
    busy.current = true
    setPhase({ state: 'pending', request })
    // The register's lifetime is taken when the request is sent: if the
    // administrator signs out before the answer arrives, the answer belongs
    // to a page state that no longer exists and changes nothing on screen.
    const since = outcomes?.lifetime()
    const reply = await admin.send<T>(request)
    busy.current = false
    if (outcomes && since !== outcomes.lifetime()) return
    setPhase({ state: 'replied', request, reply })
    if (outcomes && since !== undefined && request.key !== undefined) {
      if (reply.kind === 'unknown') outcomes.add(request, since)
      if (reply.kind === 'ok' || reply.kind === 'refused') outcomes.settle(request, since)
    }
    if (reply.kind !== 'unknown') onSettled()
  }

  return {
    phase,
    pending: phase.state === 'pending',
    run,
    clear: () => setPhase({ state: 'idle' }),
  }
}

export type Mutation<T> = ReturnType<typeof useMutation<T>>

// Unresolved is a change that was sent but got no answer. It is kept in this
// page's memory only, never in browser storage.
export interface Unresolved {
  // id is the transition the gateway records it under.
  id: string
  admin: string
  label: string
  request: MutationRequest
  sentAt: Date
  // checked is the latest look-up that could not settle it.
  checked?: { at: Date; text: string }
  settled?: { tone: 'ok' | 'pending' | 'refused'; text: string }
}

export interface OutcomeRegister {
  items: Unresolved[]
  // lifetime changes each time the register is cleared by signing out. A
  // request records it when sent and passes it back with its answer, so an
  // answer from before the clear is ignored.
  lifetime(): number
  add(request: MutationRequest, since: number): void
  // settle drops a change whose view received a definite answer.
  settle(request: MutationRequest, since: number): void
  update(id: string, change: Partial<Unresolved>): void
  forget(id: string): void
}

export const Outcomes = createContext<OutcomeRegister | null>(null)

// Gateway is the gateway's status as read by the signed-in shell. It is
// reread on sign-in, on every change of view, after each change a view
// makes, when a deadline it shows passes, and every second (liveMs).
export interface Gateway extends Resource<Status> {
  // facts is the last status read successfully, kept while later reads fail.
  facts?: Status
  // version changes when the facts change or someone asks for a reread, so
  // reads that depend on the status run again then, not at every poll.
  version: number
  // critical says a trial or temporary rule is active, as last read.
  critical: boolean
}

export const GatewayState = createContext<Gateway | null>(null)

export function useGatewayState(): Gateway {
  const state = useContext(GatewayState)
  if (!state) throw new Error('useGatewayState outside the signed-in shell')
  return state
}

// ViewMemory keeps a view's choices, such as search filters, while the
// administrator moves between views. It belongs to the signed-in shell, so
// signing out or a new sign-in forgets it; nothing goes to browser storage.
export const ViewMemory = createContext<Map<string, unknown> | null>(null)

// Navigate opens a view from a part of the page that is not a view itself,
// such as the header's search.
export const Navigate = createContext<((view: View, handoff?: Handoff) => void) | null>(null)

export function useRemembered<T>(key: string, initial: T): [T, (value: T) => void] {
  const memory = useContext(ViewMemory)
  const [value, setValue] = useState<T>(() => (memory?.has(key) ? (memory.get(key) as T) : initial))
  const remember = (next: T) => {
    memory?.set(key, next)
    setValue(next)
  }
  return [value, remember]
}
