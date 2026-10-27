import type { Operation, Policy } from './api'
import { boxKeys, type BoxKey, type BoxKind, type Positions } from './designer-model'

export const layoutKey = 'eat40005.policy-designer.layout.v1'
export const widths = { role: 220, operation: 380, api: 200, upstream: 220 } as const

// Up to this many operations stand in one column. A larger policy splits
// them into several columns, so the whole policy keeps a shape that Fit can
// show legibly instead of one tall strip.
const singleColumnLimit = 12
const operationColumnStep = widths.operation + 60

// operationColumns orders the operations by API and splits them into
// columns of at most `rows`, about the square root of twice their number.
// An API starts a new column unless it fits below the previous one, so an
// API's operations stay together where they can.
function operationColumns(policy: Policy): Operation[][] {
  const groups = policy.apis.map((api) => policy.operations.filter((operation) => operation.api === api.id))
  groups.push(policy.operations.filter((operation) => !policy.apis.some((api) => api.id === operation.api)))
  if (policy.operations.length <= singleColumnLimit) return [groups.flat()]
  const rows = Math.max(8, Math.ceil(Math.sqrt(2 * policy.operations.length)))
  const columns: Operation[][] = [[]]
  for (const group of groups) {
    if (!group.length) continue
    if (columns[columns.length - 1].length + group.length > rows && columns[columns.length - 1].length) columns.push([])
    for (const operation of group) {
      if (columns[columns.length - 1].length === rows) columns.push([])
      columns[columns.length - 1].push(operation)
    }
  }
  return columns
}

// columnStarts gives where each kind of box starts: roles, the first
// operation column, APIs after the last operation column, then upstreams.
function columnStarts(operationColumnCount: number): Record<BoxKind, number> {
  const api = 300 + operationColumnCount * operationColumnStep
  return { role: 0, operation: 300, api, upstream: api + 280 }
}

export function columnX(policy: Policy): Record<BoxKind, number> {
  return columnStarts(operationColumns(policy).length)
}

export function automaticLayout(policy: Policy, heights: Record<string, number> = {}): Positions {
  const operations = operationColumns(policy)
  const x = columnStarts(operations.length)
  const upstreamOrder = [
    ...new Set([...policy.apis.map((api) => api.upstream), ...policy.upstreams.map((upstream) => upstream.id)]),
  ].filter((id) => policy.upstreams.some((upstream) => upstream.id === id))
  const columns: { x: number; keys: BoxKey[] }[] = [
    { x: x.role, keys: policy.roles.map((role) => `role:${role.id}` as const) },
    ...operations.map((column, index) => ({
      x: x.operation + index * operationColumnStep,
      keys: column.map((operation) => `operation:${operation.id}` as const),
    })),
    { x: x.api, keys: policy.apis.map((api) => `api:${api.id}` as const) },
    { x: x.upstream, keys: upstreamOrder.map((id) => `upstream:${id}` as const) },
  ]
  const height = (key: string) => heights[key] ?? (key.startsWith('operation:') ? 176 : key.startsWith('role:') ? 96 : 72)
  const totals = columns.map(
    (column) => column.keys.reduce((total, key) => total + height(key), 0) + Math.max(0, column.keys.length - 1) * 24,
  )
  const tallest = Math.max(0, ...totals)
  const positions: Positions = {}
  columns.forEach((column, index) => {
    let y = (tallest - totals[index]) / 2
    for (const key of column.keys) {
      positions[key] = { x: column.x, y }
      y += height(key) + 24
    }
  })
  return positions
}

export function readPositions(): Positions {
  try {
    const raw = localStorage.getItem(layoutKey)
    if (!raw) return {}
    const value: unknown = JSON.parse(raw)
    if (!value || typeof value !== 'object' || Array.isArray(value)) return {}
    const item = value as Record<string, unknown>
    if (
      item.version !== 1 ||
      Object.keys(item).sort().join(',') !== 'positions,version' ||
      !item.positions ||
      typeof item.positions !== 'object' ||
      Array.isArray(item.positions)
    )
      return {}
    const positions = item.positions as Record<string, unknown>
    for (const [key, point] of Object.entries(positions)) {
      if (!/^(role|operation|api|upstream):/.test(key) || !point || typeof point !== 'object' || Array.isArray(point)) return {}
      const p = point as Record<string, unknown>
      if (
        Object.keys(p).sort().join(',') !== 'x,y' ||
        typeof p.x !== 'number' ||
        typeof p.y !== 'number' ||
        !Number.isFinite(p.x) ||
        !Number.isFinite(p.y)
      )
        return {}
    }
    return positions as Positions
  } catch {
    return {}
  }
}

export function writePositions(policy: Policy, positions: Positions): void {
  const known = new Set<string>(boxKeys(policy))
  const clean = Object.fromEntries(
    Object.entries(positions)
      .filter(([key, point]) => known.has(key) && Number.isFinite(point.x) && Number.isFinite(point.y))
      .map(([key, point]) => [key, { x: point.x, y: point.y }]),
  )
  try {
    localStorage.setItem(layoutKey, JSON.stringify({ version: 1, positions: clean }))
  } catch {
    /* Layout remains usable without storage. */
  }
}
export function forgetPositions(): void {
  try {
    localStorage.removeItem(layoutKey)
  } catch {
    /* Automatic layout does not need storage. */
  }
}
