import { useEffect, useMemo, useReducer, useRef, useState } from 'react'
import type { Scan } from './App'
import AnnotationEditor from './AnnotationEditor'
import SvgTopology from './SvgTopology'
import './topology.css'
import {
  annotationFor,
  annotationLabel,
  annotationsReducer,
  createAnnotationsState,
  getAnnotations,
  putAnnotations,
} from './annotations'
import type { AnnotationTarget } from './annotations'
import {
  clipText,
  graphNodeForSelection,
  layoutTopology,
  normalizeGraphResponse,
  sceneForGraph,
  shouldFetchTopology,
  sourceForAddress,
  type GraphNode,
  type GraphResponse,
  type GraphWireResponse,
  type LayoutSlot,
  type TopologyPlanSignal,
  type TopologySelection,
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
  const [selected, setSelected] = useState<TopologySelection | null>(null)
  const [annotations, dispatchAnnotations] = useReducer(
    annotationsReducer,
    undefined,
    () => createAnnotationsState(),
  )
  const annotationRequest = useRef(0)
  const graph = graphs[view]
  // The three views arrive one response at a time, so this map is laid out
  // several times with a growing node list. Carrying the slots across those
  // passes is what stops a late arrival from re-flowing cards the user is
  // already looking at; it is cleared with the graphs cache below.
  const slots = useRef(new Map<string, LayoutSlot>())
  const scene = useMemo(() => (graph ? sceneForGraph(graph) : null), [graph])
  const layout = useMemo(() => (scene ? layoutTopology(scene, slots.current) : null), [scene])

  useEffect(() => {
    const requestId = ++annotationRequest.current
    const revision = 0
    dispatchAnnotations({ type: 'load-start', requestId, revision })
    void getAnnotations().then(
      (document) => dispatchAnnotations({
        type: 'load-success',
        requestId,
        revision,
        document,
      }),
      (requestError: unknown) => dispatchAnnotations({
        type: 'load-failure',
        requestId,
        revision,
        error: requestError instanceof Error
          ? requestError.message
          : 'Could not load topology annotations',
      }),
    )
  }, [])

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
        const body = (await response.json().catch(() => ({}))) as GraphWireResponse & { error?: string }
        if (controller.signal.aborted) return
        if (!response.ok) {
          setGraphs((current) => {
            const next = { ...current }
            delete next[requestedView]
            return next
          })
          setError(graphError(requestedView, response.status, body.error))
          return
        }
        const normalized = normalizeGraphResponse(body)
        if (requestedView === 'diff') {
          const canonicalSlots = new Map<string, LayoutSlot>()
          layoutTopology(sceneForGraph(normalized), canonicalSlots)
          slots.current = canonicalSlots
        }
        setGraphs((current) => ({ ...current, [requestedView]: normalized }))
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

  const selectedNode = selected?.kind === 'node'
    ? graphNodeForSelection(graph?.nodes ?? [], selected.id)
    : null
  const selectedEdge = selected?.kind === 'edge'
    ? scene?.edges.find((edge) => edge.id === selected.id) ?? null
    : null
  const detail = selected ? selectedNode : hovered
  const source = useMemo(
    () => (detail && scan ? sourceForAddress(detail.address, scan.blocks) : null),
    [detail, scan],
  )

  function nodeLabel(node: GraphNode) {
    return annotationLabel(annotations.document, { kind: 'node', key: node.address }, node.name)
  }

  function edgeLabel(id: string) {
    return annotationLabel(annotations.document, { kind: 'edge', key: id }, 'depends on')
  }

  const selectedEdgeSource = selectedEdge
    ? graphNodeForSelection(scene?.nodes ?? [], selectedEdge.source)
    : null
  const selectedEdgeTarget = selectedEdge
    ? graphNodeForSelection(scene?.nodes ?? [], selectedEdge.target)
    : null

  const annotationTarget: AnnotationTarget | null = selectedNode
    ? { kind: 'node', key: selectedNode.address }
    : selectedEdge
      ? { kind: 'edge', key: selectedEdge.id }
      : null

  function choose(next: TopologyView) {
    setView(next)
    setRefresh((current) => current + 1)
    setSelected(null)
    setHovered(null)
  }

  function selectResource(node: GraphNode | null) {
    setSelected(node ? { kind: 'node', id: node.id } : null)
    setHovered(null)
  }

  async function saveAnnotations() {
    if (!annotations.loaded || !annotations.dirty || annotations.savingRequest) return
    const requestId = ++annotationRequest.current
    const revision = annotations.revision
    const document = annotations.document
    dispatchAnnotations({ type: 'save-start', requestId, revision })
    try {
      const saved = await putAnnotations(document)
      dispatchAnnotations({ type: 'save-success', requestId, revision, document: saved })
    } catch (requestError) {
      dispatchAnnotations({
        type: 'save-failure',
        requestId,
        revision,
        error: requestError instanceof Error
          ? requestError.message
          : 'Could not save topology annotations',
      })
    }
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
            value={selected?.kind === 'node' ? selected.id : ''}
            disabled={!graph || graph.nodes.length === 0}
            onChange={(event) =>
              selectResource(graphNodeForSelection(graph?.nodes ?? [], event.target.value))
            }
          >
            <option value="">Inspect resource…</option>
            {graph?.nodes.map((node) => (
              <option key={node.id} value={node.id}>
                {clipText(`${nodeLabel(node)} — ${node.type} (${node.address})`, 60)}
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
            selected={selected}
            label={`${VIEWS.find((item) => item.id === view)?.label} infrastructure topology`}
            nodeLabel={nodeLabel}
            edgeLabel={(edge) => edgeLabel(edge.id)}
            onHover={setHovered}
            onSelect={(selection) => {
              setSelected(selection)
              setHovered(null)
            }}
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

        {(detail || selectedEdge) && (
          <aside className="topology-detail" aria-live="polite">
            <div className="topology-detail-summary">
              <div className="topology-detail-text">
                {detail ? (
                  <>
                    <span className="topology-detail-type truncate" title={detail.type}>
                      {detail.type}
                    </span>
                    <strong className="truncate" title={nodeLabel(detail)}>
                      {nodeLabel(detail)}
                    </strong>
                    <code className="truncate" title={detail.address}>
                      {detail.address}
                    </code>
                  </>
                ) : selectedEdge ? (
                  <>
                    <span className="topology-detail-type">Dependency</span>
                    <strong className="truncate" title={edgeLabel(selectedEdge.id)}>
                      {edgeLabel(selectedEdge.id)}
                    </strong>
                    <code
                      className="truncate"
                      title={`${selectedEdgeSource?.address ?? selectedEdge.source} → ${selectedEdgeTarget?.address ?? selectedEdge.target}`}
                    >
                      {selectedEdgeSource ? nodeLabel(selectedEdgeSource) : selectedEdge.source}
                      {' → '}
                      {selectedEdgeTarget ? nodeLabel(selectedEdgeTarget) : selectedEdge.target}
                    </code>
                  </>
                ) : null}
              </div>
              {detail && source ? (
                <button type="button" onClick={() => onSelectSource(source.path, source.line)}>
                  Open source
                </button>
              ) : detail ? (
                <span className="topology-source-unavailable">Source unavailable</span>
              ) : null}
            </div>
            {selected && annotationTarget && (
              <AnnotationEditor
                kind={annotationTarget.kind}
                defaultLabel={selectedNode?.name ?? 'depends on'}
                annotation={annotationFor(annotations.document, annotationTarget)}
                loaded={annotations.loaded}
                dirty={annotations.dirty}
                saving={annotations.savingRequest !== null}
                saved={annotations.saved}
                error={annotations.error}
                onChange={(annotation) => dispatchAnnotations({
                  type: 'edit',
                  target: annotationTarget,
                  annotation,
                })}
                onSave={() => void saveAnnotations()}
              />
            )}
          </aside>
        )}
      </div>
    </div>
  )
}
