import type { Operation } from '../shared/api'
import { TableScroll } from '../shared/ui'

// An endpoint's body and header rules, stated in words.

export function checksText(o: Operation): string {
  const checks = []
  if (o.body) checks.push(`JSON body, at most ${o.body.max_bytes} bytes`)
  if (o.headers) checks.push('headers')
  return checks.length > 0 ? checks.join('; ') : 'no body accepted'
}

type Schema = Record<string, unknown>

interface Rule {
  field: string
  place: 'body' | 'header'
  type: string
  // 'each' marks the rule every element of an array must meet.
  required: boolean | 'each'
  limits: string
}

function isSchema(value: unknown): value is Schema {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

// limitsText states a schema node's constraints in words. The gateway accepts
// only these keywords, so nothing in a node goes unstated.
function limitsText(node: Schema): string {
  const limits: string[] = []
  if (Array.isArray(node.enum)) limits.push(`one of ${node.enum.map((v) => JSON.stringify(v)).join(', ')}`)
  const range = (min: unknown, max: unknown, unit: string) => {
    if (min !== undefined && max !== undefined) limits.push(`${min} to ${max}${unit}`)
    else if (min !== undefined) limits.push(`at least ${min}${unit}`)
    else if (max !== undefined) limits.push(`at most ${max}${unit}`)
  }
  range(node.minLength, node.maxLength, ' characters')
  range(node.minimum, node.maximum, '')
  range(node.minItems, node.maxItems, ' items')
  if (hasType(node, 'object') && node.additionalProperties === false) limits.push('no other members')
  return limits.join('; ')
}

function typeText(node: Schema): string {
  return Array.isArray(node.type) ? node.type.join(' or ') : String(node.type ?? 'any')
}

// hasType tells whether a schema node allows a type, named alone or in a list.
function hasType(node: Schema, type: string): boolean {
  return node.type === type || (Array.isArray(node.type) && node.type.includes(type))
}

// rules lists the members a schema declares, following nested objects and
// the elements of arrays (written field[]).
function rules(schema: unknown, place: Rule['place'], prefix = ''): Rule[] {
  if (!isSchema(schema) || !isSchema(schema.properties)) return []
  const required = Array.isArray(schema.required) ? schema.required : []
  const list: Rule[] = []
  for (const [name, node] of Object.entries(schema.properties)) {
    if (!isSchema(node)) continue
    const field = `${prefix}${name}`
    list.push({ field, place, type: typeText(node), required: required.includes(name), limits: limitsText(node) })
    list.push(...within(node, place, field))
  }
  return list
}

// within lists what a member's own schema declares: its object members and,
// when it may be an array, its elements, which may be arrays in turn.
function within(node: Schema, place: Rule['place'], field: string): Rule[] {
  const list = rules(node, place, `${field}.`)
  if (hasType(node, 'array') && isSchema(node.items)) {
    const element = `${field}[]`
    list.push({ field: element, place, type: typeText(node.items), required: 'each', limits: limitsText(node.items) })
    list.push(...within(node.items, place, element))
  }
  return list
}

// RequestRules states an endpoint's body and header rules in words. It
// describes the policy only: the gateway's compiler and request check stay
// the only validator.
export function RequestRules({ operation: o }: { operation: Operation }) {
  const body = o.body ? (o.body.schema as Schema) : undefined
  const headers = o.headers as Schema | undefined
  const list = [...rules(body, 'body'), ...rules(headers, 'header')]
  const closed = (schema?: Schema) => (schema?.additionalProperties === false ? 'refused' : 'accepted')
  return (
    <div className="rule-block">
      <h3>
        <code>{o.id}</code>{' '}
        <code className="route">
          <span className="method">{o.method}</span> {o.target}
        </code>
      </h3>
      <p className="hint">
        {body ? `JSON body (application/json), at most ${o.body!.max_bytes} bytes, no repeated members; members not listed are ${closed(body)}.` : 'No body accepted.'}
        {headers ? ' Each listed header may appear once; headers not listed are not checked by this rule.' : ''}
      </p>
      <TableScroll label={`Request rules of ${o.id}`}>
        <table>
          <thead>
            <tr>
              <th scope="col">Field</th>
              <th scope="col">In</th>
              <th scope="col">Type</th>
              <th scope="col">Required</th>
              <th scope="col">Constraints</th>
            </tr>
          </thead>
          <tbody>
            {list.map((r) => (
              <tr key={`${r.place}:${r.field}`}>
                <td>
                  <code>{r.field}</code>
                </td>
                <td>{r.place}</td>
                <td>
                  <code>{r.type}</code>
                </td>
                <td>{r.required === 'each' ? 'every element' : r.required ? 'required' : 'optional'}</td>
                <td>{r.limits || <span className="muted">none</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </TableScroll>
    </div>
  )
}
