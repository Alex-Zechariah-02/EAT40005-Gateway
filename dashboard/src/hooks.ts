import { createContext, useContext, useEffect, useRef, useState } from 'react'
import type { Admin, MutationRequest, Outcome, Reply, Status } from './api'

// useResource loads one server read when the view opens and on refresh. A
// newer load or leaving the view aborts the older one, so an earlier answer
// can never replace a later one.
export function useResource<T>(load: (signal: AbortSignal) => Promise<Reply<T>>, deps: unknown[]) {
  const [reply, setReply] = useState<Reply<T> | null>(null)
  const [readAt, setReadAt] = useState<Date | null>(null)
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    load(controller.signal).then((r) => {
      if (controller.signal.aborted || r.kind === 'aborted') return
      setReply(r)
      setReadAt(new Date())
      setLoading(false)
    })
    return () => controller.abort()
  }, [...deps, tick])

  return { reply, readAt, loading, refresh: () => setTick((t) => t + 1) }
}

export type Resource<T> = ReturnType<typeof useResource<T>>

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

type Phase<T> =
  | { state: 'idle' }
  | { state: 'pending'; request: MutationRequest }
  | { state: 'replied'; request: MutationRequest; reply: Reply<T> }

// useMutation sends one state-changing request at a time. The result is the
// server's reply only; after any definite reply the view rereads its data.
// An unanswered request stays "outcome unknown" until the administrator
// refreshes or deliberately resends the identical request.
export function useMutation<T = Outcome>(admin: Admin, onSettled: () => void) {
  const [phase, setPhase] = useState<Phase<T>>({ state: 'idle' })
  const busy = useRef(false)

  async function run(request: MutationRequest) {
    if (busy.current) return
    busy.current = true
    setPhase({ state: 'pending', request })
    const reply = await admin.send<T>(request)
    busy.current = false
    setPhase({ state: 'replied', request, reply })
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

// The gateway's status as last read by the signed-in shell. It is reread on
// sign-in, on every change of view, after each change a view makes and when
// a deadline it shows passes; nothing polls it.
export const GatewayState = createContext<Resource<Status> | null>(null)

export function useGatewayState(): Resource<Status> {
  const state = useContext(GatewayState)
  if (!state) throw new Error('useGatewayState outside the signed-in shell')
  return state
}
