import cytoscape from 'cytoscape'
import type { Core, NodeSingular } from 'cytoscape'
import { useEffect, useMemo, useRef, useState } from 'react'
import type { Scan } from './App'
import './topology.css'
import {
  graphElements,
  graphNodeForSelection,
  shouldFetchTopology,
  sourceForAddress,
  type GraphNode,
  type GraphResponse,
  type TopologyPlanSignal,
  type TopologyView,
} from './topology'

type Props = {
  scan: Scan | null
  planSignal: TopologyPlanSignal
  onSelectSource: (path: string, line: number) => void
}

type Position = { x: number; y: number }

const VIEWS: { id: TopologyView; label: string }[] = [
  { id: 'current', label: 'Current' },
  { id: 'proposed', label: 'Proposed' },
  { id: 'diff', label: 'Diff' },
]

const GRAPH_STYLE: cytoscape.StylesheetJson = [
  {
    selector: 'node',
    style: {
      width: 172,
      height: 58,
      shape: 'round-rectangle',
      'background-color': '#1b1d21',
      'background-image': 'data(card)',
      'background-fit': 'none',
      'background-width': 172,
      'background-height': 58,
      'border-width': 1,
      'border-color': '#34373d',
      'overlay-opacity': 0,
    },
  },
  {
    selector: '$node > node',
    style: {
      padding: '24px',
      'background-color': '#1a1c20',
      'background-opacity': 0.72,
      'background-position-x': '0%',
      'background-position-y': '0%',
      'border-color': '#30343b',
    },
  },
  {
    selector: 'node:selected',
    style: {
      'border-width': 2,
      'border-color': '#d6d8dc',
    },
  },
  {
    selector: 'node.state-created',
    style: { 'border-width': 2, 'border-color': '#46b26b' },
  },
  {
    selector: 'node.state-changed',
    style: { 'border-width': 2, 'border-color': '#c9a45c' },
  },
  {
    selector: 'node.state-replaced',
    style: { 'border-width': 2, 'border-color': '#d2853f' },
  },
  {
    selector: 'node.state-destroyed',
    style: {
      'border-width': 2,
      'border-color': '#c56b6b',
      'border-style': 'dashed',
      opacity: 0.72,
    },
  },
  {
    selector: 'edge',
    style: {
      width: 1,
      'curve-style': 'bezier',
      'line-color': '#454951',
      'target-arrow-color': '#454951',
      'target-arrow-shape': 'triangle',
      'arrow-scale': 0.65,
      'overlay-opacity': 0,
    },
  },
  {
    selector: 'edge.state-opened',
    style: { width: 2, 'line-color': '#46b26b', 'target-arrow-color': '#46b26b' },
  },
  {
    selector: 'edge.state-closed',
    style: {
      width: 2,
      'line-color': '#c56b6b',
      'target-arrow-color': '#c56b6b',
      'line-style': 'dashed',
      opacity: 0.72,
    },
  },
]

function graphError(view: TopologyView, status: number, error?: string) {
  if (error) return error
  if (status === 404) return `${VIEWS.find((item) => item.id === view)?.label} topology is unavailable`
  if (status === 409) return 'Topology is unavailable while the plan is running'
  return `Could not load topology (${status})`
}

function savePositions(cy: Core, positions: Map<string, Position>) {
  cy.nodes().forEach((node) => {
    positions.set(node.id(), { ...node.position() })
  })
}

function runLayout(cy: Core, positions: Map<string, Position>) {
  const reused: NodeSingular[] = []
  cy.nodes().forEach((node) => {
    const position = positions.get(node.id())
    if (position) {
      node.position(position)
      node.lock()
      reused.push(node)
    }
  })
  const layout = cy.layout({
    name: 'cose',
    animate: false,
    randomize: reused.length === 0,
    fit: true,
    padding: 32,
    nodeRepulsion: () => 7000,
    idealEdgeLength: () => 110,
    componentSpacing: 80,
  })
  layout.one('layoutstop', () => reused.forEach((node) => node.unlock()))
  layout.run()
}

export default function TopologyPanel({ scan, planSignal, onSelectSource }: Props) {
  const [view, setView] = useState<TopologyView>('current')
  const [graphs, setGraphs] = useState<Partial<Record<TopologyView, GraphResponse>>>({})
  const [loading, setLoading] = useState(false)
  const [building, setBuilding] = useState(false)
  const [error, setError] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [hovered, setHovered] = useState<GraphNode | null>(null)
  const [selected, setSelected] = useState<GraphNode | null>(null)
  const container = useRef<HTMLDivElement>(null)
  const cyRef = useRef<Core | null>(null)
  const positions = useRef(new Map<string, Position>())
  const graph = graphs[view]

  useEffect(() => {
    if (planSignal.revision === 0) return
    setGraphs((current) => ({ current: current.current }))
    setError('')
    setSelected(null)
    setHovered(null)
    if (planSignal.kind === 'running') {
      setBuilding(true)
      setView('current')
      return
    }
    setBuilding(false)
    if (planSignal.kind === 'changed') {
      setView('diff')
      setRefresh((current) => current + 1)
    } else {
      setView('current')
    }
  }, [planSignal])

  useEffect(() => {
    if (!shouldFetchTopology(view, building)) {
      setLoading(false)
      setError('')
      return
    }
    const controller = new AbortController()
    const requestedView = view
    setLoading(true)
    setError('')
    void (async () => {
      try {
        const response = await fetch(`/api/graph?view=${requestedView}`, {
          credentials: 'same-origin',
          signal: controller.signal,
        })
        const body = (await response.json().catch(() => ({}))) as GraphResponse & { error?: string }
        if (!response.ok) {
          setGraphs((current) => {
            const next = { ...current }
            delete next[requestedView]
            return next
          })
          setError(graphError(requestedView, response.status, body.error))
          return
        }
        setGraphs((current) => ({ ...current, [requestedView]: body }))
      } catch (requestError) {
        if ((requestError as Error).name !== 'AbortError') {
          setError('Could not reach the topology service')
        }
      } finally {
        if (!controller.signal.aborted) setLoading(false)
      }
    })()
    return () => controller.abort()
  }, [building, refresh, view])

  useEffect(() => {
    const target = container.current
    if (!target || !graph || graph.nodes.length === 0) return
    const positionCache = positions.current
    if (cyRef.current) {
      savePositions(cyRef.current, positionCache)
      cyRef.current.destroy()
    }
    const nodesById = new Map(graph.nodes.map((node) => [node.id, node]))
    const cy = cytoscape({
      container: target,
      elements: graphElements(graph),
      style: GRAPH_STYLE,
      minZoom: 0.25,
      maxZoom: 2.5,
      boxSelectionEnabled: false,
      autoungrabify: true,
    })
    cyRef.current = cy
    runLayout(cy, positionCache)
    cy.on('mouseover', 'node', (event) => setHovered(nodesById.get(event.target.id()) ?? null))
    cy.on('mouseout', 'node', () => setHovered(null))
    cy.on('tap', 'node', (event) => setSelected(nodesById.get(event.target.id()) ?? null))
    cy.on('tap', (event) => {
      if (event.target === cy) setSelected(null)
    })
    const observer = new ResizeObserver(() => cy.resize())
    observer.observe(target)
    return () => {
      observer.disconnect()
      savePositions(cy, positionCache)
      cy.destroy()
      if (cyRef.current === cy) cyRef.current = null
    }
  }, [graph])

  const detail = selected ?? hovered
  const source = useMemo(
    () => (detail && scan ? sourceForAddress(detail.address, scan.blocks) : null),
    [detail, scan],
  )

  function choose(next: TopologyView) {
    setView(next)
    setRefresh((current) => current + 1)
    setSelected(null)
    setHovered(null)
  }

  function selectResource(node: GraphNode | null) {
    setSelected(node)
    setHovered(null)
    cyRef.current?.elements().unselect()
    if (node) cyRef.current?.getElementById(node.id).select()
  }

  return (
    <div className="topology">
      <div className="topology-toolbar" aria-label="Topology view">
        {VIEWS.map((item) => (
          <button
            key={item.id}
            type="button"
            className={item.id === view ? 'topology-view current' : 'topology-view'}
            aria-pressed={item.id === view}
            disabled={building && item.id !== 'current'}
            onClick={() => choose(item.id)}
          >
            {item.label}
          </button>
        ))}
        <label className="topology-resource" htmlFor="topology-resource">
          <span>Resource</span>
          <select
            id="topology-resource"
            value={selected?.id ?? ''}
            disabled={!graph || graph.nodes.length === 0}
            onChange={(event) =>
              selectResource(graphNodeForSelection(graph?.nodes ?? [], event.target.value))
            }
          >
            <option value="">Inspect resource…</option>
            {graph?.nodes.map((node) => (
              <option key={node.id} value={node.id}>
                {node.name} — {node.type} ({node.address})
              </option>
            ))}
          </select>
        </label>
      </div>

      <div className="topology-stage">
        <div
          ref={container}
          className="topology-canvas"
          role="img"
          aria-label={`${VIEWS.find((item) => item.id === view)?.label} infrastructure topology`}
        />
        <div className="topology-status" role={error ? 'alert' : 'status'} aria-live="polite">
          {building && view === 'current'
            ? 'Building proposed topology…'
            : loading
              ? `Loading ${view} topology…`
              : error || (!graph || graph.nodes.length === 0)
                ? error || `No ${view} topology available`
                : ''}
        </div>
      </div>

      {detail && (
        <aside className="topology-detail" aria-live="polite">
          <div className="topology-detail-text">
            <span className="topology-detail-type">{detail.type}</span>
            <strong>{detail.name}</strong>
            <code title={detail.address}>{detail.address}</code>
          </div>
          {source ? (
            <button type="button" onClick={() => onSelectSource(source.path, source.line)}>
              Open source
            </button>
          ) : (
            <span className="topology-source-unavailable">Source unavailable</span>
          )}
        </aside>
      )}
    </div>
  )
}
