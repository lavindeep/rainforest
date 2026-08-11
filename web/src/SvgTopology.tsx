import { useEffect, useRef, useState } from 'react'
import type { KeyboardEvent, PointerEvent } from 'react'
import {
  clipText,
  EDGE_LABEL_HEIGHT,
  EDGE_LABEL_WIDTH,
  edgeLabelPoint,
  fitViewport,
  glyphForNode,
  LABEL_CHARS,
  layoutTopology,
  nodesByContainmentDepth,
  sceneForGraph,
  zoomViewport,
  type GraphNode,
  type Viewport,
} from './topology'
import TopologyGlyph from './TopologyGlyph'

type Scene = ReturnType<typeof sceneForGraph>
type Layout = ReturnType<typeof layoutTopology>
type Point = { x: number; y: number }

type Props = {
  scene: Scene
  layout: Layout
  selectedId?: string
  label: string
  onHover: (node: GraphNode | null) => void
  onSelect: (node: GraphNode | null) => void
}

const EMPTY_VIEWPORT: Viewport = { x: 0, y: 0, zoom: 1, owned: false }

function pathFor(points: Point[]) {
  if (points.length === 0) return ''
  return points.map((point, index) => `${index === 0 ? 'M' : 'L'} ${point.x} ${point.y}`).join(' ')
}

function stateClass(state?: string) {
  return state ? ` state-${state}` : ''
}

export default function SvgTopology({
  scene,
  layout,
  selectedId,
  label,
  onHover,
  onSelect,
}: Props) {
  const svg = useRef<SVGSVGElement>(null)
  const size = useRef({ width: 0, height: 0 })
  const drag = useRef<{ id: number; x: number; y: number; moved: boolean } | null>(null)
  const suppressClick = useRef(false)
  const [viewport, setViewport] = useState<Viewport>(EMPTY_VIEWPORT)
  const compounds = new Set(scene.nodes.flatMap((node) => (node.parent ? [node.parent] : [])))
  const compoundNodes = nodesByContainmentDepth(scene.nodes.filter((node) => compounds.has(node.id)))
  const leafNodes = scene.nodes.filter((node) => !compounds.has(node.id))

  useEffect(() => {
    const target = svg.current
    if (!target) return
    const observer = new ResizeObserver((entries) => {
      const { width, height } = entries[0]?.contentRect ?? target.getBoundingClientRect()
      if (width <= 0 || height <= 0) return
      const next = { width, height }
      size.current = next
      setViewport((current) => fitViewport(layout.nodes, next, current, layout.edges))
    })
    observer.observe(target)
    return () => observer.disconnect()
  }, [layout])

  useEffect(() => {
    const target = svg.current
    if (!target) return
    const wheel = (event: globalThis.WheelEvent) => {
      if (event.deltaY === 0 || Math.abs(event.deltaX) > Math.abs(event.deltaY)) return
      event.preventDefault()
      const bounds = target.getBoundingClientRect()
      const pointer = { x: event.clientX - bounds.left, y: event.clientY - bounds.top }
      setViewport((current) =>
        zoomViewport(current, current.zoom * Math.exp(-event.deltaY * 0.0015), pointer),
      )
    }
    target.addEventListener('wheel', wheel, { passive: false })
    return () => target.removeEventListener('wheel', wheel)
  }, [])

  useEffect(() => {
    const currentSize = size.current
    setViewport(
      currentSize.width > 0 && currentSize.height > 0
        ? fitViewport(layout.nodes, currentSize, undefined, layout.edges)
        : EMPTY_VIEWPORT,
    )
  }, [layout])

  function startPan(event: PointerEvent<SVGRectElement>) {
    event.currentTarget.setPointerCapture(event.pointerId)
    drag.current = { id: event.pointerId, x: event.clientX, y: event.clientY, moved: false }
  }

  function pan(event: PointerEvent<SVGRectElement>) {
    const current = drag.current
    if (!current || current.id !== event.pointerId) return
    const dx = event.clientX - current.x
    const dy = event.clientY - current.y
    if (dx === 0 && dy === 0) return
    current.x = event.clientX
    current.y = event.clientY
    current.moved = true
    setViewport((view) => ({ ...view, x: view.x + dx, y: view.y + dy, owned: true }))
  }

  function endPan(event: PointerEvent<SVGRectElement>, clearSelection: boolean) {
    const current = drag.current
    if (!current || current.id !== event.pointerId) return
    suppressClick.current = current.moved
    if (current.moved) {
      setTimeout(() => {
        suppressClick.current = false
      }, 0)
    }
    if (!current.moved && clearSelection) onSelect(null)
    drag.current = null
  }

  function selectWithKeyboard(event: KeyboardEvent<SVGGElement>, node: GraphNode) {
    if (event.key !== 'Enter' && event.key !== ' ') return
    event.preventDefault()
    onSelect(node)
  }

  function nodeGroup(node: GraphNode, compound: boolean) {
    const box = layout.nodes.get(node.id)
    if (!box) return null
    const tileSize = compound ? 18 : 38
    const tileX = box.x + 10
    const tileY = box.y + (compound ? 6 : 10)
    const textX = tileX + tileSize + (compound ? 7 : 10)
    const textLimit = compound ? LABEL_CHARS : LABEL_CHARS - 6
    return (
      <g
        key={node.id}
        className={`topology-node${compound ? ' topology-compound' : ''}${stateClass(node.state)}${selectedId === node.id ? ' selected' : ''}`}
        role="button"
        tabIndex={0}
        aria-label={`${node.type} ${node.name}`}
        aria-pressed={selectedId === node.id}
        onPointerEnter={() => onHover(node)}
        onPointerLeave={() => onHover(null)}
        onClick={() => {
          if (suppressClick.current) {
            suppressClick.current = false
            return
          }
          onSelect(node)
        }}
        onKeyDown={(event) => selectWithKeyboard(event, node)}
      >
        <rect
          className="topology-node-surface"
          x={box.x}
          y={box.y}
          width={box.w}
          height={box.h}
          rx={7}
          {...(compound
            ? {
                onPointerDown: startPan,
                onPointerMove: pan,
                onPointerUp: (event: PointerEvent<SVGRectElement>) => endPan(event, false),
                onPointerCancel: () => {
                  drag.current = null
                },
              }
            : {})}
        />
        <TopologyGlyph kind={glyphForNode(node)} x={tileX} y={tileY} size={tileSize} />
        <text
          x={textX}
          y={compound ? box.y + 12 : box.y + 22}
          textAnchor="start"
        >
          <tspan className="topology-node-type" x={textX}>
            {clipText(node.type, textLimit)}
          </tspan>
          <tspan
            className="topology-node-name"
            x={textX}
            dy={compound ? '1.2em' : '1.55em'}
          >
            {clipText(node.name, textLimit)}
          </tspan>
        </text>
      </g>
    )
  }

  return (
    <svg ref={svg} className="topology-canvas" role="group" aria-label={label}>
      <defs>
        <marker id="topology-arrow" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
          <path d="M 0 0 L 7 3.5 L 0 7 Z" />
        </marker>
        <marker id="topology-arrow-opened" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
          <path d="M 0 0 L 7 3.5 L 0 7 Z" />
        </marker>
        <marker id="topology-arrow-closed" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
          <path d="M 0 0 L 7 3.5 L 0 7 Z" />
        </marker>
      </defs>
      <rect
        className="topology-background"
        width="100%"
        height="100%"
        onPointerDown={startPan}
        onPointerMove={pan}
        onPointerUp={(event) => endPan(event, true)}
        onPointerCancel={() => {
          drag.current = null
        }}
      />
      <g transform={`translate(${viewport.x} ${viewport.y}) scale(${viewport.zoom})`}>
        {compoundNodes.map((node) => nodeGroup(node, true))}
        {layout.edges.map((edge) => {
          const point = edgeLabelPoint(edge.points)
          return (
            <g key={edge.id} className="topology-edge-group">
              <path
                className={`topology-edge${stateClass(edge.state)}`}
                d={pathFor(edge.points)}
                markerEnd={
                  edge.state === 'opened' || edge.state === 'closed'
                    ? `url(#topology-arrow-${edge.state})`
                    : 'url(#topology-arrow)'
                }
              />
              <g className="topology-edge-label" transform={`translate(${point.x} ${point.y})`}>
                <rect
                  x={-EDGE_LABEL_WIDTH / 2}
                  y={-EDGE_LABEL_HEIGHT / 2}
                  width={EDGE_LABEL_WIDTH}
                  height={EDGE_LABEL_HEIGHT}
                  rx="4"
                />
                <text y="3" textAnchor="middle">depends on</text>
              </g>
            </g>
          )
        })}
        {leafNodes.map((node) => nodeGroup(node, false))}
      </g>
    </svg>
  )
}
