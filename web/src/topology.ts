import type { Core, StylesheetJson } from 'cytoscape'

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

// Counts code points, so an emoji is dropped whole instead of split in half.
export function clipText(value: string, limit: number) {
  const points = [...value]
  return points.length > limit ? `${points.slice(0, limit - 1).join('')}…` : value
}

function className(value: string) {
  return value.toLowerCase().replace(/[^a-z0-9_-]+/g, '-')
}

// Plain text rendered by cytoscape's native canvas label — never HTML or a URI.
export function nodeLabel(node: GraphNode) {
  return `${clipText(node.type, LABEL_CHARS)}\n${clipText(node.name, LABEL_CHARS)}`
}

export const CARD_W = 172
export const CARD_H = 58
export const MIN_ZOOM = 0.05
// Shared by the panel and the tests so both exercise the same zoom envelope.
// maxZoom is deliberately past 1:1 — manual zoom-in is useful on a dense plan —
// while fitComposed caps *fitting* at 1:1 so a two-card graph is never blown up.
export const ZOOM_LIMITS = { minZoom: MIN_ZOOM, maxZoom: 2.5 }
const GRID_GAP = 36
const GROUP_PAD = 28
const MAX_PER_ROW = 3
const FIT_MARGIN = 48
const TEXT_MAX_W = 150
// A realistic worst-case character in the label font stack measures ~6.4px at
// 11px, so this many fit the text width on one line (measured: 146px of 150).
export const LABEL_CHARS = Math.floor(TEXT_MAX_W / 6.4)

// Missing, self-referential and cyclic parents are dropped: malformed data has
// to land at the root rather than loop any walk up the chain, ours or
// cytoscape's.
function parentMap(nodes: GraphNode[]) {
  const raw = new Map(nodes.map((node) => [node.id, node.parent ?? '']))
  const parents = new Map<string, string>()
  for (const [id, parent] of raw) {
    if (!parent || !raw.has(parent)) continue
    const seen = new Set([id])
    let cursor = parent
    while (cursor !== '' && !seen.has(cursor)) {
      seen.add(cursor)
      cursor = raw.get(cursor) ?? ''
    }
    if (cursor === '') parents.set(id, parent)
  }
  return parents
}

// Where an id was first placed: top-left slot plus the box it reported then.
// Both are frozen, so the center derived from them never changes.
export type LayoutSlot = { x: number; y: number; w?: number; h?: number }

type Rect = { x: number; y: number; w: number; h: number }

// Fixed-size cards packed into a wrapping grid inside their compound. Every id
// gets a center, including compounds: a node with children in one view can be a
// bare card in another, and cytoscape recomputes real compounds from their
// children anyway.
//
// `slots` makes the layout monotonic while view responses trickle in: an id's
// slot is recorded the first time it is placed and reused for the life of the
// map, so a late response can only append cards, never shove drawn ones
// sideways. Omit it to lay out from scratch.
// ponytail: an anchored compound that gains children grows down and right and
// can reach a neighbour anchored below it. Reserve a whole row per compound if
// that ever shows up on a real plan.
export function presetPositions(nodes: GraphNode[], slots = new Map<string, LayoutSlot>()) {
  const parents = parentMap(nodes)
  const children = new Map<string, GraphNode[]>()
  const roots: GraphNode[] = []
  for (const node of nodes) {
    const parent = parents.get(node.id)
    if (parent) {
      const siblings = children.get(parent) ?? []
      siblings.push(node)
      children.set(parent, siblings)
    } else {
      roots.push(node)
    }
  }
  const positions = new Map<string, { x: number; y: number }>()

  function place(node: GraphNode, x: number, y: number): Rect {
    const slot = slots.get(node.id) ?? { x, y }
    slots.set(node.id, slot)
    const kids = children.get(node.id) ?? []
    let box = { w: CARD_W, h: CARD_H }
    if (kids.length > 0) {
      const inner = packGrid(kids, slot.x + GROUP_PAD, slot.y + GROUP_PAD)
      box = { w: inner.w + GROUP_PAD * 2, h: inner.h + GROUP_PAD * 2 }
    }
    slot.w ??= box.w
    slot.h ??= box.h
    positions.set(node.id, { x: slot.x + slot.w / 2, y: slot.y + slot.h / 2 })
    // The live box, not the frozen one, so fresh siblings clear what this
    // compound actually occupies now.
    return { x: slot.x, y: slot.y, ...box }
  }

  function packGrid(items: GraphNode[], x: number, y: number): { w: number; h: number } {
    let right = x
    let bottom = y
    let anchored = false
    const fresh: GraphNode[] = []
    // Anchored items are re-placed in their own slot and only report the space
    // they take; fresh ones start a new row underneath all of it.
    for (const item of items) {
      if (!slots.has(item.id)) {
        fresh.push(item)
        continue
      }
      const rect = place(item, x, y)
      right = Math.max(right, rect.x + rect.w)
      bottom = Math.max(bottom, rect.y + rect.h)
      anchored = true
    }
    let rowX = x
    let rowY = anchored ? bottom + GRID_GAP : y
    let rowH = 0
    let inRow = 0
    for (const item of fresh) {
      if (inRow === MAX_PER_ROW) {
        rowY += rowH + GRID_GAP
        rowX = x
        rowH = 0
        inRow = 0
      }
      const rect = place(item, rowX, rowY)
      rowX += rect.w + GRID_GAP
      rowH = Math.max(rowH, rect.h)
      right = Math.max(right, rowX - GRID_GAP)
      bottom = Math.max(bottom, rowY + rowH)
      inRow++
    }
    return { w: right - x, h: bottom - y }
  }

  packGrid(roots, 0, 0)
  return positions
}

function hasAncestor(parents: Map<string, string>, id: string, ancestorId: string) {
  for (let parent = parents.get(id); parent; parent = parents.get(parent)) {
    if (parent === ancestorId) return true
  }
  return false
}

// Every view is laid out from this one list, so a node shared by Current,
// Proposed and Diff sits in the same place in all of them. The union only
// covers views already fetched, so it grows as responses land — presetPositions
// absorbs that by appending rather than relaying out.
export function unionNodes(graphs: Partial<Record<TopologyView, GraphResponse>>) {
  const union = new Map<string, GraphNode>()
  for (const view of ['current', 'proposed', 'diff'] as const) {
    for (const node of graphs[view]?.nodes ?? []) {
      if (!union.has(node.id)) union.set(node.id, node)
    }
  }
  return [...union.values()]
}

export function graphElements(graph: GraphResponse): GraphElement[] {
  const parents = parentMap(graph.nodes)
  const nodes = graph.nodes.map((node) => {
    const parent = parents.get(node.id)
    return {
      data: {
        id: node.id,
        address: node.address,
        type: node.type,
        name: node.name,
        kind: node.kind,
        label: nodeLabel(node),
        ...(parent ? { parent } : {}),
        ...(node.state ? { state: node.state } : {}),
      },
      classes: [`kind-${className(node.kind)}`, node.state ? `state-${node.state}` : '']
        .filter(Boolean)
        .join(' '),
    }
  })
  // Containment already draws node-inside-ancestor; the matching dependency
  // edge would only sweep across the compound box, so drop it.
  const edges = graph.edges
    .filter(
      (edge) =>
        !hasAncestor(parents, edge.source, edge.target) &&
        !hasAncestor(parents, edge.target, edge.source),
    )
    .map((edge) => ({
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

// Fit with a margin, never magnify past 1:1, then recenter: zooming back out
// leaves the pan cytoscape computed for the magnified fit, which parks the
// graph against the top-left corner of the pane.
export function fitComposed(cy: Core) {
  cy.fit(undefined, FIT_MARGIN)
  if (cy.zoom() > 1) cy.zoom(1)
  cy.center()
}

export const GRAPH_VARS = [
  '--text',
  '--muted',
  '--green',
  '--amber',
  '--orange',
  '--red',
  '--elev-1',
  '--elev-2',
  '--elev-3',
  '--card-border',
  '--edge',
] as const

export type Palette = Record<(typeof GRAPH_VARS)[number], string>

export function graphStyle(palette: Palette): StylesheetJson {
  return [
    {
      selector: 'node',
      style: {
        width: CARD_W,
        height: CARD_H,
        shape: 'round-rectangle',
        'background-color': palette['--elev-3'],
        'background-opacity': 1,
        'border-width': 1,
        'border-color': palette['--card-border'],
        label: 'data(label)',
        color: palette['--text'],
        // Double quotes, not single: cytoscape's font-family validator rejects
        // single-quoted families and silently falls back to its own stack,
        // which would break the character-width math behind LABEL_CHARS.
        'font-family': 'system-ui, -apple-system, "Segoe UI", sans-serif',
        'font-size': 11,
        'text-wrap': 'wrap',
        'text-max-width': `${TEXT_MAX_W}px`,
        // Terraform names are one long underscored token, which whitespace
        // wrapping cannot break out of the card.
        'text-overflow-wrap': 'anywhere',
        'line-height': 1.5,
        'text-halign': 'center',
        'text-valign': 'center',
        'overlay-opacity': 0,
      },
    },
    {
      selector: ':parent',
      style: {
        padding: `${GROUP_PAD}px`,
        'background-color': palette['--elev-1'],
        'border-color': palette['--elev-2'],
        color: palette['--muted'],
        'font-size': 10,
        'text-valign': 'top',
        'text-margin-y': -6,
      },
    },
    {
      selector: ':parent:child',
      style: {
        'background-color': palette['--elev-2'],
        'border-color': palette['--elev-3'],
      },
    },
    {
      selector: 'node:selected',
      style: { 'border-width': 2, 'border-color': palette['--text'] },
    },
    {
      selector: 'node.state-created',
      style: { 'border-width': 2, 'border-color': palette['--green'] },
    },
    {
      selector: 'node.state-changed',
      style: { 'border-width': 2, 'border-color': palette['--amber'] },
    },
    {
      selector: 'node.state-replaced',
      style: { 'border-width': 2, 'border-color': palette['--orange'] },
    },
    {
      selector: 'node.state-destroyed',
      style: {
        'border-width': 2,
        'border-color': palette['--red'],
        'border-style': 'dashed',
      },
    },
    {
      // Fading the card reads as "going away"; fading a compound would drag its
      // whole subtree — tint, children and all — down with it, so the dashed
      // red border carries the meaning there instead.
      selector: 'node.state-destroyed:childless',
      style: { opacity: 0.72 },
    },
    {
      selector: 'edge',
      style: {
        width: 1,
        'curve-style': 'bezier',
        'line-color': palette['--edge'],
        'target-arrow-color': palette['--edge'],
        'target-arrow-shape': 'triangle',
        'arrow-scale': 0.65,
        'overlay-opacity': 0,
      },
    },
    {
      selector: 'edge.state-opened',
      style: { 'line-color': palette['--green'], 'target-arrow-color': palette['--green'] },
    },
    {
      selector: 'edge.state-closed',
      style: {
        'line-color': palette['--red'],
        'target-arrow-color': palette['--red'],
        'line-style': 'dashed',
        opacity: 0.72,
      },
    },
  ]
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
