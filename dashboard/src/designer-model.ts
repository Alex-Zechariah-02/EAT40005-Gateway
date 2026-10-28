import type { ControlChange, Operation, Policy } from './api'

export type BoxKind = 'role' | 'operation' | 'api' | 'upstream'
export type BoxKey = `${BoxKind}:${string}`
export interface DesignerDraft {
  document: Policy
  base: Policy
  baseRevision: number
}
export interface Issue {
  id: string
  field: string
  message: string
  box?: BoxKey
}
export interface Position {
  x: number
  y: number
}
export type Positions = Record<string, Position>
export const sections = { role: 'roles', operation: 'operations', api: 'apis', upstream: 'upstreams' } as const
// The words the Designer shows for each kind of box. The policy's own field
// names (operation, upstream) stay in the JSON view.
export const kindNames: Record<BoxKind, { one: string; many: string }> = {
  role: { one: 'role', many: 'roles' },
  operation: { one: 'endpoint', many: 'endpoints' },
  api: { one: 'API', many: 'APIs' },
  upstream: { one: 'service address', many: 'service addresses' },
}
export function capital(text: string): string {
  return text[0].toUpperCase() + text.slice(1)
}

export function copy<T>(value: T): T {
  return structuredClone(value)
}
export function canonical(value: unknown): string {
  return JSON.stringify(value, (_, item: unknown) =>
    item && typeof item === 'object' && !Array.isArray(item)
      ? Object.fromEntries(Object.entries(item).sort(([a], [b]) => a.localeCompare(b, 'en')))
      : item,
  )
}
export function boxKeys(policy: Policy): BoxKey[] {
  return (Object.keys(sections) as BoxKind[]).flatMap((kind) => policy[sections[kind]].map(({ id }) => `${kind}:${id}` as BoxKey))
}
export function splitKey(key: BoxKey): [BoxKind, string] {
  const colon = key.indexOf(':')
  return [key.slice(0, colon) as BoxKind, key.slice(colon + 1)]
}

function object(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}
function members(value: unknown, required: Record<string, string>, optional: Record<string, string> = {}): string | null {
  if (!object(value)) return 'must be an object'
  for (const [name, type] of Object.entries(required)) {
    if (!(name in value)) return `${name} is required`
    if (!primitive(value[name], type)) return `${name} must be ${type}`
  }
  for (const [name, item] of Object.entries(value)) {
    const type = required[name] ?? optional[name]
    if (!type) return `${name} is not a recognised member`
    if (!primitive(item, type)) return `${name} must be ${type}`
  }
  return null
}
function primitive(value: unknown, type: string): boolean {
  if (type === 'array') return Array.isArray(value)
  if (type === 'object') return object(value)
  return typeof value === type
}

// These checks only keep the document drawable. Formats, ranges, schema
// keywords and security semantics are validated exclusively by the Gateway.
export function shapeIssue(value: unknown): Issue | null {
  const root = members(value, {
    schema_version: 'number',
    profile: 'string',
    upstreams: 'array',
    apis: 'array',
    operations: 'array',
    roles: 'array',
    permits: 'array',
  })
  if (root) return { id: 'shape', field: '$', message: root }
  const policy = value as unknown as Policy
  const definitions: Record<string, [Record<string, string>, Record<string, string>?]> = {
    upstreams: [{ id: 'string', dial: 'string' }],
    apis: [{ id: 'string', upstream: 'string' }],
    operations: [
      { id: 'string', api: 'string', method: 'string', target: 'string', access: 'string', auth: 'string' },
      { body: 'object', headers: 'object', rate: 'object' },
    ],
    roles: [{ id: 'string' }],
    permits: [{ role: 'string', operation: 'string' }],
  }
  for (const [section, [required, optional]] of Object.entries(definitions)) {
    const items = (policy[section as keyof Policy] as unknown[] | undefined) ?? []
    for (let index = 0; index < items.length; index++) {
      const item = items[index]
      let error = members(item, required, optional)
      let field = `${section}[${index}]`
      if (!error && object(item)) {
        if (section === 'operations') {
          if (item.rate !== undefined) {
            error = members(item.rate, { algorithm: 'string', count: 'number', window_seconds: 'number', key: 'string' })
            if (error) field += '.rate'
          }
          if (!error && item.body !== undefined) {
            error = members(item.body, { max_bytes: 'number', schema: 'object' })
            if (error) field += '.body'
          }
        }
      }
      if (error) return { id: 'shape', field, message: error }
    }
  }
  return null
}

export function localIssues(policy: Policy): Issue[] {
  const shape = shapeIssue(policy)
  if (shape) return [shape]
  const issues: Issue[] = []
  for (const kind of Object.keys(sections) as BoxKind[]) {
    const seen = new Set<string>()
    policy[sections[kind]].forEach(({ id }, index) => {
      if (!id || seen.has(id))
        issues.push({
          id: 'unique_id',
          field: `${sections[kind]}[${index}].id`,
          message: 'Use a non-empty ID that is unique in this kind.',
          box: `${kind}:${id}`,
        })
      seen.add(id)
    })
  }
  policy.operations.forEach((operation, index) => {
    if (!policy.apis.some((api) => api.id === operation.api))
      issues.push({
        id: 'reference',
        field: `operations[${index}].api`,
        message: 'Choose an API in this document.',
        box: `operation:${operation.id}`,
      })
    if (operation.auth === 'none' && policy.permits.some((permit) => permit.operation === operation.id))
      issues.push({
        id: 'public_rule',
        field: `operations[${index}].auth`,
        message: 'Public endpoints cannot have a permission.',
        box: `operation:${operation.id}`,
      })
    if (operation.auth === 'none' && operation.rate && operation.rate.key !== 'client_address')
      issues.push({
        id: 'public_rule',
        field: `operations[${index}].rate`,
        message: 'A public endpoint can only be limited per client address.',
        box: `operation:${operation.id}`,
      })
  })
  policy.apis.forEach((api, index) => {
    if (!policy.upstreams.some((upstream) => upstream.id === api.upstream))
      issues.push({ id: 'reference', field: `apis[${index}].upstream`, message: 'Choose a service address in this document.', box: `api:${api.id}` })
  })
  policy.upstreams.forEach((upstream, index) => {
    if (!upstream.dial)
      issues.push({ id: 'dial', field: `upstreams[${index}].dial`, message: 'Enter the service address.', box: `upstream:${upstream.id}` })
  })
  return issues
}

function identities(policy: Policy): Map<string, unknown> {
  const items = new Map<string, unknown>()
  for (const kind of Object.keys(sections) as BoxKind[]) {
    for (const item of policy[sections[kind]]) {
      if (kind === 'operation') {
        const { rate, ...operation } = item as Operation
        items.set(`operation:${item.id}`, operation)
        if (rate) items.set(`rate:${item.id}`, rate)
      } else items.set(`${kind}:${item.id}`, item)
    }
  }
  for (const permit of policy.permits) items.set(`permit:${permit.role}:${permit.operation}`, permit)
  return items
}
export function changes(before: Policy, after: Policy): ControlChange[] {
  const old = identities(before)
  const next = identities(after)
  return [...new Set([...old.keys(), ...next.keys()])]
    .filter((key) => canonical(old.get(key)) !== canonical(next.get(key)))
    .map((key) => {
      const colon = key.indexOf(':')
      return { control: key.slice(0, colon), target: key.slice(colon + 1) }
    })
}

export function connectionReason(policy: Policy, source: string, target: string): string | null {
  const [from, fromId] = splitKey(source as BoxKey)
  const [to, toId] = splitKey(target as BoxKey)
  if (from === 'role' && to === 'operation') {
    const operation = policy.operations.find((item) => item.id === toId)
    if (operation?.auth === 'none') return 'This endpoint is public (no sign-in), so no role can be given permission for it.'
    if (policy.permits.some((permit) => permit.role === fromId && permit.operation === toId))
      return 'This role already has permission for this endpoint.'
    return null
  }
  if ((from === 'operation' && to === 'api') || (from === 'api' && to === 'upstream')) return null
  return 'Arrows go roles → operations → APIs → upstreams.'
}
export function connect(policy: Policy, source: BoxKey, target: BoxKey): Policy {
  if (connectionReason(policy, source, target)) return policy
  const next = copy(policy)
  const [kind, id] = splitKey(source)
  const [, destination] = splitKey(target)
  if (kind === 'role') next.permits.push({ role: id, operation: destination })
  if (kind === 'operation') next.operations.find((item) => item.id === id)!.api = destination
  if (kind === 'api') next.apis.find((item) => item.id === id)!.upstream = destination
  return next
}
export function rename(policy: Policy, key: BoxKey, id: string): Policy {
  const next = copy(policy)
  const [kind, old] = splitKey(key)
  next[sections[kind]].find((item) => item.id === old)!.id = id
  if (kind === 'role') {
    next.permits.forEach((permit) => {
      if (permit.role === old) permit.role = id
    })
  }
  if (kind === 'operation')
    next.permits.forEach((permit) => {
      if (permit.operation === old) permit.operation = id
    })
  if (kind === 'api')
    next.operations.forEach((operation) => {
      if (operation.api === old) operation.api = id
    })
  if (kind === 'upstream')
    next.apis.forEach((api) => {
      if (api.upstream === old) api.upstream = id
    })
  return next
}
export function deleteReason(policy: Policy, key: BoxKey): string | null {
  const [kind, id] = splitKey(key)
  if (kind === 'api' && policy.operations.some((operation) => operation.api === id)) return 'Move its endpoints to another API first.'
  if (kind === 'upstream' && policy.apis.some((api) => api.upstream === id)) return 'Point its APIs at another service address first.'
  return null
}
export function remove(policy: Policy, key: BoxKey): Policy {
  if (deleteReason(policy, key)) return policy
  const next = copy(policy)
  const [kind, id] = splitKey(key)
  const section = sections[kind]
  // Filtering preserves every surviving object's document order.
  if (section === 'roles') next.roles = next.roles.filter((item) => item.id !== id)
  if (section === 'operations') next.operations = next.operations.filter((item) => item.id !== id)
  if (section === 'apis') next.apis = next.apis.filter((item) => item.id !== id)
  if (section === 'upstreams') next.upstreams = next.upstreams.filter((item) => item.id !== id)
  if (kind === 'role') next.permits = next.permits.filter((permit) => permit.role !== id)
  if (kind === 'operation') next.permits = next.permits.filter((permit) => permit.operation !== id)
  return next
}
export function add(policy: Policy, kind: BoxKind): { document: Policy; key: BoxKey } | null {
  if ((kind === 'operation' && !policy.apis.length) || (kind === 'api' && !policy.upstreams.length)) return null
  const next = copy(policy)
  const prefix = kind === 'role' ? 'ROLE_' : `${kind}-`
  let index = 1
  while (next[sections[kind]].some((item) => item.id === `${prefix}${index}`)) index++
  const id = `${prefix}${index}`
  if (kind === 'role') next.roles.push({ id })
  if (kind === 'upstream') next.upstreams.push({ id, dial: '' })
  if (kind === 'api') next.apis.push({ id, upstream: next.upstreams[0].id })
  if (kind === 'operation')
    next.operations.push({
      id,
      api: next.apis[0].id,
      method: 'GET',
      target: '/new-path',
      access: 'deny',
      auth: 'required',
      rate: { algorithm: 'sliding-window', count: 100, window_seconds: 60, key: 'subject' },
    })
  return { document: next, key: `${kind}:${id}` }
}
export function fieldBox(policy: Policy, field: string): BoxKey | null {
  const match = /^(upstreams|apis|operations|roles)\[(\d+)\]/.exec(field)
  if (!match) return null
  const section = match[1] as (typeof sections)[BoxKind]
  const kind = (Object.keys(sections) as BoxKind[]).find((key) => sections[key] === section)!
  const item = policy[section][Number(match[2])]
  return item ? `${kind}:${item.id}` : null
}

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

export interface NewEndpoint {
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
