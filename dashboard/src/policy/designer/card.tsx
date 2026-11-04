import { memo, type ReactNode } from 'react'
import { BaseEdge, Handle, Position, type Node, type NodeProps, type EdgeProps } from '@xyflow/react'
import { CaretDown, CaretRight } from '@phosphor-icons/react'
import type { BoxKey, BoxKind, GroupAction } from './model'
import { ActionMenu } from '../../shared/overlays'
import { Badge } from '../../shared/ui'

// The canvas cards: one group of boxes each, and the lines that join them.

// One line of a card: a role, an endpoint, an API or a service address.
export interface Row {
  key: BoxKey
  title: ReactNode
  detail?: ReactNode
  // kind starts the row's accessible name, which is otherwise its visible text.
  kind: string
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
export type CardNode = Node<CardData, 'card'>
export interface CardSpec {
  id: string
  kind: BoxKind
  api?: string
  data: Pick<CardData, 'kind' | 'title' | 'count' | 'rows' | 'collapsible' | 'dim'>
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
            aria-expanded={data.open}
            aria-controls={listId}
            onClick={data.onToggle}
          >
            {data.open ? <CaretDown size={14} aria-hidden="true" /> : <CaretRight size={14} aria-hidden="true" />}
            <span className="designer-card-title">{data.title}</span>{' '}
            <span className="designer-card-count">{data.count}</span>
          </button>
        ) : (
          <span className="designer-card-heading">
            <span className="designer-card-title">{data.title}</span>{' '}
            <span className="designer-card-count">{data.count}</span>
          </span>
        )}
        {data.actions.length ? (
          <ActionMenu className="designer-card-add nopan" label={`Add to ${data.title}`} tip={`Add to ${data.title}`} items={data.actions} />
        ) : null}
      </h2>
      {/* The list stays mounted so it can ease open and closed; the canvas
          measures each frame, so the cards below glide with it. */}
      <div className="designer-card-body" data-open={data.open ? '' : undefined}>
        <ul id={listId} className="designer-card-rows">
          {data.rows.map((row) => (
            <li key={row.key}>
              <button
                type="button"
                className={`designer-row nopan${row.dim ? ' is-dim' : ''}${row.mark ? ` is-${row.mark}` : ''}`}
                aria-current={data.selection === row.key ? 'true' : undefined}
                data-box={row.key}
                onClick={() => data.onSelect(row.key)}
              >
                <span className="designer-row-main">
                  <span className="designer-row-title">
                    <span className="visually-hidden">{row.kind} </span>
                    {row.title}
                  </span>{' '}
                  {row.detail ? <span className="designer-row-detail">{row.detail}</span> : null}
                </span>{' '}
                {row.mark ? <span className="designer-row-mark">{row.mark === 'allowed' ? 'Allowed' : 'No Permission'}</span> : null}{' '}
                {row.badges.map((badge) => (
                  <Badge key={badge.text} tone={badge.tone}>
                    {badge.text}
                  </Badge>
                ))}
              </button>
            </li>
          ))}
        </ul>
      </div>
      <Handle type="source" position={Position.Right} isConnectable={false} />
    </section>
  )
})
export const nodeTypes = { card: Card }

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
export const edgeTypes = { bus: BusEdge }
