import type { Policy } from './api'
import { copy } from './designer-model'

// Prepared Designer edits: the templates and the step-by-step wizards' changes.

// Templates prepare a draft from the document being edited. Each returns the
// new document and a sentence saying what it changed, or why it changed
// nothing. Nothing is saved: the draft still goes through the gateway's check,
// a trial and keeping, like any other change.
export type TemplateResult = { document: Policy; summary: string } | { problem: string }

export function limitSignIn(policy: Policy): TemplateResult {
  const next = copy(policy)
  const login = next.operations.find((operation) => operation.id === 'accounts.login')
  if (!login) return { problem: 'This policy has no accounts.login endpoint.' }
  login.rate = { algorithm: 'sliding-window', count: 5, window_seconds: 60, key: 'client_address' }
  return { document: next, summary: 'accounts.login limited to 5 requests per 60 s per client address.' }
}

export function readOnlyAccess(policy: Policy, role: string, api: string): TemplateResult {
  const next = copy(policy)
  const reads = next.operations.filter((operation) => operation.api === api && operation.method === 'GET' && operation.auth !== 'none')
  if (reads.length === 0) return { problem: `API ${api} has no read endpoint that needs a sign-in.` }
  const missing = reads.filter((operation) => !next.permits.some((permit) => permit.role === role && permit.operation === operation.id))
  for (const operation of missing) next.permits.push({ role, operation: operation.id })
  return { document: next, summary: `${role} may read API ${api}: ${missing.length} of its ${reads.length} read endpoints added.` }
}

export function removeApiAccess(policy: Policy, role: string, api: string): TemplateResult {
  const next = copy(policy)
  const endpoints = new Set(next.operations.filter((operation) => operation.api === api).map((operation) => operation.id))
  const kept = next.permits.filter((permit) => !(permit.role === role && endpoints.has(permit.operation)))
  const removed = next.permits.length - kept.length
  if (removed === 0) return { problem: `${role} has no permission on API ${api}.` }
  next.permits = kept
  return { document: next, summary: `${role} loses ${removed} ${removed === 1 ? 'permission' : 'permissions'} on API ${api}.` }
}

export function tightenRate(policy: Policy, endpoint: string, count: number): TemplateResult {
  const next = copy(policy)
  const operation = next.operations.find((item) => item.id === endpoint)
  if (!operation) return { problem: `This policy has no endpoint ${endpoint}.` }
  if (!Number.isInteger(count) || count < 1) return { problem: 'Enter a whole number of requests, at least 1.' }
  if (operation.rate && count >= operation.rate.count)
    return { problem: `${endpoint} already allows ${operation.rate.count} per window; enter a lower number.` }
  const key = operation.auth === 'none' ? 'client_address' : 'subject'
  operation.rate = operation.rate ? { ...operation.rate, count } : { algorithm: 'sliding-window', count, window_seconds: 60, key }
  return { document: next, summary: `${endpoint} limited to ${count} requests per ${operation.rate.window_seconds} s.` }
}

// The step-by-step wizards build their edit with these. Each returns a new
// document and leaves the one being edited untouched; the gateway still checks
// the result when the draft is saved.
export function grantAccess(policy: Policy, role: string, endpoints: string[]): Policy {
  const next = copy(policy)
  for (const operation of endpoints)
    if (!next.permits.some((permit) => permit.role === role && permit.operation === operation)) next.permits.push({ role, operation })
  return next
}

export function setRate(policy: Policy, endpoint: string, count: number, windowSeconds: number): Policy {
  const next = copy(policy)
  const operation = next.operations.find((item) => item.id === endpoint)
  if (!operation) return next
  const key = operation.rate?.key ?? (operation.auth === 'none' ? 'client_address' : 'subject')
  operation.rate = { algorithm: 'sliding-window', count, window_seconds: windowSeconds, key }
  return next
}

interface NewEndpoint {
  id: string
  api: string
  method: string
  target: string
  open: boolean
  roles: string[]
  count: number
  windowSeconds: number
}

export function addEndpoint(policy: Policy, endpoint: NewEndpoint): Policy {
  const next = copy(policy)
  next.operations.push({
    id: endpoint.id,
    api: endpoint.api,
    method: endpoint.method,
    target: endpoint.target,
    access: 'allow',
    auth: endpoint.open ? 'none' : 'required',
    rate: {
      algorithm: 'sliding-window',
      count: endpoint.count,
      window_seconds: endpoint.windowSeconds,
      key: endpoint.open ? 'client_address' : 'subject',
    },
  })
  if (!endpoint.open) for (const role of endpoint.roles) next.permits.push({ role, operation: endpoint.id })
  return next
}
