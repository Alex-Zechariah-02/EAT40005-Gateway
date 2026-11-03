import { useEffect, useMemo, useRef, useState } from 'react'
import type { Page } from '../shared/api'
import { useGatewayState, liveMs, usePoll, useResource } from '../shared/hooks'
import { PageHeader } from '../app/header'
import { Problem } from '../shared/replies'
import type { ViewProps } from '../app/views'
import { EventRows, type RequestEvent } from './requests'
import { type Traffic, TrafficCards } from './summary'
import { lookupRef } from './lookup'

// The Live Traffic page: the request stream and a summary of a chosen window.

// LiveTraffic streams request events as they happen and summarises a
// chosen window. Pause keeps the list still for inspection; events that
// arrive meanwhile are counted, not lost from the gateway's record.
// The page keeps more requests than it draws, so a filter can still find
// writes made minutes ago between the boards' frequent reads.
const keptEvents = 1000
const shownEvents = 200
const reads = new Set(['GET', 'HEAD', 'OPTIONS'])

export function LiveTraffic({ admin }: ViewProps) {
  const state = useGatewayState()
  const [span, setSpan] = useState(60)
  const traffic = useResource<Traffic>((signal) => admin.get(`/admin/traffic?window=${span}`, signal), [span])
  usePoll(traffic.refresh, liveMs, state.critical)
  const [events, setEvents] = useState<RequestEvent[]>([])
  const [paused, setPaused] = useState(false)
  const [held, setHeld] = useState(0)
  const [dropped, setDropped] = useState(0)
  const [reconnected, setReconnected] = useState(false)
  const [stream, setStream] = useState<'connecting' | 'live' | 'closed'>('connecting')
  const [only, setOnly] = useState<'all' | 'changes' | 'refused'>('all')
  const pausedRef = useRef(paused)
  pausedRef.current = paused
  // Requests that arrive while paused wait here, newest first, and join the
  // list on Resume.
  const waiting = useRef<RequestEvent[]>([])

  useEffect(() => {
    const source = new EventSource('/admin/events/stream')
    let opened = false
    source.onopen = () => {
      if (opened) setReconnected(true)
      opened = true
      setStream('live')
    }
    source.onerror = () => setStream(source.readyState === EventSource.CLOSED ? 'closed' : 'connecting')
    source.addEventListener('request', (message) => {
      const event = JSON.parse((message as MessageEvent<string>).data) as RequestEvent
      if (pausedRef.current) {
        waiting.current = [event, ...waiting.current].slice(0, keptEvents)
        setHeld((n) => n + 1)
      } else setEvents((list) => [event, ...list].slice(0, keptEvents))
    })
    source.addEventListener('dropped', (message) => {
      const { count } = JSON.parse((message as MessageEvent<string>).data) as { count: number }
      setDropped((n) => n + count)
    })
    return () => source.close()
  }, [])

  // The list starts with the latest recorded requests; the stream adds newer
  // ones on top. A request in both appears once.
  const recent = useResource<Page<RequestEvent>>((signal) => admin.get('/admin/events?limit=50', signal), [])
  const recentItems = recent.reply?.kind === 'ok' ? recent.reply.body.items : undefined
  const all = useMemo(() => {
    const live = new Set(events.map((e) => e.request_id))
    return [...events, ...(recentItems ?? []).filter((e) => !live.has(e.request_id))]
  }, [events, recentItems])
  const matching = useMemo(
    () => (only === 'refused' ? all.filter((e) => e.decision !== 'allowed') : only === 'changes' ? all.filter((e) => !reads.has(e.method)) : all),
    [all, only],
  )
  const shown = matching.slice(0, shownEvents)
  return (
    <>
      <PageHeader title="Live Traffic" lede="Every request the gateway decides, in real time, with the check that decided it." />
      {traffic.reply && traffic.reply.kind !== 'ok' ? <Problem reply={traffic.reply} /> : null}
      {recent.reply && recent.reply.kind !== 'ok' ? <Problem reply={recent.reply} /> : null}
      <TrafficCards
        resource={traffic}
        span={span}
        onSpan={setSpan}
        stickySide
        main={
          <section className="card" aria-labelledby="stream-title">
            <div className="card-head card-head-pad">
              <h2 id="stream-title">
                Request Stream{' '}
                <span className={`live-state live-${stream}`} role="status">
                  {stream === 'live' ? (paused ? 'Paused' : 'Live') : stream === 'closed' ? 'Stream Closed' : 'Connecting…'}
                </span>
              </h2>
              <div className="card-tools">
                <div className="segmented" role="group" aria-label="Show">
                  <button type="button" aria-pressed={only === 'all'} onClick={() => setOnly('all')}>
                    All
                  </button>
                  <button type="button" aria-pressed={only === 'changes'} onClick={() => setOnly('changes')} title="Write requests only; reads are hidden">
                    Changes Only
                  </button>
                  <button type="button" aria-pressed={only === 'refused'} onClick={() => setOnly('refused')}>
                    Refused Only
                  </button>
                </div>
                <button
                  type="button"
                  className="btn btn-small"
                  aria-pressed={paused}
                  onClick={() => {
                    if (paused) {
                      const arrived = waiting.current
                      waiting.current = []
                      setEvents((list) => [...arrived, ...list].slice(0, keptEvents))
                    }
                    setPaused(!paused)
                    setHeld(0)
                  }}
                >
                  {paused ? `Resume${held ? ` (${held} new)` : ''}` : 'Pause'}
                </button>
              </div>
            </div>
            {reconnected ? (
              <p className="notice tone-pending card-notice">Stream reconnected. Events may have been missed; use Request lookup to search recent requests.</p>
            ) : null}
            {dropped > 0 ? (
              <p className="notice tone-pending card-notice">This view fell behind and skipped {dropped} events. The security log has every request.</p>
            ) : null}
            <div className="card-body">
              {shown.length ? (
                <>
                  <EventRows events={shown} open={lookupRef} label="Request Stream" />
                  {matching.length > shown.length ? (
                    <p className="hint card-empty">
                      Showing the newest {shown.length} of {matching.length.toLocaleString()}. Request lookup finds any request by its Ref.
                    </p>
                  ) : null}
                </>
              ) : (
                <p className="hint card-empty">
                  {stream === 'closed'
                    ? 'Stream closed. Reload the page to reconnect.'
                    : all.length
                      ? 'No matching requests yet.'
                      : 'Waiting for requests.'}
                </p>
              )}
            </div>
          </section>
        }
      />
    </>
  )
}
