import { useEffect, useMemo, useRef, useState } from 'react'
import {
  ReactFlow,
  ReactFlowProvider,
  Panel,
  useNodesInitialized,
  useNodesState,
  useReactFlow,
  applyNodeChanges,
  type NodeChange,
  type Viewport,
} from '@xyflow/react'
import { CornersIn, CornersOut } from '@phosphor-icons/react'
import '@xyflow/react/dist/base.css'
import { deleteReason, splitKey, type BoxKind } from './model'
import { type CardNode, edgeTypes, nodeTypes } from './card'
import { buildGraph, type CanvasProps } from './graph'
import { useZone } from '../../shared/zone'
import { allReadable, ease, fitTo, focusOn, motion, useCanvasHeight } from './camera'

// The Designer's canvas: the draft's boxes and links, moved and fitted to the window.

const columnX: Record<BoxKind, number> = { role: 0, operation: 256, api: 712, upstream: 948 }
const gap = 20

function CanvasGraph(props: CanvasProps) {
  const flow = useReactFlow()
  const travel = motion(420)
  const [message, setMessage] = useState('')
  const measured = useNodesInitialized()
  const placed = useRef(false)
  // refit is set when groups open or close: once the cards have their new
  // sizes, the view moves to a group that was opened. After a group closes,
  // or after Expand All or Collapse All, every card is fitted again when
  // they can all be read; otherwise the view stays on the closed group.
  const refit = useRef<{ card?: string; opened: boolean } | null>(null)
  const settle = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(settle.current), [])
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

  const zone = useZone()
  const graph = useMemo(() => buildGraph(props, zone), [props.policy, props.selection, props.issues, props.overlays, props.provisional, props.refused, props.trace, zone])

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
      // The view moves once the cards stop changing size, so it travels to
      // where they end, not to a frame of their opening.
      if (refit.current) {
        clearTimeout(settle.current)
        settle.current = setTimeout(() => {
          if (!refit.current) return
          const { card, opened } = refit.current
          refit.current = null
          if (card && (opened || !allReadable(flow, area.current))) focusOn(flow, area.current, flow.getNodesBounds([card]), travel)
          else fitTo(flow, area.current, travel)
        }, 80)
      }
      return keepMeasured(laidOut(heightsOf(updated)), updated)
    })
  }
  useEffect(() => {
    if (!measured || placed.current) return
    placed.current = true
    fitTo(flow, area.current, 0)
  }, [measured])
  useEffect(() => {
    if (placed.current) fitTo(flow, area.current, travel)
  }, [props.expanded])

  const area = useRef<HTMLDivElement>(null)
  const before = useRef<Viewport | null>(null)
  // pendingFocus is the timer that will bring the selection into view.
  const pendingFocus = useRef<ReturnType<typeof setTimeout>>(undefined)
  const height = useCanvasHeight(area, props.expanded)
  useEffect(() => {
    if (placed.current) fitTo(flow, area.current, travel)
  }, [height])
  useEffect(() => {
    if (!props.panelOpen) {
      if (before.current) void flow.setViewport(before.current, { duration: travel, ease })
      before.current = null
      return
    }
    const key = props.selection ?? (props.trace ? `operation:${props.trace.operation}` : null)
    if (!key) return
    // A selection can open its group first, so the row is measured once the
    // cards have taken their new sizes.
    const timer = setTimeout(() => {
      pendingFocus.current = undefined
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
        focusOn(flow, area.current, { x: start.x, y: start.y, width: end.x - start.x, height: end.y - start.y }, travel)
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
      void flow.setViewport({ ...viewport, x: viewport.x + Math.max(shift, Math.min(0, limit)) }, { duration: travel, ease })
    }, 120)
    pendingFocus.current = timer
    return () => clearTimeout(timer)
  }, [props.panelOpen, props.selection, props.trace?.operation])

  // cancelMoves drops a selection still waiting to be brought into view and a
  // group still settling, so the fit the person asked for wins over both.
  function cancelMoves() {
    clearTimeout(pendingFocus.current)
    pendingFocus.current = undefined
    clearTimeout(settle.current)
    refit.current = null
  }

  function setAll(state: 'open' | 'closed') {
    cancelMoves()
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
            void flow.setViewport({ ...viewport, x: viewport.x + x, y: viewport.y + y }, { duration: motion(140), ease })
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
            <button
              className="btn btn-small"
              type="button"
              onClick={() => {
                cancelMoves()
                fitTo(flow, area.current, 200)
              }}
            >
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
