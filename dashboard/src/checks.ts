// The gateway's request checks, in the fixed order it applies them, with one
// vocabulary for their results and an explanation of each check.

export type Stage = 'route' | 'access' | 'token' | 'blocked' | 'permit' | 'rate' | 'validation' | 'record' | 'forward'

interface Check {
  id: Stage
  name: string
  // plain says what the check does, for an administrator new to policies.
  plain: string
  // technical says how it works and what the gateway answers when it refuses.
  technical: string
}

export const stages: Check[] = [
  {
    id: 'route',
    name: 'Route',
    plain: 'The gateway finds the endpoint that matches the request method and path. If no endpoint matches, it refuses the request.',
    technical: 'Matches against the active policy version; the most specific target wins. Refusals: 404 not_found, 405 method_not_allowed (with an Allow header).',
  },
  {
    id: 'access',
    name: 'Access',
    plain: 'The policy can close an endpoint to all callers. A closed endpoint refuses every request, also from signed-in accounts.',
    technical: 'Endpoint access "deny" refuses with 403 forbidden. Default deny: only endpoints set to "allow" continue.',
  },
  {
    id: 'token',
    name: 'Sign-In',
    plain: 'The request must carry a valid sign-in token from the trusted sign-in service. Public endpoints do not need one.',
    technical: 'RS256 JWT: signature by key ID (kid) from a trusted issuer, issuer, audience and expiry. Refusal: 401 unauthenticated. Skipped for auth "none".',
  },
  {
    id: 'blocked',
    name: 'Blocked List',
    plain: 'The gateway refuses an account that an administrator has blocked. A block takes effect at once, also when the token is still valid.',
    technical: 'The token subject is checked against the blocked-accounts list, which is separate from the policy. Refusal: 403 account_blocked.',
  },
  {
    id: 'permit',
    name: 'Role',
    plain: 'One of the account’s roles must have permission to call this endpoint.',
    technical: 'RBAC: roles come only from the verified token and count only if the policy defines them. Refusal: 403 forbidden.',
  },
  {
    id: 'rate',
    name: 'Rate',
    plain: 'Each account can send only a set number of requests in a period. Public endpoints count requests per client address.',
    technical: 'Weighted sliding-window limit per account, or per client IP for public endpoints. Refusal: 429 rate_limited.',
  },
  {
    id: 'validation',
    name: 'Body',
    plain: 'The gateway makes sure the request has the expected form: a JSON body of the permitted size, with the required fields and no unknown fields.',
    technical: 'Bounded JSON Schema validation, Content-Type and size limits, duplicate members; malformed requests are refused before routing. Refusals: 400 bad_request, 413, 415.',
  },
  {
    id: 'record',
    name: 'Record',
    plain: 'The gateway writes the request to the security log before it sends it on. If it cannot write the record, it refuses the request, so no request passes unrecorded.',
    technical: 'Durable request.admitted audit write, fail closed. Refusal: 503 gateway_unavailable. Also refused here when no active policy is loaded.',
  },
  {
    id: 'forward',
    name: 'Forward',
    plain: 'The gateway sends the request to the service address of the endpoint’s API and returns the answer.',
    technical: 'Reverse proxy to the bound service address only. Refusals: 502 upstream_unavailable; 504 upstream_timeout, where the outcome is unknown.',
  },
]

// The result of one check for one request, in the same words on every page.
export type StepState = 'passed' | 'refused' | 'unknown' | 'skipped' | 'unreached' | 'inapplicable'

export const stepWords: Record<StepState, string> = {
  passed: 'Passed',
  refused: 'Refused Here',
  unknown: 'Outcome Unknown',
  skipped: 'Not Checked',
  unreached: 'Not Reached',
  inapplicable: 'Not Applicable',
}

// The outcome of a whole request.
type Decision = 'allowed' | 'refused' | 'failed'

export const decisionWords: Record<Decision, string> = { allowed: 'Allowed', refused: 'Refused', failed: 'Outcome Unknown' }
