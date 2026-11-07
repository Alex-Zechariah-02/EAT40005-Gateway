import type { MenuAction } from '../../shared/overlays'
import type { ControlChange, Operation, Policy } from '../../shared/api'

// The Designer's draft: box kinds and keys, its local checks, and rename, add and remove.

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
    apis: [{ id: 'string', upstream: 'string' }, { forward: 'object', cors: 'object', response_headers: 'object', allow_addresses: 'array' }],
    operations: [
      { id: 'string', api: 'string', method: 'string', target: 'string', access: 'string', auth: 'string' },
      { body: 'object', headers: 'object', rate: 'object', when: 'object' },
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
          if (!error && item.when !== undefined) {
            error = members(item.when, {}, { hosts: 'array', headers: 'array' })
            if (error) field += '.when'
          }
        }
        if (section === 'apis' && item.forward !== undefined) {
          error = members(item.forward, {}, { strip_prefix: 'string', add_prefix: 'string' })
          if (error) field += '.forward'
        }
        if (section === 'apis' && !error && item.cors !== undefined) {
          error = members(item.cors, { origins: 'array', methods: 'array' }, { headers: 'array', expose: 'array', credentials: 'boolean', max_age_seconds: 'number' })
          if (error) field += '.cors'
        }
        if (section === 'apis' && !error && item.response_headers !== undefined) {
          error = members(item.response_headers, {}, { hsts: 'boolean', nosniff: 'boolean', frame: 'string', referrer: 'string', remove: 'array' })
          if (error) field += '.response_headers'
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

// GroupAction is one choice of a group's + menu; adds names the kind of box
// it adds directly, so the list can open the new box.
export type GroupAction = MenuAction & { adds?: BoxKind }
