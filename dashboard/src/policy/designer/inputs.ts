import type { Policy } from '../../shared/api'
import type { NewEndpoint } from './edits'
import { wholeNumber, withBody } from './wizards'

// Connect an API's input: one endpoint row and the checks of each field's form.
// Whether the policy is valid stays the gateway compiler's answer on saving.

export interface Row {
  key: number
  method: string
  target: string
  // id is null until typed; until then it follows the API and the path.
  id: string | null
  open: boolean
  roles: string[]
  count: string
  seconds: string
  checked: boolean
  bytes: string
  schema: string
}

const starter = '{\n  "type": "object",\n  "properties": {},\n  "additionalProperties": false\n}'

export function blank(key: number): Row {
  return { key, method: 'GET', target: '', id: null, open: false, roles: [], count: '100', seconds: '60', checked: false, bytes: '4096', schema: starter }
}

// suggestedId names an endpoint after its API and the path's last fixed part.
export function suggestedId(api: string, target: string): string {
  const segment =
    target
      .split('/')
      .filter((part) => part && !part.startsWith('{') && part !== '*')
      .pop() ?? ''
  return segment ? `${api}.${segment}` : ''
}

// rowId is the row's endpoint ID: the one typed, or the suggested one.
export function rowId(api: string, row: Row): string {
  return row.id ?? suggestedId(api, row.target)
}

export function writes(row: Row): boolean {
  return withBody.has(row.method)
}

// Policy IDs are letters and digits, with dots, hyphens and underscores after the first.
const idForm = /^[A-Za-z0-9][A-Za-z0-9._-]*$/

export function idProblem(id: string, taken: string | undefined): string | undefined {
  if (!id) return 'Enter an ID.'
  if (id.length > 64) return 'Use at most 64 characters.'
  if (!idForm.test(id)) return 'Use letters, digits, dots, hyphens and underscores, starting with a letter or digit.'
  return taken
}

export function dialProblem(dial: string): string | undefined {
  const match = /^(\[[0-9A-Fa-f:.]+\]|[^\s:[\]]+):(\d{1,5})$/.exec(dial)
  const port = Number(match?.[2])
  return match && port >= 1 && port <= 65535 ? undefined : 'Enter the address as host:port, for example 127.0.0.1:9600.'
}

export function pathProblem(target: string): string | undefined {
  if (!target.startsWith('/')) return 'Enter a path starting with /.'
  if (/\s/.test(target)) return 'A path cannot contain spaces.'
  return undefined
}

export function countProblem(text: string): string | undefined {
  return wholeNumber(text) ? undefined : 'Enter a whole number of at least 1.'
}

// schemaValue reads a schema's text as one JSON object, or null; its rules are the gateway's to check.
function schemaValue(text: string): Record<string, unknown> | null {
  try {
    const value: unknown = JSON.parse(text)
    return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : null
  } catch {
    return null
  }
}

const schemaProblem = 'Enter well-formed JSON: one object, such as {"type": "object"}.'

// rowErrors names each field of one row whose form is wrong, keyed by field.
export function rowErrors(policy: Policy, api: string, rows: Row[], index: number): Record<string, string | undefined> {
  const row = rows[index]
  const id = rowId(api, row)
  const repeated = rows.slice(0, index).some((other) => rowId(api, other) === id)
  const taken = policy.operations.some((operation) => operation.id === id)
    ? 'This draft already has an endpoint with this ID.'
    : repeated
      ? 'Another endpoint in this flow has this ID.'
      : undefined
  return {
    target: pathProblem(row.target),
    id: idProblem(id, taken),
    count: countProblem(row.count),
    seconds: countProblem(row.seconds),
    bytes: row.checked && writes(row) ? countProblem(row.bytes) : undefined,
    schema: row.checked && writes(row) && !schemaValue(row.schema) ? schemaProblem : undefined,
  }
}

// endpoints turns rows whose fields all passed their checks into the draft's new endpoints.
export function endpoints(api: string, rows: Row[]): NewEndpoint[] {
  return rows.map((row) => {
    const schema = row.checked && writes(row) ? schemaValue(row.schema) : null
    return {
      id: rowId(api, row),
      api,
      method: row.method,
      target: row.target,
      open: row.open,
      roles: row.open ? [] : row.roles,
      count: Number(row.count),
      windowSeconds: Number(row.seconds),
      body: schema ? { max_bytes: Number(row.bytes), schema } : undefined,
    }
  })
}
