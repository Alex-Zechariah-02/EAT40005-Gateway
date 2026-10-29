import { memo, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  ReactFlow,
  ReactFlowProvider,
  BaseEdge,
  Handle,
  Position,
  Panel,
  MarkerType,
  useNodesInitialized,
  useNodesState,
  useReactFlow,
  applyNodeChanges,
  type Node,
  type NodeProps,
  type Edge,
  type EdgeProps,
  type NodeChange,
  type Viewport,
  type FitViewOptions,
} from '@xyflow/react'
import { CaretDown, CaretRight, CornersIn, CornersOut } from '@phosphor-icons/react'
import '@xyflow/react/dist/base.css'
import type { ControlChange, Operation, Override, Policy } from './api'
import { capital, deleteReason, splitKey, type BoxKey, type BoxKind, type GroupAction, type Issue } from './designer-model'
import { ActionMenu, Badge } from './ui'

export interface TracePath {
  operation: string
  roles: string[]
  forward: boolean
}
interface CanvasProps {
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

// One line of a card: a role, an endpoint, an API or a service address.
interface Row {
  key: BoxKey
  title: ReactNode
  detail?: ReactNode
  label: string
  badges: { tone: 'pending' | 'refused' | 'neutral' | 'ok'; text: string }[]
  // mark is set while a role is selected: whether that role may call this endpoint.
  mark?: 'allowed' | 'not-allowed'
  dim?: boolean
}
interface CardData extends Record<string, unknown> {
  kind: BoxKind
  title: string
  count: string
  rows: Row[]
  open: boolean
  collapsible: boolean
  selection: BoxKey | null
  onToggle: () => void
  onSelect: (key: BoxKey) => void
  actions: GroupAction[]
  dim?: boolean
}
type CardNode = Node<CardData, 'card'>
interface CardSpec {
  id: string
  kind: BoxKind
  api?: string
  data: Pick<CardData, 'kind' | 'title' | 'count' | 'rows' | 'collapsible' | 'dim'>
}

function until(iso: string): string {
  return new Date(iso).toLocaleTimeString('en-GB', { hour12: false })
}

// Card is one group of boxes: the roles, the endpoints of one API, the APIs
// or the service addresses. A long group opens on demand, so the whole
// policy fits on screen and its connections stay few and readable.
const Card = memo(function Card({ id, data }: NodeProps<CardNode>) {
  const listId = `designer-card-${id.replace(/\W+/g, '-')}`
  return (
    <section className={`designer-card designer-kind-${data.kind}${data.dim ? ' is-dim' : ''}`} aria-label={data.title}>
      <Handle type="target" position={Position.Left} isConnectable={false} />
      <h2 className="designer-card-head">
        {data.collapsible ? (
          <button
            type="button"
            className="designer-card-toggle nopan"
            aria-label={`${data.title}, ${data.count}`}
            aria-expanded={data.open}
            aria-controls={listId}
            onClick={data.onToggle}
          >
            {data.open ? <CaretDown size={14} aria-hidden="true" /> : <CaretRight size={14} aria-hidden="true" />}
            <span className="designer-card-title">{data.title}</span>
            <span className="designer-card-count">{data.count}</span>
          </button>
        ) : (
          <span className="designer-card-heading">
            <span className="designer-card-title">{data.title}</span>
            <span className="designer-card-count">{data.count}</span>
          </span>
        )}
        {data.actions.length ? (
          <ActionMenu className="designer-card-add nopan" label={`Add to ${data.title}`} tip={`Add to ${data.title}`} items={data.actions} />
        ) : null}
      </h2>
      {data.open ? (
        <ul id={listId} className="designer-card-rows">
          {data.rows.map((row) => (
            <li key={row.key}>
              <button
                type="button"
                className={`designer-row nopan${row.dim ? ' is-dim' : ''}${row.mark ? ` is-${row.mark}` : ''}`}
                aria-current={data.selection === row.key ? 'true' : undefined}
                aria-label={row.label}
                data-box={row.key}
                onClick={() => data.onSelect(row.key)}
              >
                <span className="designer-row-main">
                  <span className="designer-row-title">{row.title}</span>
                  {row.detail ? <span className="designer-row-detail">{row.detail}</span> : null}
                </span>
                {row.mark ? <span className="designer-row-mark">{row.mark === 'allowed' ? 'Allowed' : 'No Access'}</span> : null}
                {row.badges.map((badge) => (
                  <Badge key={badge.text} tone={badge.tone}>
                    {badge.text}
                  </Badge>
                ))}
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <Handle type="source" position={Position.Right} isConnectable={false} />
    </section>
  )
})
const nodeTypes = { card: Card }

// BusEdge joins two columns with straight lines and square, slightly rounded
// bends on one shared vertical halfway between them. Every connection from
// one card, or into one card, runs along the same line, so several of them
// merge into one clean trunk instead of crossing.
function BusEdge({ sourceX, sourceY, targetX, targetY, markerEnd, interactionWidth }: EdgeProps) {
  const middle = (sourceX + targetX) / 2
  const rise = targetY - sourceY
  let path = `M ${sourceX},${sourceY} L ${targetX},${targetY}`
  if (Math.abs(rise) >= 1) {
    const turn = Math.sign(rise)
    const radius = Math.min(8, Math.abs(rise) / 2, Math.abs(targetX - sourceX) / 2)
    path = [
      `M ${sourceX},${sourceY}`,
      `L ${middle - radius},${sourceY}`,
      `Q ${middle},${sourceY} ${middle},${sourceY + turn * radius}`,
      `L ${middle},${targetY - turn * radius}`,
      `Q ${middle},${targetY} ${middle + radius},${targetY}`,
      `L ${targetX},${targetY}`,
    ].join(' ')
  }
  return <BaseEdge path={path} markerEnd={markerEnd} interactionWidth={interactionWidth} />
}
const edgeTypes = { bus: BusEdge }

const columnX: Record<BoxKind, number> = { role: 0, operation: 256, api: 712, upstream: 948 }
const gap = 20
// The top keeps room for the canvas toolbar, so no card sits under it.
const fit: { maxZoom: number; padding: FitViewOptions['padding'] } = { maxZoom: 1, padding: { top: '64px', x: '6%', bottom: '6%' } }
// A card brought into focus is never shown smaller than this, so its text
// stays readable.
const readable = 0.85

function CanvasGraph(props: CanvasProps) {
  const flow = useReactFlow()
  const [message, setMessage] = useState('')
  const measured = useNodesInitialized()
  const placed = useRef(false)
  // refit is set when groups open or close: once the cards have their new
  // sizes, the view moves to a group that was opened. After a group closes,
  // or after Expand All or Collapse All, every card is fitted again when
  // they can all be read; otherwise the view stays on the closed group.
  const refit = useRef<{ card?: string; opened: boolean } | null>(null)
  // Which groups are open. Roles start open; endpoint lists start closed and
  // open on demand, or by themselves for the selected or traced endpoint.
  const [open, setOpen] = useState<Set<string>>(() => new Set(['roles']))
  const focusedOperation = props.selection?.startsWith('operation:') ? splitKey(props.selection)[1] : props.trace?.operation
  const focusedApi = props.policy.operations.find((operation) => operation.id === focusedOperation)?.api
  useEffect(() => {
    if (focusedApi === undefined) return
    const group = props.policy.apis.some((api) => api.id === focusedApi) ? `api:${focusedApi}` : 'api:'
    if (!open.has(group)) setOpen((previous) => new Set(previous).add(group))
  }, [focusedApi, focusedOperation])

  const graph = useMemo(() => {
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
        label: `Role ${role.id}`,
        badges,
        dim: !inTrace(key) || (selectedRole !== undefined && selectedRole !== role.id),
      }
    })
    const operationRow = (operation: Operation): Row => {
      const key: BoxKey = `operation:${operation.id}`
      const badges = badgesFor(key)
      if (operation.auth === 'none') badges.push({ tone: 'neutral', text: 'Public' })
      if (operation.access === 'deny') badges.push({ tone: 'refused', text: 'Denied' })
      const denied = props.overlays.find((override) => override.control === 'operation' && override.operation === operation.id)
      if (denied) badges.push({ tone: 'refused', text: `Denied until ${until(denied.expires_at)}` })
      const rate = props.overlays.find((override) => override.control === 'rate' && override.operation === operation.id)
      if (rate) badges.push({ tone: 'pending', text: `Limit ${rate.count} until ${until(rate.expires_at)}` })
      for (const override of props.overlays.filter((item) => item.control === 'permit' && item.operation === operation.id))
        badges.push({
          tone: override.type === 'containment' ? 'refused' : 'pending',
          text: `${override.role ?? 'All roles'} ${override.type === 'containment' ? 'removed' : 'allowed'} until ${until(override.expires_at)}`,
        })
      if (changed('operation', operation.id) || changed('rate', operation.id)) badges.push({ tone: 'pending', text: trial })
      return {
        key,
        title: (
          <code>
            <span className="method">{operation.method}</span> {operation.target}
          </code>
        ),
        detail: operation.id,
        label: `Endpoint ${operation.id}, ${operation.method} ${operation.target}`,
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
              detail: `→ ${api.upstream || 'no service address'}`,
              label: `API ${api.id}`,
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
              label: `Service address ${upstream.id}`,
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
  }, [props.policy, props.selection, props.issues, props.overlays, props.provisional, props.refused, props.trace])

  // Cards stand in four columns, each centred on one line as in a pipeline
  // diagram, and restack as groups open and close.
  function laidOut(heights: Record<string, number>): CardNode[] {
    const kinds: BoxKind[] = ['role', 'operation', 'api', 'upstream']
    const height = (id: string) => heights[id] ?? 56
    const columns = kinds.map((kind) => graph.cards.filter((card) => card.kind === kind))
    const totals = columns.map((list) => list.reduce((sum, card) => sum + height(card.id), 0) + Math.max(0, list.length - 1) * gap)
    const tallest = Math.max(0, ...totals)
    return columns.flatMap((list, index) => {
      let y = (tallest - totals[index]) / 2
      return list.map((card) => {
        const node: CardNode = {
          id: card.id,
          type: 'card',
          position: { x: columnX[kinds[index]], y },
          className: `designer-node-${kinds[index]}`,
          draggable: false,
          selectable: false,
          focusable: false,
          ariaRole: 'group',
          data: {
            ...card.data,
            open: !card.data.collapsible || open.has(card.id),
            actions: props.editable ? props.actions(card.kind, card.api) : [],
            selection: props.selection,
            onSelect: props.onSelect,
            onToggle: () => {
              refit.current = { card: card.id, opened: !open.has(card.id) }
              setOpen((previous) => {
                const next = new Set(previous)
                if (next.has(card.id)) next.delete(card.id)
                else next.add(card.id)
                return next
              })
            },
          },
        }
        y += height(card.id) + gap
        return node
      })
    })
  }
  const heightsOf = (list: CardNode[]) =>
    Object.fromEntries(list.filter((node) => node.measured?.height).map((node) => [node.id, node.measured!.height!]))
  const keepMeasured = (laid: CardNode[], from: CardNode[]) =>
    laid.map((node) => ({ ...node, measured: from.find((old) => old.id === node.id)?.measured }))
  const [nodes, setNodes] = useNodesState<CardNode>(laidOut({}))
  useEffect(() => {
    setNodes((previous) => keepMeasured(laidOut(heightsOf(previous)), previous))
  }, [graph, open, props.editable])
  function onNodesChange(changes: NodeChange<CardNode>[]) {
    setNodes((previous) => {
      const updated = applyNodeChanges(changes, previous)
      if (!changes.some((change) => change.type === 'dimensions')) return updated
      if (refit.current) {
        const { card, opened } = refit.current
        refit.current = null
        requestAnimationFrame(() => {
          if (card && (opened || !allReadable())) focusOn(flow.getNodesBounds([card]), 250)
          else fitTo(200)
        })
      }
      return keepMeasured(laidOut(heightsOf(updated)), updated)
    })
  }
  useEffect(() => {
    if (!measured || placed.current) return
    placed.current = true
    fitTo(0)
  }, [measured])
  useEffect(() => {
    if (placed.current) fitTo(200)
  }, [props.expanded])

  const area = useRef<HTMLDivElement>(null)
  const before = useRef<Viewport | null>(null)
  // fitTo shows the whole diagram in the window, clear of the toolbar and of
  // the side form when it is open.
  function fitTo(duration: number) {
    const canvas = area.current
    const panel = canvas?.closest('.designer-workspace')?.querySelector('.designer-side-panel')
    const covered = canvas && panel && panel.getBoundingClientRect().top < canvas.getBoundingClientRect().bottom - 1
    const padding: FitViewOptions['padding'] = covered ? { top: '64px', bottom: '24px', left: '24px', right: `${panel.getBoundingClientRect().width + 36}px` } : fit.padding
    void flow.fitView({ ...fit, padding, duration })
  }
  // allReadable says whether fitting every card keeps them at a readable size.
  function allReadable(): boolean {
    const canvas = area.current
    if (!canvas) return true
    const bounds = flow.getNodesBounds(flow.getNodes())
    return Math.min((canvas.clientWidth * 0.88) / bounds.width, (canvas.clientHeight - 64 - 24) / bounds.height) >= readable
  }
  // focusOn brings one card or row (in the diagram's own coordinates) into
  // the uncovered part of the canvas at a readable size: centred when it
  // fits, its top just under the toolbar when it is taller than the window.
  function focusOn(box: { x: number; y: number; width: number; height: number }, duration: number) {
    const canvas = area.current
    if (!canvas) return
    const panel = canvas.closest('.designer-workspace')?.querySelector('.designer-side-panel')
    const canvasBox = canvas.getBoundingClientRect()
    const covered = panel && panel.getBoundingClientRect().top < canvasBox.bottom - 1
    const left = 24
    const right = covered ? panel.getBoundingClientRect().width + 36 : 24
    const top = 64
    const bottom = 24
    const width = canvasBox.width - left - right
    const height = canvasBox.height - top - bottom
    const zoom = Math.max(readable, Math.min(1, width / box.width, height / box.height))
    const x = left + (width - box.width * zoom) / 2 - box.x * zoom
    const y = box.height * zoom <= height ? top + (height - box.height * zoom) / 2 - box.y * zoom : top - box.y * zoom
    void flow.setViewport({ x, y, zoom }, { duration })
  }
  // The canvas takes the window's height left between the toolbar above it
  // and what follows it, so the whole diagram shows without scrolling the
  // page. Full screen and narrow screens keep their own fixed heights.
  const [height, setHeight] = useState<number>()
  useEffect(() => {
    if (props.expanded) {
      setHeight(undefined)
      return
    }
    const page = document.getElementById('content')
    const measure = () => {
      const canvas = area.current
      if (!canvas || !page || window.innerWidth < 768) {
        setHeight(undefined)
        return
      }
      const box = canvas.getBoundingClientRect()
      // The lowest element in the page's flow: closed dialogs and the fixed
      // notification bar take no room in it.
      const inFlow = [...page.children].filter(
        (child) => child.tagName !== 'DIALOG' && child.getClientRects().length && !['fixed', 'absolute'].includes(getComputedStyle(child).position),
      )
      const end = Math.max(box.bottom, ...inFlow.map((child) => child.getBoundingClientRect().bottom))
      const below = end - box.bottom + parseFloat(getComputedStyle(page).paddingBottom)
      setHeight(Math.max(420, Math.floor(window.innerHeight - (box.top + window.scrollY) - below)))
    }
    measure()
    const observer = new ResizeObserver(measure)
    if (page) observer.observe(page)
    window.addEventListener('resize', measure)
    return () => {
      observer.disconnect()
      window.removeEventListener('resize', measure)
    }
  }, [props.expanded])
  useEffect(() => {
    if (placed.current) fitTo(200)
  }, [height])
  useEffect(() => {
    if (!props.panelOpen) {
      if (before.current) void flow.setViewport(before.current, { duration: 250 })
      before.current = null
      return
    }
    const key = props.selection ?? (props.trace ? `operation:${props.trace.operation}` : null)
    if (!key) return
    // A selection can open its group first, so the row is measured once the
    // cards have taken their new sizes.
    const timer = setTimeout(() => {
      const canvas = area.current
      const row = canvas?.querySelector(`[data-box="${CSS.escape(key)}"]`) ?? canvas?.querySelector(`[data-box^="${CSS.escape(key.split(':')[0])}:"]`)
      const panel = canvas?.closest('.designer-workspace')?.querySelector('.designer-side-panel')
      if (!canvas || !row || !panel) return
      // An endpoint is brought into view at a readable size beside the form.
      if (key.startsWith('operation:') && row.getAttribute('data-box') === key) {
        before.current ??= flow.getViewport()
        const box = row.getBoundingClientRect()
        const start = flow.screenToFlowPosition({ x: box.left, y: box.top })
        const end = flow.screenToFlowPosition({ x: box.right, y: box.bottom })
        focusOn({ x: start.x, y: start.y, width: end.x - start.x, height: end.y - start.y }, 250)
        return
      }
      const rowBox = row.getBoundingClientRect()
      const panelBox = panel.getBoundingClientRect()
      const canvasBox = canvas.getBoundingClientRect()
      // A narrow screen stacks the form under the canvas: nothing is covered.
      if (panelBox.top >= canvasBox.bottom - 1) return
      const room = 24
      const shift = panelBox.left - room - rowBox.right
      if (shift >= 0) return
      const viewport = flow.getViewport()
      before.current ??= viewport
      // Move left by what is covered, but never push the row off the left edge.
      const limit = canvasBox.left + room - rowBox.left
      void flow.setViewport({ ...viewport, x: viewport.x + Math.max(shift, Math.min(0, limit)) }, { duration: 250 })
    }, 120)
    return () => clearTimeout(timer)
  }, [props.panelOpen, props.selection, props.trace?.operation])

  function setAll(state: 'open' | 'closed') {
    refit.current = { opened: false }
    setOpen(state === 'open' ? new Set(graph.cards.filter((card) => card.data.collapsible).map((card) => card.id)) : new Set())
  }

  return (
    <>
      <div
        ref={area}
        className="designer-canvas"
        style={height ? { height } : undefined}
        role="region"
        aria-label="Policy canvas"
        tabIndex={0}
        onKeyDown={(event) => {
          if (event.target === event.currentTarget && ['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(event.key)) {
            event.preventDefault()
            const viewport = flow.getViewport()
            const x = event.key === 'ArrowLeft' ? 32 : event.key === 'ArrowRight' ? -32 : 0
            const y = event.key === 'ArrowUp' ? 32 : event.key === 'ArrowDown' ? -32 : 0
            void flow.setViewport({ ...viewport, x: viewport.x + x, y: viewport.y + y })
            return
          }
          if (event.key === 'Escape' && props.expanded) {
            props.onExpand()
            return
          }
          if (event.key !== 'Delete' || !props.editable || !props.selection) return
          if ((event.target as HTMLElement).closest('input,textarea,select')) return
          event.preventDefault()
          const reason = deleteReason(props.policy, props.selection)
          if (reason) setMessage(reason)
          else props.onDelete(props.selection)
        }}
      >
        <ReactFlow<CardNode>
          nodes={nodes}
          edges={graph.edges}
          nodeTypes={nodeTypes}
          edgeTypes={edgeTypes}
          colorMode="light"
          // The library's MIT licence allows its attribution to be hidden.
          proOptions={{ hideAttribution: true }}
          // Low enough that every group open still fits a short canvas.
          minZoom={0.1}
          // The wheel moves around the canvas like a page; pinch or Ctrl and
          // the wheel zoom.
          panOnScroll
          nodesDraggable={false}
          nodesConnectable={false}
          elementsSelectable={false}
          deleteKeyCode={null}
          onNodesChange={onNodesChange}
        >
          <Panel position="top-right" className="designer-viewport-controls">
            {props.tools ? <div className="designer-canvas-tools">{props.tools}</div> : null}
            <button className="btn btn-small" type="button" onClick={() => setAll('open')}>
              Expand All
            </button>
            <button className="btn btn-small" type="button" onClick={() => setAll('closed')}>
              Collapse All
            </button>
            <button className="btn btn-small" type="button" onClick={() => fitTo(200)}>
              Fit
            </button>
            <button className="btn btn-small" type="button" aria-pressed={props.expanded} onClick={props.onExpand}>
              {props.expanded ? <CornersIn size={16} aria-hidden="true" /> : <CornersOut size={16} aria-hidden="true" />}
              {props.expanded ? 'Exit Full Screen' : 'Full Screen'}
            </button>
          </Panel>
        </ReactFlow>
      </div>
      {message ? (
        <p role="status" className="hint">
          {message}
        </p>
      ) : null}
    </>
  )
}

export function DesignerCanvas(props: CanvasProps) {
  return (
    <ReactFlowProvider>
      <CanvasGraph {...props} />
    </ReactFlowProvider>
  )
}
