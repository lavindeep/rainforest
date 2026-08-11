import { useEffect, useMemo, useRef, useState } from 'react'
import type { Scan } from './App'
import SvgTopology from './SvgTopology'
import './topology.css'
import {
  clipText,
  graphNodeForSelection,
  layoutTopology,
  sceneForGraph,
  shouldFetchTopology,
  sourceForAddress,
  type GraphNode,
  type GraphResponse,
  type LayoutSlot,
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

export default function TopologyPanel({ scan, planSignal, onSelectSource }: Props) {
  const [view, setView] = useState<TopologyView>('current')
  const [graphs, setGraphs] = useState<Partial<Record<TopologyView, GraphResponse>>>({})
  const [loading, setLoading] = useState(false)
  const [building, setBuilding] = useState(false)
  const [error, setError] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [hovered, setHovered] = useState<GraphNode | null>(null)
  const [selected, setSelected] = useState<GraphNode | null>(null)
  const graph = graphs[view]
  // The three views arrive one response at a time, so this map is laid out
  // several times with a growing node list. Carrying the slots across those
  // passes is what stops a late arrival from re-flowing cards the user is
  // already looking at; it is cleared with the graphs cache below.
  const slots = useRef(new Map<string, LayoutSlot>())
  const scene = useMemo(() => (graph ? sceneForGraph(graph) : null), [graph])
  const layout = useMemo(() => (scene ? layoutTopology(scene, slots.current) : null), [scene])

  useEffect(() => {
    if (planSignal.revision === 0) return
    slots.current = new Map()
    setGraphs((current) => ({
      current: current.current ? { ...current.current } : undefined,
    }))
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
        if (requestedView === 'diff') {
          const canonicalSlots = new Map<string, LayoutSlot>()
          layoutTopology(sceneForGraph(body), canonicalSlots)
          slots.current = canonicalSlots
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
        {scene && layout && scene.nodes.length > 0 && (
          <SvgTopology
            scene={scene}
            layout={layout}
            selectedId={selected?.id}
            label={`${VIEWS.find((item) => item.id === view)?.label} infrastructure topology`}
            onHover={setHovered}
            onSelect={selectResource}
          />
        )}
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
