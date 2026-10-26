import type { Policy } from './api'
import { boxKeys, type BoxKey, type Positions } from './designer-model'

export const layoutKey = 'eat40005.policy-designer.layout.v1'
export const widths = { role: 220, operation: 380, api: 200, upstream: 220 } as const

export function automaticLayout(policy: Policy, heights: Record<string, number> = {}): Positions {
  const operationOrder = policy.apis.flatMap((api) => policy.operations.filter((operation) => operation.api === api.id))
  operationOrder.push(...policy.operations.filter((operation) => !policy.apis.some((api) => api.id === operation.api)))
  const upstreamOrder = [
    ...new Set([...policy.apis.map((api) => api.upstream), ...policy.upstreams.map((upstream) => upstream.id)]),
  ].filter((id) => policy.upstreams.some((upstream) => upstream.id === id))
  const columns: { x: number; keys: BoxKey[] }[] = [
    { x: 0, keys: policy.roles.map((role) => `role:${role.id}` as const) },
    { x: 300, keys: operationOrder.map((operation) => `operation:${operation.id}` as const) },
    { x: 740, keys: policy.apis.map((api) => `api:${api.id}` as const) },
    { x: 1020, keys: upstreamOrder.map((id) => `upstream:${id}` as const) },
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
