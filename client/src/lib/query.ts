import { useCallback, useEffect, useState } from 'react'
import type { Answer } from './api'
import { call, useSession } from './session'

export interface Query<T> {
  // answer is the latest answer for this path and person.
  answer: Answer<T> | null
  // data is the last successful answer's data, kept while a refresh runs or
  // after a refresh fails, so the screen does not empty itself.
  data: T | null
  loading: boolean
  reload: () => void
}

interface Held<T> {
  key: string
  answer: Answer<T> | null
  data: T | null
  loading: boolean
}

// useQuery reads one path through the gateway as the current person. A
// change of person starts again from nothing. With poll, it reads again every
// poll milliseconds while the tab is visible, never two at once, until the
// service answers 404.
export function useQuery<T>(path: string | null, options: { poll?: number } = {}): Query<T> {
  const { generation } = useSession()
  const key = `${generation}|${path ?? ''}`
  const [held, setHeld] = useState<Held<T>>({ key, answer: null, data: null, loading: !!path })
  const [round, setRound] = useState(0)
  const poll = options.poll

  useEffect(() => {
    if (!path) return
    const controller = new AbortController()
    let busy = false
    let timer: ReturnType<typeof setInterval> | undefined
    const load = () => {
      if (busy) return
      busy = true
      setHeld((h) => (h.key === key ? { ...h, loading: true } : { key, answer: null, data: null, loading: true }))
      void call<T>({ method: 'GET', path, signal: controller.signal }).then((answer) => {
        busy = false
        if (answer.kind === 'cancelled') return
        // A service that says the thing does not exist leaves nothing to
        // follow, and the page keeps showing one Ref. The gateway's own 404
        // (no route under the policy in force) can pass, so it is asked again.
        if (answer.kind === 'refused' && answer.status === 404 && answer.service) clearInterval(timer)
        setHeld((h) => ({
          key,
          answer,
          data: answer.kind === 'ok' ? answer.data : h.key === key ? h.data : null,
          loading: false,
        }))
      })
    }
    load()
    timer = poll
      ? setInterval(() => {
          if (document.visibilityState === 'visible') load()
        }, poll)
      : undefined
    return () => {
      controller.abort()
      clearInterval(timer)
    }
  }, [key, path, poll, round])

  const reload = useCallback(() => setRound((r) => r + 1), [])
  if (held.key !== key) return { answer: null, data: null, loading: !!path, reload }
  return { answer: held.answer, data: held.data, loading: held.loading, reload }
}
