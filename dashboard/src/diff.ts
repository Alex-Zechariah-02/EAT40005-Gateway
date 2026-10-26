import type { Policy } from './api'

// Differences between two policy documents, computed in the browser to help
// an administrator review a candidate or an earlier revision. It is a reading
// aid only: the gateway validates every document with its own compiler and
// reports the controls it changes once a candidate is provisional.

export interface Change {
  kind: 'added' | 'changed' | 'removed'
  // text names the item, as "section: kind name".
  text: string
  // details list the members that differ, for a changed item.
  details: string[]
}

type Item = Record<string, unknown>

const sections = ['upstreams', 'apis', 'operations', 'roles', 'permits', 'subjects'] as const

// canonical writes a JSON value with object members sorted, so the order in
// which a document was written never shows as a difference.
function canonical(value: unknown): string {
  return JSON.stringify(value, (_, v: unknown) =>
    v && typeof v === 'object' && !Array.isArray(v) ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : 1))) : v,
  )
}

function isObject(value: unknown): value is Item {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function shown(value: unknown): string {
  if (value === undefined) return 'none'
  if (Array.isArray(value) && value.every((v) => typeof v === 'string')) return value.length > 0 ? value.join(', ') : 'none'
  const text = canonical(value)
  return text.length > 80 ? `${text.slice(0, 77)}…` : text
}

// members lists the members of two versions of one item that differ, with
// nested objects such as a rate rule or a schema followed member by member.
function members(before: Item, after: Item, path = '', depth = 0): string[] {
  const names = [...new Set([...Object.keys(before), ...Object.keys(after)])].sort()
  const lines: string[] = []
  for (const name of names) {
    const old = before[name]
    const now = after[name]
    if (canonical(old) === canonical(now)) continue
    const where = `${path}${name}`
    if (isObject(old) && isObject(now) && depth < 4) {
      lines.push(...members(old, now, `${where}.`, depth + 1))
    } else if (old === undefined) {
      lines.push(`${where} added: ${shown(now)}`)
    } else if (now === undefined) {
      lines.push(`${where} removed (was ${shown(old)})`)
    } else {
      lines.push(`${where}: ${shown(old)} → ${shown(now)}`)
    }
  }
  return lines
}

export function differences(before: Policy, after: Policy): Change[] {
  const changes: Change[] = []
  for (const section of sections) {
    const name = (item: Item) => (section === 'permits' ? `${item.role} → ${item.operation}` : String(item.id))
    const old = new Map(((before[section] as unknown as Item[]) ?? []).map((item) => [name(item), item]))
    const now = new Map(((after[section] as unknown as Item[]) ?? []).map((item) => [name(item), item]))
    for (const [id, item] of now) {
      const previous = old.get(id)
      if (!previous) {
        changes.push({ kind: 'added', text: `${section}: added ${id}`, details: [] })
      } else if (canonical(previous) !== canonical(item)) {
        changes.push({ kind: 'changed', text: `${section}: changed ${id}`, details: members(previous, item) })
      }
    }
    for (const id of old.keys()) {
      if (!now.has(id)) changes.push({ kind: 'removed', text: `${section}: removed ${id}`, details: [] })
    }
  }
  if (before.profile !== after.profile || before.schema_version !== after.schema_version) {
    changes.push({
      kind: 'changed',
      text: 'profile or schema version changed',
      details: members({ profile: before.profile, schema_version: before.schema_version }, { profile: after.profile, schema_version: after.schema_version }),
    })
  }
  return changes
}
