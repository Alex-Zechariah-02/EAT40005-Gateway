import type { Operation, Policy } from './api'

export interface TraceStep {
  name: string
  state: 'passes' | 'refused' | 'not applicable' | 'not reached'
  text: string
}
export const traceExplanation =
  'This explains the policy document. The Gateway decides each request, and active temporary overrides can change the result.'

// who is "none" (no token) or "role:<id>" (a token carrying that role).
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
    operation.access === 'deny' ? '403 forbidden: the endpoint is denied' : 'The endpoint allows requests.',
    operation.access === 'deny',
  )
  const isPublic = operation.auth === 'none'
  step(
    'Sign-In',
    isPublic
      ? 'not checked (public)'
      : who === 'none'
        ? '401: a valid token is required'
        : 'A valid token from the trusted issuer is required; this trace does not verify one. Its roles are kept only if this policy defines them.',
    !isPublic && who === 'none',
  )
  step(
    'Blocked List',
    isPublic ? 'not applicable' : 'An account on the blocked list is refused (403 account_blocked). The list is kept apart from the policy.',
    false,
    !isPublic,
  )
  const roles = who.startsWith('role:') ? [who.slice(5)] : []
  const permitted = policy.permits.some((permit) => permit.operation === operation.id && roles.includes(permit.role))
  step('Role', isPublic ? 'not applicable' : permitted ? 'A held role has permission.' : '403: no permission', !isPublic && !permitted, !isPublic)
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
  step('Body', `${body}${headers ? `; declared headers: ${Object.keys(headers).join(', ')}` : ''}`)
  step('Record', 'recorded in the security log before forwarding (503 if it cannot be written)')
  const api = policy.apis.find((item) => item.id === operation.api)
  const upstream = policy.upstreams.find((item) => item.id === api?.upstream)
  step('Forward', `${operation.api} → ${upstream?.id ?? 'missing service address'} ${upstream?.dial ?? ''}`)
  return steps
}
