import { memo, useEffect, useMemo, useRef, useState } from 'react'
import {
  ReactFlow,
  ReactFlowProvider,
  Handle,
  Position,
  Panel,
  MarkerType,
  useNodesInitialized,
  useNodesState,
  useEdgesState,
  useReactFlow,
  applyNodeChanges,
  type Node,
  type NodeProps,
  type Edge,
  type Connection,
  type NodeChange,
} from '@xyflow/react'
import '@xyflow/react/dist/base.css'
import type { ControlChange, Operation, Override, Policy } from './api'
import { automaticLayout, columnX } from './designer-layout'
import {
  connectionReason,
  connect,
  copy,
  deleteReason,
  splitKey,
  type BoxKey,
  type BoxKind,
  type Issue,
  type Positions,
} from './designer-model'
import { Badge } from './ui'

interface BoxData extends Record<string, unknown> {
  kind: BoxKind
  id: string
  policy: Policy
  editable: boolean
  issues: Issue[]
  overlays: Override[]
  changed?: number
  refused?: string
  onSelect: (key: BoxKey, section?: string) => void
}
type PolicyNode = Node<BoxData, 'policy'>
type CanvasNode = PolicyNode | Node<{ label: string }, 'heading'>
export interface TracePath {
  operation: string
  roles: string[]
  forward: boolean
}
interface CanvasProps {
  policy: Policy
  editable: boolean
  selection: BoxKey | null
  positions: Positions
  issues: Issue[]
  overlays: Override[]
  provisional?: { revision: number; changes: ControlChange[] }
  refused?: { box?: BoxKey; edge?: string; code: string }
  trace?: TracePath
  reset: number
  onSelect: (key: BoxKey, section?: string) => void
  onChange: (policy: Policy) => void
  onDelete: (key: BoxKey) => void
  onPositions: (positions: Positions) => void
  onReset: () => void
}

export function checks(
  policy: Policy,
  operation: Operation,
  overlays: Override[] = [],
): { name: string; value: string; tone: 'neutral' | 'refused' | 'pending' | null }[] {
  const api = policy.apis.find((item) => item.id === operation.api)
  const rate = overlays.find((override) => override.control === 'rate' && override.operation === operation.id)
  return [
    { name: 'Route', value: `${operation.method} ${operation.target}`, tone: 'neutral' },
    { name: 'Access', value: operation.access, tone: operation.access === 'deny' ? 'refused' : 'neutral' },
    { name: 'Token', value: operation.auth === 'none' ? 'public' : 'required', tone: operation.auth === 'none' ? 'pending' : 'neutral' },
    {
      name: 'Permit',
      value: operation.auth === 'none' ? 'n/a' : String(policy.permits.filter((permit) => permit.operation === operation.id).length),
      tone: 'neutral',
    },
    {
      name: 'Rate',
      value: rate
        ? `${rate.count} temporarily, until ${until(rate.expires_at)}`
        : operation.rate
          ? `${operation.rate.count}/${operation.rate.window_seconds}s`
          : 'none',
      tone: rate ? 'pending' : operation.rate ? 'neutral' : null,
    },
    {
      name: 'Body',
      value: operation.body
        ? `≤ ${operation.body.max_bytes} B${operation.headers ? ' + headers' : ''}`
        : operation.headers
          ? 'headers'
          : 'none',
      tone: operation.body || operation.headers ? 'neutral' : null,
    },
    { name: 'Record', value: 'always', tone: 'neutral' },
    { name: 'Forward', value: `${operation.api} → ${api?.upstream ?? 'missing upstream'}`, tone: 'neutral' },
  ]
}
function until(iso: string): string {
  return new Date(iso).toLocaleTimeString('en-GB', { hour12: false })
}

const PolicyBox = memo(function PolicyBox({ data }: NodeProps<PolicyNode>) {
  const key: BoxKey = `${data.kind}:${data.id}`
  const operation = data.kind === 'operation' ? data.policy.operations.find((item) => item.id === data.id) : undefined
  const upstream = data.kind === 'upstream' ? data.policy.upstreams.find((item) => item.id === data.id) : undefined
  const denied = data.overlays.find((override) => override.control === 'operation' && override.operation === data.id)
  return (
    <div
      className={`designer-box designer-box-${data.kind}${data.issues.length ? ' is-incomplete' : ''}${data.refused ? ' is-refused' : ''}`}
      data-kind={data.kind}
    >
      {data.editable && data.kind !== 'role' ? (
        <Handle type="target" position={Position.Left} role="button" tabIndex={-1} aria-label={`Connect to ${data.kind} ${data.id}`} />
      ) : null}
      {operation ? (
        <>
          <div className="designer-operation-title">
            <Badge tone="neutral">{operation.method}</Badge> <code>{operation.target}</code>
          </div>
          <p className="designer-object-id">
            <code>{operation.id}</code>
          </p>
          <div className="designer-checks" aria-label={`Checks for ${operation.id}`}>
            {checks(data.policy, operation, data.overlays).map((check) => (
              <button
                type="button"
                key={check.name}
                className={`designer-check${check.tone ? ` tone-${check.tone}` : ' is-none'}`}
                onClick={(event) => {
                  event.stopPropagation()
                  data.onSelect(key, check.name)
                }}
              >
                {check.name} {check.value}
              </button>
            ))}
          </div>
        </>
      ) : (
        <>
          <strong className="designer-box-title">{data.id || '(empty ID)'}</strong>
          {upstream ? (
            <p className="designer-object-id">
              <code>{upstream.dial || 'Dial address needed'}</code>
            </p>
          ) : null}
        </>
      )}
      {data.issues.length ? <Badge tone="pending">Incomplete</Badge> : null}
      {data.refused ? <Badge tone="refused">Refused: {data.refused}</Badge> : null}
      {denied ? <Badge tone="refused">denied until {until(denied.expires_at)}</Badge> : null}
      {data.changed ? <Badge tone="pending">changed in provisional r{data.changed}</Badge> : null}
      {data.editable && data.kind !== 'upstream' ? (
        <Handle type="source" position={Position.Right} role="button" tabIndex={-1} aria-label={`Connect from ${data.kind} ${data.id}`} />
      ) : null}
    </div>
  )
})
const ColumnHeading = memo(function ColumnHeading({ data }: NodeProps<Node<{ label: string }, 'heading'>>) {
  return <div className="designer-column-heading">{data.label}</div>
})
const nodeTypes = { policy: PolicyBox, heading: ColumnHeading }

function ViewportControls({ onReset }: { onReset: () => void }) {
  const flow = useReactFlow()
  return (
    <Panel position="bottom-left" className="designer-viewport-controls">
      <button className="btn btn-small" type="button" onClick={() => void flow.fitView()}>
        Fit
      </button>
      <button className="btn btn-small" type="button" aria-label="Zoom in" onClick={() => void flow.zoomIn()}>
        +
      </button>
      <button className="btn btn-small" type="button" aria-label="Zoom out" onClick={() => void flow.zoomOut()}>
        −
      </button>
      <button className="btn btn-small" type="button" onClick={onReset}>
        Reset layout
      </button>
    </Panel>
  )
}

// Below this zoom the 12px box text is too small to read.
const readableZoom = 0.85
const viewPadding = 24

function CanvasGraph(props: CanvasProps) {
  const flow = useReactFlow()
  const [message, setMessage] = useState('')
  const area = useRef<HTMLDivElement>(null)
  const measured = useNodesInitialized()
  const placed = useRef(false)
  // Fit the whole graph when it stays readable. Otherwise open at a readable
  // zoom with the roles column at the left edge and the graph's middle in
  // view; Fit still shows everything.
  function openingView() {
    const element = area.current
    if (!element) return
    const bounds = flow.getNodesBounds(flow.getNodes())
    const width = element.clientWidth - 2 * viewPadding
    const height = element.clientHeight - 2 * viewPadding
    const fit = Math.min(width / bounds.width, height / bounds.height)
    if (fit >= readableZoom) {
      void flow.fitView({ maxZoom: 1 })
      return
    }
    const x = bounds.width * readableZoom <= width ? (element.clientWidth - bounds.width * readableZoom) / 2 : viewPadding
    void flow.setViewport({
      x: x - bounds.x * readableZoom,
      y: element.clientHeight / 2 - (bounds.y + bounds.height / 2) * readableZoom,
      zoom: readableZoom,
    })
  }
  useEffect(() => {
    if (!measured || placed.current) return
    placed.current = true
    openingView()
  }, [measured])
  const automatic = useMemo(() => automaticLayout(props.policy), [props.policy])
  const graph = useMemo(() => {
    const nodes: CanvasNode[] = []
    const edges: Edge[] = []
    const changed = (control: string, target: string) =>
      props.provisional?.changes.some((item) => item.control === control && item.target === target)
    const inTrace = (key: string) => {
      if (!props.trace) return true
      const [kind, id] = splitKey(key as BoxKey)
      if (kind === 'role') return props.trace.roles.includes(id)
      if (kind === 'operation') return props.trace.operation === id
      const operation = props.policy.operations.find((item) => item.id === props.trace?.operation)
      if (!props.trace.forward || !operation) return false
      return kind === 'api' ? operation.api === id : props.policy.apis.find((api) => api.id === operation.api)?.upstream === id
    }
    const headingX = columnX(props.policy)
    for (const kind of ['role', 'operation', 'api', 'upstream'] as BoxKind[]) {
      const items =
        kind === 'role'
          ? props.policy.roles
          : kind === 'operation'
            ? props.policy.operations
            : kind === 'api'
              ? props.policy.apis
              : props.policy.upstreams
      for (const item of items) {
        const key: BoxKey = `${kind}:${item.id}`
        const operation = kind === 'operation' ? (item as Operation) : undefined
        nodes.push({
          id: key,
          type: 'policy',
          position: props.positions[key] ?? automatic[key],
          className: `designer-node-${kind}`,
          selected: props.selection === key,
          ariaRole: 'group',
          ariaLabel: `${kind === 'api' ? 'API' : kind[0].toUpperCase() + kind.slice(1)} ${item.id}${operation ? `, ${operation.method} ${operation.target}` : ''}`,
          data: {
            kind,
            id: item.id,
            policy: props.policy,
            editable: props.editable,
            issues: props.issues.filter((issue) => issue.box === key),
            overlays: props.overlays,
            changed: changed(kind, item.id) || (kind === 'operation' && changed('rate', item.id)) ? props.provisional?.revision : undefined,
            refused: props.refused?.box === key ? props.refused.code : undefined,
            onSelect: props.onSelect,
          },
        })
      }
      const column = nodes.filter((node) => node.id.startsWith(`${kind}:`))
      const firstY = column.length ? Math.min(...column.map((node) => node.position.y)) : 0
      nodes.push({
        id: `column:${kind}`,
        type: 'heading',
        position: { x: headingX[kind], y: firstY - 18 - 16 },
        data: { label: { role: 'Roles', operation: 'Operations', api: 'APIs', upstream: 'Upstreams' }[kind] },
        selectable: false,
        draggable: false,
        connectable: false,
        focusable: false,
        ariaRole: 'presentation',
      })
    }
    function edge(id: string, source: BoxKey, target: BoxKey, label: string, route = false, override?: Override) {
      const traced = inTrace(source) && inTrace(target)
      const modified = override?.type === 'containment' ? 'removed' : override ? 'temporary' : ''
      const provisional = changed('permit', `${splitKey(source)[1]}:${splitKey(target)[1]}`)
      const marker = override
        ? `${modified === 'removed' ? 'removed' : 'temporary'} until ${until(override.expires_at)}`
        : provisional
          ? `changed in provisional r${props.provisional?.revision}`
          : ''
      edges.push({
        id,
        source,
        target,
        type: 'smoothstep',
        label: marker || undefined,
        // A labelled arrow is drawn above the boxes so its label is never hidden.
        zIndex: marker ? 1 : undefined,
        labelBgPadding: [6, 2],
        labelBgBorderRadius: 3,
        ariaLabel: `${label}${marker ? `, ${marker}` : ''}`,
        focusable: true,
        markerEnd: route ? { type: MarkerType.ArrowClosed, color: 'var(--ink-3)' } : undefined,
        className: `${modified ? `designer-edge-${modified}` : ''}${props.trace ? (traced ? ' designer-edge-traced' : ' designer-not-traced') : ''}${props.refused?.edge === id ? ' designer-edge-refused' : ''}`,
      })
    }
    for (const permit of props.policy.permits)
      edge(
        `permit:${permit.role}:${permit.operation}`,
        `role:${permit.role}`,
        `operation:${permit.operation}`,
        `Permit: ${permit.role} may call ${permit.operation}`,
        false,
        props.overlays.find(
          (override) => override.control === 'permit' && override.role === permit.role && override.operation === permit.operation,
        ),
      )
    for (const override of props.overlays.filter((item) => item.control === 'permit' && item.type === 'exception')) {
      if (
        !props.policy.permits.some((permit) => permit.role === override.role && permit.operation === override.operation) &&
        props.policy.roles.some((role) => role.id === override.role) &&
        props.policy.operations.some((operation) => operation.id === override.operation)
      )
        edge(
          `override:${override.id}`,
          `role:${override.role}`,
          `operation:${override.operation}`,
          `Temporary permit: ${override.role} may call ${override.operation}`,
          false,
          override,
        )
    }
    for (const operation of props.policy.operations)
      if (props.policy.apis.some((api) => api.id === operation.api))
        edge(
          `route:${operation.id}`,
          `operation:${operation.id}`,
          `api:${operation.api}`,
          `${operation.id} routes to API ${operation.api}`,
          true,
        )
    for (const api of props.policy.apis)
      if (props.policy.upstreams.some((upstream) => upstream.id === api.upstream))
        edge(`upstream:${api.id}`, `api:${api.id}`, `upstream:${api.upstream}`, `API ${api.id} uses upstream ${api.upstream}`, true)
    return { nodes, edges }
  }, [
    props.policy,
    props.editable,
    props.selection,
    props.positions,
    props.issues,
    props.overlays,
    props.provisional,
    props.refused,
    props.trace,
    props.onSelect,
    automatic,
  ])
  const [nodes, setNodes] = useNodesState<CanvasNode>(graph.nodes)
  const [edges, setEdges, onEdgesChange] = useEdgesState(graph.edges)
  function measuredLayout(current: CanvasNode[]): CanvasNode[] {
    const heights = Object.fromEntries(
      current.filter((node) => node.type === 'policy' && node.measured?.height).map((node) => [node.id, node.measured!.height!]),
    )
    const layout = automaticLayout(props.policy, heights)
    return current.map((node) => {
      if (node.type === 'policy') return { ...node, position: props.positions[node.id] ?? layout[node.id] ?? node.position }
      const kind = node.id.slice('column:'.length)
      const column = Object.entries(layout).filter(([key]) => key.startsWith(`${kind}:`))
      const y = column.length ? Math.min(...column.map(([key, point]) => props.positions[key]?.y ?? point.y)) : 0
      return { ...node, position: { ...node.position, y: y - 18 - 16 } }
    })
  }
  useEffect(() => {
    setNodes((previous) =>
      measuredLayout(graph.nodes.map((node) => ({ ...node, measured: previous.find((old) => old.id === node.id)?.measured }))),
    )
    setEdges(graph.edges)
  }, [graph, props.reset])
  function onNodesChange(changes: NodeChange<CanvasNode>[]) {
    const selected = changes.find((change) => change.type === 'select' && change.selected)
    if (selected?.type === 'select' && selected.id !== props.selection && !selected.id.startsWith('column:'))
      props.onSelect(selected.id as BoxKey)
    setNodes((previous) => {
      const updated = applyNodeChanges(changes, previous)
      if (!changes.some((change) => change.type === 'dimensions')) return updated
      return measuredLayout(updated)
    })
  }
  function valid(connection: Connection | Edge): boolean {
    return connectionReason(props.policy, connection.source, connection.target) === null
  }

  return (
    <>
      <p className="designer-mobile-note hint">The canvas is easier on a wider screen; the Form tab has every option.</p>
      <div
        ref={area}
        className="designer-canvas"
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
          if (event.key !== 'Delete' || !props.editable || (event.target as HTMLElement).closest('input,textarea,select,button')) return
          const selected = nodes.find((node) => node.selected && node.type === 'policy')
          if (selected) {
            event.preventDefault()
            const reason = deleteReason(props.policy, selected.id as BoxKey)
            if (reason) setMessage(reason)
            else props.onDelete(selected.id as BoxKey)
            return
          }
          const selectedEdge = edges.find((item) => item.selected)
          if (!selectedEdge) return
          event.preventDefault()
          if (selectedEdge.id.startsWith('permit:')) {
            const next = copy(props.policy)
            next.permits = next.permits.filter((permit) => `permit:${permit.role}:${permit.operation}` !== selectedEdge.id)
            props.onChange(next)
          } else
            setMessage(
              selectedEdge.id.startsWith('route:')
                ? 'An operation must route to one API. Draw an arrow to another API to change it.'
                : 'An API must use one upstream. Draw an arrow to another upstream to change it.',
            )
        }}
      >
        <ReactFlow<CanvasNode>
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          colorMode="light"
          // The library's credit is given in the legend below the canvas, where
          // it cannot sit over a box's controls.
          proOptions={{ hideAttribution: true }}
          // Low enough for Fit to show a policy of a few dozen operations whole.
          minZoom={0.05}
          nodesFocusable
          edgesFocusable
          nodesDraggable={props.editable}
          nodesConnectable={props.editable}
          deleteKeyCode={null}
          onNodesChange={onNodesChange}
          onEdgesChange={onEdgesChange}
          onNodeClick={(_event, node) => {
            if (node.type === 'policy') props.onSelect(node.id as BoxKey)
          }}
          onConnect={(connection) => {
            props.onChange(connect(props.policy, connection.source as BoxKey, connection.target as BoxKey))
            setMessage('Connection added to the draft.')
          }}
          isValidConnection={valid}
          onConnectEnd={(_event, connection) => {
            if (!connection.isValid && connection.fromNode && connection.toNode)
              setMessage(connectionReason(props.policy, connection.fromNode.id, connection.toNode.id) ?? '')
          }}
          onNodeDragStop={(_event, node) =>
            props.onPositions({ ...props.positions, [node.id]: { x: node.position.x, y: node.position.y } })
          }
          ariaLabelConfig={{
            'node.a11yDescription.default': 'Press Enter to select a box and arrow keys to move it. The Form tab has every editing option.',
          }}
        >
          <ViewportControls onReset={props.onReset} />
        </ReactFlow>
      </div>
      {message ? (
        <p role="status" className="hint">
          {message}
        </p>
      ) : null}
      <p className="designer-legend">
        Solid = permit or route · dashed amber = temporary · dashed muted red = removed. Every temporary marker states its expiry.
        Drag the background or use the arrow keys to move around; Fit shows the whole policy. Canvas drawn with{' '}
        <a href="https://reactflow.dev" target="_blank" rel="noopener noreferrer">
          React Flow
        </a>
        .
      </p>
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
