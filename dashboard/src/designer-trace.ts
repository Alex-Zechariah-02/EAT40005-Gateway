import type { Operation, Policy } from './api'

export interface TraceStep {
  name: string
  state: 'passes' | 'refused' | 'not applicable' | 'not reached'
  text: string
}
export const traceExplanation =
  'This explains the policy document. The Gateway decides each request, and active temporary overrides can change the result.'

export function trace(policy: Policy, who: string, operation: Operation): TraceStep[] {
  const steps: TraceStep[] = []
  let stopped = false
  const step = (name: string, text: string, refused = false, applicable = true) => {
    steps.push({
      name,
      state: stopped ? 'not reached' : !applicable ? 'not applicable' : refused ? 'refused' : 'passes',
      text: stopped ? 'not reached' : text,
    })
    if (refused && applicable) stopped = true
  }
  step(
    'Route',
    `${operation.method} ${operation.target}. A request whose path also matches a more specific target is routed there instead.`,
  )
  step(
    'Access',
    operation.access === 'deny' ? '403 forbidden: the operation is denied' : 'The operation allows requests.',
    operation.access === 'deny',
  )
  const isPublic = operation.auth === 'none'
  step(
    'Token',
    isPublic
      ? 'not checked (public)'
      : who === 'none'
        ? '401: a valid token is required'
        : 'A valid token is required; this trace does not verify one.',
    !isPublic && who === 'none',
  )
  const subject = who.startsWith('subject:') ? policy.subjects?.find((item) => item.id === who.slice(8)) : undefined
  const roles = who.startsWith('role:') ? [who.slice(5)] : (subject?.roles ?? [])
  const permitted = policy.permits.some((permit) => permit.operation === operation.id && roles.includes(permit.role))
  const reason =
    who.startsWith('subject:') && !subject
      ? '403: subject not in the policy'
      : subject?.status === 'disabled'
        ? '403: subject disabled'
        : !permitted
          ? '403: no permit'
          : 'A held role has a permit.'
  step('Permit', isPublic ? 'not applicable' : reason, !isPublic && reason.startsWith('403'), !isPublic)
  step(
    'Rate',
    operation.rate
      ? `limited to ${operation.rate.count} per ${operation.rate.window_seconds} s per ${operation.rate.key === 'client_address' ? 'client address' : 'account'} (429 beyond)`
      : 'no rate limit',
  )
  const body = operation.body
    ? `body up to ${operation.body.max_bytes} B matching its schema (400 otherwise)`
    : 'no body accepted (400 if one is sent)'
  const headers =
    operation.headers && typeof operation.headers === 'object'
      ? (operation.headers as { properties?: Record<string, unknown> }).properties
      : undefined
  step('Body and headers', `${body}${headers ? `; declared headers: ${Object.keys(headers).join(', ')}` : ''}`)
  step('Event record', 'recorded before forwarding (503 if the event log cannot be written)')
  const api = policy.apis.find((item) => item.id === operation.api)
  const upstream = policy.upstreams.find((item) => item.id === api?.upstream)
  step('Forward', `${operation.api} → ${upstream?.id ?? 'missing upstream'} ${upstream?.dial ?? ''}`)
  return steps
}
