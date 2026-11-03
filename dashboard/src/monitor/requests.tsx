import { AccountLink, XRef } from '../shared/links'
import { Badge, TableScroll, type Tone } from '../shared/ui'
import { Time, Zone } from '../shared/time'
import { decisionWords, stages, stepWords, type Stage, type StepState } from '../shared/checks'
import { Explain } from '../shared/explain'

// Request events as the gateway records them: their rows, details and decision path.

export interface RequestEvent {
  request_id: string
  time: string
  method: string
  path: string
  operation: string | null
  api: string | null
  subject: string | null
  roles: string[]
  client_address: string
  decision: 'allowed' | 'refused' | 'failed'
  status: number
  stage: Stage
  code: string | null
  generation: string
  duration_ms: number
}

// Plain words for the gateway's refusal codes; the code stays one
// hover away.
const reasons: Record<string, string> = {
  bad_request: 'Request Refused by Validation',
  unauthenticated: 'Not Signed In or Token Not Accepted',
  forbidden: 'No Permission',
  account_blocked: 'Account Blocked',
  not_found: 'No Such Route',
  method_not_allowed: 'Method Not Allowed',
  payload_too_large: 'Body Too Large',
  unsupported_media_type: 'Body Not JSON',
  rate_limited: 'Too Many Requests',
  upstream_unavailable: 'Service Unreachable',
  gateway_unavailable: 'Gateway Could Not Record or Decide',
  upstream_timeout: 'Service Too Slow; Outcome Unknown',
}

export function reasonOf(code: string | null): string {
  return code ? (reasons[code] ?? code) : 'Allowed'
}

const decisionTone: Record<RequestEvent['decision'], Tone> = { allowed: 'ok', refused: 'refused', failed: 'unknown' }

// beforeRouting says the gateway decided a request before matching it to an
// operation: it was refused on its form, or no policy could be captured (a
// gateway that admits nothing). The event then names a later check, but none
// of the checks ahead of it ran.
function beforeRouting(event: Pick<RequestEvent, 'stage' | 'decision' | 'operation'>): boolean {
  return event.decision !== 'allowed' && event.operation === null && event.stage !== 'route'
}

// pathStates says what happened at each of the gateway's checks: those ahead
// of the deciding check passed, unless the request was decided before
// routing; those after it were never reached.
function pathStates(event: Pick<RequestEvent, 'stage' | 'decision' | 'operation'>): StepState[] {
  const stop = stages.findIndex((s) => s.id === event.stage)
  const early = beforeRouting(event)
  return stages.map((_, n) => {
    if (event.decision === 'allowed') return 'passed'
    if (n === stop) return event.decision === 'failed' ? 'unknown' : 'refused'
    if (n > stop) return 'unreached'
    return early ? 'skipped' : 'passed'
  })
}

// DecisionPath shows the checks a request cleared, the one that decided it
// and those it never reached.
function DecisionPath({ event }: { event: Pick<RequestEvent, 'stage' | 'decision' | 'operation'> }) {
  const states = pathStates(event)
  const allowed = event.decision === 'allowed'
  const early = beforeRouting(event)
  const name = stages.find((s) => s.id === event.stage)?.name ?? event.stage
  const cleared = states.filter((s) => s === 'passed').length
  return (
    <span className="decision-path">
      <span
        className="dp-steps"
        role="img"
        aria-label={allowed ? 'Cleared every check' : early ? `Decided before routing, at ${name}` : `Cleared ${cleared} checks, decided at ${name}`}
      >
        {stages.map((s, n) => (
          <span key={s.id} className={`dp-step dp-${states[n] === 'skipped' ? 'unreached' : states[n]}`} title={`${s.name}: ${stepWords[states[n]]}`} />
        ))}
      </span>
      <span className={`dp-label dp-label-${event.decision}`}>{allowed ? 'All Passed' : early ? 'Before Routing' : name}</span>
    </span>
  )
}

export function EventRows({ events, open, label }: { events: RequestEvent[]; open: (ref: string) => void; label: string }) {
  return (
    <TableScroll label={label}>
      <table className="events">
        <thead>
          <tr>
            <th scope="col">
              Time (<Zone />)
            </th>
            <th scope="col">Request</th>
            <th scope="col">Account</th>
            <th scope="col">Result</th>
            <th scope="col">Checks</th>
            <th scope="col" className="end">
              Ref
            </th>
          </tr>
        </thead>
        <tbody>
          {events.map((e) => (
            <tr key={e.request_id}>
              <td className="num nowrap muted">
                <Time value={e.time} compact />
              </td>
              <td className="nowrap">
                {e.operation ? (
                  <XRef view="policy" focus={{ select: `operation:${e.operation}` }}>
                    <code>
                      <span className="method">{e.method}</span> {e.path}
                    </code>
                  </XRef>
                ) : (
                  <code>
                    <span className="method">{e.method}</span> {e.path}
                  </code>
                )}
              </td>
              <td className="nowrap">
                {e.subject ? (
                  <AccountLink account={e.subject}>
                    <code className="soft">{e.subject}</code>
                  </AccountLink>
                ) : (
                  <code className="soft">{e.client_address}</code>
                )}
              </td>
              <td>
                {e.decision === 'allowed' ? (
                  <span className="muted">
                    Allowed in {e.duration_ms.toFixed(1)} ms
                  </span>
                ) : (
                  <span title={e.code ?? undefined}>{reasonOf(e.code)}</span>
                )}{' '}
                <span className="status-chip">{e.status}</span>
              </td>
              <td>
                <DecisionPath event={e} />
              </td>
              <td className="nowrap end">
                <button type="button" className="link-button" onClick={() => open(e.request_id)} title="Open in Request Lookup">
                  <code>{e.request_id.slice(0, 8)}</code>
                  <span className="visually-hidden"> (open request {e.request_id})</span>
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </TableScroll>
  )
}

export function EventDetail({ event }: { event: RequestEvent }) {
  return (
    <section className="section card card-pad event-detail" aria-labelledby="detail-title">
      <div className="card-head">
        <h2 id="detail-title">
          <Badge tone={decisionTone[event.decision]}>{decisionWords[event.decision]}</Badge>{' '}
          {event.decision === 'allowed' ? 'Forwarded to the service' : reasonOf(event.code)}
        </h2>
      </div>
      <ol className="path-steps" aria-label="The gateway's checks, in order">
        {pathStates(event).map((state, n) => (
          <li key={stages[n].id} className={`path-step dp-${state === 'skipped' ? 'unreached' : state}`}>
            <span className="path-bar" aria-hidden="true" />
            <span className="path-name">
              <Explain term={stages[n].name} plain={stages[n].plain} technical={stages[n].technical} />
            </span>
            <span className="path-word">{stepWords[state]}</span>
          </li>
        ))}
      </ol>
      {beforeRouting(event) ? (
        <p className="hint">Decided before endpoint matching; earlier checks did not run.</p>
      ) : null}
      <dl className="facts">
        <div className="fact">
          <dt>Reference</dt>
          <dd>
            <code>{event.request_id}</code>
          </dd>
        </div>
        <div className="fact">
          <dt>When</dt>
          <dd>
            <Time value={event.time} />
          </dd>
        </div>
        <div className="fact">
          <dt>Request</dt>
          <dd>
            <code>
              {event.method} {event.path}
            </code>
          </dd>
        </div>
        <div className="fact">
          <dt>Endpoint</dt>
          <dd>
            {event.operation ? (
              <XRef view="policy" focus={{ select: `operation:${event.operation}` }}>
                <code>{event.operation}</code>
              </XRef>
            ) : (
              'none matched'
            )}
          </dd>
        </div>
        <div className="fact">
          <dt>Account</dt>
          <dd>
            {event.subject ? <code>{event.subject}</code> : 'not signed in'}
            {event.roles.length ? <> as {event.roles.join(', ')}</> : null}
          </dd>
        </div>
        <div className="fact">
          <dt>Client Address</dt>
          <dd>
            <code>{event.client_address}</code>
          </dd>
        </div>
        <div className="fact">
          <dt>Answer</dt>
          <dd>
            <code>
              {event.status}
              {event.code ? ` ${event.code}` : ''}
            </code>{' '}
            in {event.duration_ms.toFixed(1)} ms
          </dd>
        </div>
        <div className="fact">
          <dt>Active Policy (Generation)</dt>
          <dd>
            <code>{event.generation}</code>
          </dd>
        </div>
      </dl>
      <p className="hint">Tokens, passwords and request bodies are never logged.</p>
    </section>
  )
}
