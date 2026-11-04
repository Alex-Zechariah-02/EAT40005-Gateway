import type { ReactNode } from 'react'
import { MarkerType, type Edge } from '@xyflow/react'
import type { ControlChange, Operation, Override, Policy } from '../../shared/api'
import { capital, splitKey, type BoxKey, type BoxKind, type GroupAction, type Issue } from './model'
import { type CardSpec, type Row } from './card'
import { time } from '../../shared/time'

// What the canvas draws: its cards and connections, built from the draft and what overlays it.

export interface TracePath {
  operation: string
  roles: string[]
  forward: boolean
}
export interface CanvasProps {
  policy: Policy
  editable: boolean
  selection: BoxKey | null
  issues: Issue[]
  overlays: Override[]
  provisional?: { revision: number; changes: ControlChange[] }
  refused?: { box?: BoxKey; edge?: string; code: string }
  trace?: TracePath
  expanded: boolean
  // panelOpen is set while the side form or the trace covers the canvas's
  // right side; the canvas then moves the selected row out from under it.
  panelOpen: boolean
  onExpand: () => void
  onSelect: (key: BoxKey) => void
  onDelete: (key: BoxKey) => void
  // actions are what a group's + offers; none while the policy is read-only.
  actions: (kind: BoxKind, api?: string) => GroupAction[]
  // tools are the page's canvas-only controls, shown with the view controls.
  tools?: ReactNode
}

// buildGraph turns the draft and what overlays it into the canvas cards and
// their connections.
export function buildGraph(props: CanvasProps, zone: string): { cards: CardSpec[]; edges: Edge[] } {
  const { policy } = props
  const selectedRole = props.selection?.startsWith('role:') ? splitKey(props.selection)[1] : undefined
  const changed = (control: string, target: string) =>
    props.provisional?.changes.some((item) => item.control === control && item.target === target)
  const refusedPermit = props.refused?.edge?.startsWith('permit:') ? props.refused.edge.split(':') : undefined
  const refusedOn = (key: BoxKey) =>
    props.refused?.box === key || (!!refusedPermit && (key === `role:${refusedPermit[1]}` || key === `operation:${refusedPermit[2]}`))
  const inTrace = (key: BoxKey) => {
    if (!props.trace) return true
    const [kind, id] = splitKey(key)
    if (kind === 'role') return props.trace.roles.includes(id)
    if (kind === 'operation') return props.trace.operation === id
    const operation = policy.operations.find((item) => item.id === props.trace?.operation)
    if (!props.trace.forward || !operation) return false
    return kind === 'api' ? operation.api === id : policy.apis.find((api) => api.id === operation.api)?.upstream === id
  }
  const badgesFor = (key: BoxKey) => {
    const badges: Row['badges'] = []
    if (props.issues.some((issue) => issue.box === key)) badges.push({ tone: 'pending', text: 'Incomplete' })
    if (refusedOn(key)) badges.push({ tone: 'refused', text: `Refused: ${props.refused!.code}` })
    return badges
  }
  const allowed = (role: string, operation: string) =>
    policy.permits.some((permit) => permit.role === role && permit.operation === operation) ||
    props.overlays.some(
      (override) =>
        override.control === 'permit' && override.type === 'exception' && override.role === role && override.operation === operation,
    )
  const trial = `Changed in the trial of version ${props.provisional?.revision}`

  const roleRows: Row[] = policy.roles.map((role) => {
    const key: BoxKey = `role:${role.id}`
    const count = policy.operations.filter((operation) => operation.auth !== 'none' && allowed(role.id, operation.id)).length
    const badges = badgesFor(key)
    if (changed('role', role.id)) badges.push({ tone: 'pending', text: trial })
    return {
      key,
      title: role.id || '(empty ID)',
      detail: `${count} ${count === 1 ? 'endpoint' : 'endpoints'}`,
      kind: 'Role',
      badges,
      dim: !inTrace(key) || (selectedRole !== undefined && selectedRole !== role.id),
    }
  })
  const operationRow = (operation: Operation): Row => {
    const key: BoxKey = `operation:${operation.id}`
    const badges = badgesFor(key)
    if (operation.auth === 'none') badges.push({ tone: 'neutral', text: 'Public' })
    if (operation.access === 'deny') badges.push({ tone: 'refused', text: 'Access: Deny' })
    const denied = props.overlays.find((override) => override.control === 'operation' && override.operation === operation.id)
    if (denied) badges.push({ tone: 'refused', text: `Access: Deny until ${time(denied.expires_at, zone)}` })
    const rate = props.overlays.find((override) => override.control === 'rate' && override.operation === operation.id)
    if (rate) badges.push({ tone: 'pending', text: `Limit ${rate.count} until ${time(rate.expires_at, zone)}` })
    for (const override of props.overlays.filter((item) => item.control === 'permit' && item.operation === operation.id))
      badges.push({
        tone: override.type === 'containment' ? 'refused' : 'pending',
        text: `${override.role ?? 'All roles'} ${override.type === 'containment' ? 'removed' : 'allowed'} until ${time(override.expires_at, zone)}`,
      })
    if (changed('operation', operation.id) || changed('rate', operation.id)) badges.push({ tone: 'pending', text: trial })
    return {
      key,
      title: (
        <code>
          <span className="method">{operation.method}</span> {operation.target}
        </code>
      ),
      detail: <code>{operation.id}</code>,
      kind: 'Endpoint',
      badges,
      mark: selectedRole && operation.auth !== 'none' ? (allowed(selectedRole, operation.id) ? 'allowed' : 'not-allowed') : undefined,
      dim: !inTrace(key),
    }
  }
  const groups = [
    ...policy.apis.map((api) => ({ id: api.id, operations: policy.operations.filter((operation) => operation.api === api.id) })),
    { id: '', operations: policy.operations.filter((operation) => !policy.apis.some((api) => api.id === operation.api)) },
  ].filter((group) => group.id !== '' || group.operations.length > 0)

  const cards: CardSpec[] = [
    { id: 'roles', kind: 'role', data: { kind: 'role', title: 'Roles', count: String(policy.roles.length), rows: roleRows, collapsible: true } },
    ...groups.map((group): CardSpec => {
      const usable = group.operations.filter((operation) => operation.auth !== 'none')
      const reachable = selectedRole ? usable.filter((operation) => allowed(selectedRole, operation.id)).length : undefined
      return {
        id: `api:${group.id}`,
        kind: 'operation',
        api: group.id || undefined,
        data: {
          kind: 'operation',
          title: group.id ? `${capital(group.id)} API Endpoints` : 'Endpoints without an API',
          count: reachable !== undefined ? `${reachable} of ${usable.length}` : String(group.operations.length),
          rows: group.operations.map(operationRow),
          collapsible: true,
          dim: !!props.trace && !group.operations.some((operation) => operation.id === props.trace?.operation),
        },
      }
    }),
    {
      id: 'apis',
      kind: 'api',
      data: {
        kind: 'api',
        title: 'APIs',
        count: String(policy.apis.length),
        collapsible: false,
        rows: policy.apis.map((api) => {
          const key: BoxKey = `api:${api.id}`
          return {
            key,
            title: api.id || '(empty ID)',
            detail: api.upstream ? <>→ <code>{api.upstream}</code></> : '→ no service address',
            kind: 'API',
            badges: badgesFor(key),
            dim: !inTrace(key),
          }
        }),
      },
    },
    {
      id: 'upstreams',
      kind: 'upstream',
      data: {
        kind: 'upstream',
        title: 'Service Addresses',
        count: String(policy.upstreams.length),
        collapsible: false,
        rows: policy.upstreams.map((upstream) => {
          const key: BoxKey = `upstream:${upstream.id}`
          return {
            key,
            title: upstream.id || '(empty ID)',
            detail: <code>{upstream.dial || 'Address needed'}</code>,
            kind: 'Service address',
            badges: badgesFor(key),
            dim: !inTrace(key),
          }
        }),
      },
    },
  ]

  // Few connections, each standing for many: roles reach an API's
  // endpoints, the endpoints route to their API, and the APIs forward to
  // their service addresses.
  const edges: Edge[] = []
  const focus = (on: boolean) => (selectedRole ? (on ? ' designer-edge-focus' : ' designer-edge-dim') : '')
  for (const group of groups) {
    const reached = group.operations.filter((operation) => policy.roles.some((role) => allowed(role.id, operation.id)))
    if (reached.length)
      edges.push({
        id: `access:${group.id}`,
        source: 'roles',
        target: `api:${group.id}`,
        type: 'bus',
        className: `designer-edge-permit${focus(!!selectedRole && reached.some((operation) => allowed(selectedRole, operation.id)))}`,
        ariaLabel: `Roles with permissions call ${group.id || 'unassigned'} endpoints`,
        markerEnd: { type: MarkerType.ArrowClosed },
      })
    if (group.id)
      edges.push({
        id: `route:${group.id}`,
        source: `api:${group.id}`,
        target: 'apis',
        type: 'bus',
        className: 'designer-edge-route',
        ariaLabel: `${group.id} endpoints route to API ${group.id}`,
        markerEnd: { type: MarkerType.ArrowClosed },
      })
  }
  if (policy.apis.length && policy.upstreams.length)
    edges.push({
      id: 'forward',
      source: 'apis',
      target: 'upstreams',
      type: 'bus',
      className: 'designer-edge-route',
      ariaLabel: 'Each API forwards to its service address',
      markerEnd: { type: MarkerType.ArrowClosed },
    })
  edges.sort((a, b) => Number(a.className?.includes('designer-edge-focus')) - Number(b.className?.includes('designer-edge-focus')))
  return { cards, edges }
}
