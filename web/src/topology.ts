export type TopologyView = 'current' | 'proposed' | 'diff'

export type TopologyPlanSignal = {
  kind: 'running' | 'settled' | 'changed'
  revision: number
}

type PlanSummaryState = {
  state: string
  noChanges: boolean
  showError: string
}

export function topologySignalForDone(
  completedRunId: string,
  topologyRunId: string,
): TopologyPlanSignal['kind'] | null {
  if (topologyRunId === '') return null
  return topologyRunId === 'pending' || topologyRunId === completedRunId ? 'settled' : null
}

export function topologySignalForSummary(
  summary: PlanSummaryState,
): TopologyPlanSignal['kind'] {
  return summary.state === 'succeeded' && !summary.noChanges && summary.showError === ''
    ? 'changed'
    : 'settled'
}

export function shouldMarkTopologyRunning(runId: string, completedRunId: string) {
  return runId !== completedRunId
}

export function shouldFetchTopology(view: TopologyView, building: boolean) {
  return view !== 'current' || !building
}

export type NodeState = 'unchanged' | 'created' | 'changed' | 'replaced' | 'destroyed'
export type EdgeState = 'unchanged' | 'opened' | 'closed'

export type GraphNode = {
  id: string
  address: string
  type: string
  name: string
  kind: string
  parent?: string
  state?: NodeState
}

export function graphNodeForSelection(nodes: GraphNode[], id: string) {
  return nodes.find((node) => node.id === id) ?? null
}

export type GraphEdge = {
  id: string
  source: string
  target: string
  kind: 'dependency'
  state?: EdgeState
}

export type GraphResponse = {
  view: TopologyView
  runId: string
  nodes: GraphNode[]
  edges: GraphEdge[]
}

export type GraphElement = {
  data: Record<string, string>
  classes: string
}

type SourceBlock = {
  address: string
  file: string
  line: number
}

export type SourceLocation = { path: string; line: number }

function escapeXml(value: string) {
  return value
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&apos;')
}

function cardText(value: string, limit: number) {
  return value.length > limit ? `${value.slice(0, limit - 1)}…` : value
}

function className(value: string) {
  return value.toLowerCase().replace(/[^a-z0-9_-]+/g, '-')
}

export function nodeCardDataUri(node: GraphNode) {
  const type = escapeXml(cardText(node.type, 28))
  const name = escapeXml(cardText(node.name, 24))
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="172" height="58" viewBox="0 0 172 58"><text x="12" y="20" fill="#7c818b" font-family="system-ui,-apple-system,Segoe UI,sans-serif" font-size="10">${type}</text><text x="12" y="42" fill="#d6d8dc" font-family="system-ui,-apple-system,Segoe UI,sans-serif" font-size="14" font-weight="600">${name}</text></svg>`
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

export function graphElements(graph: GraphResponse): GraphElement[] {
  const nodes = graph.nodes.map((node) => ({
    data: {
      id: node.id,
      address: node.address,
      type: node.type,
      name: node.name,
      kind: node.kind,
      card: nodeCardDataUri(node),
      ...(node.parent ? { parent: node.parent } : {}),
      ...(node.state ? { state: node.state } : {}),
    },
    classes: [`kind-${className(node.kind)}`, node.state ? `state-${node.state}` : '']
      .filter(Boolean)
      .join(' '),
  }))
  const edges = graph.edges.map((edge) => ({
    data: {
      id: edge.id,
      source: edge.source,
      target: edge.target,
      kind: edge.kind,
      ...(edge.state ? { state: edge.state } : {}),
    },
    classes: [`edge-${edge.kind}`, edge.state ? `state-${edge.state}` : '']
      .filter(Boolean)
      .join(' '),
  }))
  return [...nodes, ...edges]
}

export function sourceForAddress(
  address: string,
  blocks: SourceBlock[],
): SourceLocation | null {
  if (address.startsWith('module.')) return null
  const base = address.replace(/(?:\[[^\]]+\])+$/, '')
  const block = blocks.find((candidate) => candidate.address === address || candidate.address === base)
  return block ? { path: block.file, line: block.line } : null
}
