import cytoscape from 'cytoscape'
import type { Core } from 'cytoscape'
import { useEffect, useMemo, useRef, useState } from 'react'
import type { Scan } from './App'
import './topology.css'
import {
  GRAPH_VARS,
  ZOOM_LIMITS,
  clipText,
  fitComposed,
  graphElements,
  graphNodeForSelection,
  graphStyle,
  presetPositions,
  shouldFetchTopology,
  sourceForAddress,
  unionNodes,
  type GraphNode,
  type GraphResponse,
  type LayoutSlot,
  type Palette,
  type TopologyPlanSignal,
  type TopologyView,
} from './topology'

type Props = {
  scan: Scan | null
  planSignal: TopologyPlanSignal
  onSelectSource: (path: string, line: number) => void
}

const VIEWS: { id: TopologyView; label: string }[] = [
  { id: 'current', label: 'Current' },
  { id: 'proposed', label: 'Proposed' },
  { id: 'diff', label: 'Diff' },
]

function graphError(view: TopologyView, status: number, error?: string) {
  if (error) return error
  if (status === 404) return `${VIEWS.find((item) => item.id === view)?.label} topology is unavailable`
  if (status === 409) return 'Topology is unavailable while the plan is running'
  return `Could not load topology (${status})`
}

function readPalette(): Palette {
  const style = getComputedStyle(document.documentElement)
  return Object.fromEntries(
    GRAPH_VARS.map((name) => [name, style.getPropertyValue(name).trim()]),
  ) as Palette
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
  const graph = graphs[view]
  // The three views arrive one response at a time, so this map is laid out
  // several times with a growing node list. Carrying the slots across those
  // passes is what stops a late arrival from re-flowing cards the user is
  // already looking at; it is cleared with the graphs cache below.
  const slots = useRef(new Map<string, LayoutSlot>())
  const positions = useMemo(
    () => Object.fromEntries(presetPositions(unionNodes(graphs), slots.current)),
    [graphs],
  )
  const positionsRef = useRef(positions)
  positionsRef.current = positions

  useEffect(() => {
    if (planSignal.revision === 0) return
    slots.current = new Map()
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
    cyRef.current?.destroy()
    const nodesById = new Map(graph.nodes.map((node) => [node.id, node]))
    const cy = cytoscape({
      container: target,
      elements: graphElements(graph),
      style: graphStyle(readPalette()),
      ...ZOOM_LIMITS,
      boxSelectionEnabled: false,
      autoungrabify: true,
    })
    cyRef.current = cy
    // ponytail: preset over cose — cose diverges on compound graphs in
    // cytoscape 3.34; revisit if free-form (non-containment) layouts are needed.
    cy.layout({ name: 'preset', positions: positionsRef.current, animate: false, fit: false }).run()
    fitComposed(cy)
    cy.on('mouseover', 'node', (event) => setHovered(nodesById.get(event.target.id()) ?? null))
    cy.on('mouseout', 'node', () => setHovered(null))
    cy.on('tap', 'node', (event) => setSelected(nodesById.get(event.target.id()) ?? null))
    cy.on('tap', (event) => {
      if (event.target === cy) setSelected(null)
    })
    // The pane strip can mount this container zero-sized or hand out a
    // transient 1x1 measurement, so keep refitting on every resize until the
    // user pans, zooms or taps the graph — after that the view is theirs.
    let owned = false
    cy.on('tapstart dragpan scrollzoom pinchzoom', () => {
      owned = true
    })
    const observer = new ResizeObserver(() => {
      cy.resize()
      if (!owned && target.clientWidth > 0 && target.clientHeight > 0) fitComposed(cy)
    })
    observer.observe(target)
    return () => {
      observer.disconnect()
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
                {clipText(`${node.name} — ${node.type} (${node.address})`, 60)}
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
            <span className="topology-detail-type truncate" title={detail.type}>
              {detail.type}
            </span>
            <strong className="truncate" title={detail.name}>
              {detail.name}
            </strong>
            <code className="truncate" title={detail.address}>
              {detail.address}
            </code>
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
