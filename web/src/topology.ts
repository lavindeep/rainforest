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

export const CARD_W = 172
export const CARD_H = 58
const MIN_ZOOM = 0.05
// Manual zoom can pass 1:1, while automatic fitting never magnifies cards.
export const ZOOM_LIMITS = { minZoom: MIN_ZOOM, maxZoom: 2.5 }
const GRID_GAP = 36
const GROUP_PAD = 28
const MAX_PER_ROW = 3
const FIT_MARGIN = 48
export const LABEL_CHARS = 23
const MAX_CONTAINMENT_DEPTH = 32

// Missing, self-referential and cyclic parents are dropped: malformed data has
// to land at the root rather than loop any walk up the chain.
function parentMap(nodes: GraphNode[]) {
  const raw = new Map(nodes.map((node) => [node.id, node.parent ?? '']))
  const parents = new Map<string, string>()
  for (const [id, parent] of raw) {
    if (!parent || !raw.has(parent)) continue
    const seen = new Set([id])
    let cursor = parent
    let depth = 0
    while (cursor !== '' && !seen.has(cursor) && depth < MAX_CONTAINMENT_DEPTH) {
      seen.add(cursor)
      cursor = raw.get(cursor) ?? ''
      depth++
    }
    if (cursor === '') parents.set(id, parent)
  }
  return parents
}

// Where an id was first placed: top-left slot plus the box it reported then.
// Both are frozen, so the center derived from them never changes.
export type LayoutSlot = { x: number; y: number; w?: number; h?: number }

export type Rect = { x: number; y: number; w: number; h: number }
export type Point = { x: number; y: number }

export type Viewport = { x: number; y: number; zoom: number; owned: boolean }

// Fixed-size cards packed into a wrapping grid inside their compound. Every id
// gets a center, including compounds: a node with children in one view can be a
// bare card in another.
//
// `slots` makes the layout monotonic while view responses trickle in: an id's
// slot is recorded the first time it is placed and reused for the life of the
// map, so a late response can only append cards, never shove drawn ones
// sideways. Omit it to lay out from scratch.
// ponytail: an anchored compound that gains children grows down and right and
// can reach a neighbour anchored below it. Reserve a whole row per compound if
// that ever shows up on a real plan.
function presetPositions(nodes: GraphNode[], slots = new Map<string, LayoutSlot>()) {
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

// Renderer-neutral graph data: preserve presentation state, but only expose
// containment and dependencies that are valid for this node set.
export function sceneForGraph(graph: GraphResponse): GraphResponse {
  const parents = parentMap(graph.nodes)
  const ids = new Set(graph.nodes.map((node) => node.id))
  return {
    ...graph,
    nodes: graph.nodes.map((node) => {
      const parent = parents.get(node.id)
      const { parent: _parent, ...rest } = node
      return parent ? { ...rest, parent } : rest
    }),
    edges: graph.edges.filter(
      (edge) =>
        ids.has(edge.source) &&
        ids.has(edge.target) &&
        !hasAncestor(parents, edge.source, edge.target) &&
        !hasAncestor(parents, edge.target, edge.source),
    ),
  }
}

export function nodesByContainmentDepth(nodes: GraphNode[]) {
  const byId = new Map(nodes.map((node) => [node.id, node]))
  function depth(node: GraphNode) {
    let value = 0
    const seen = new Set([node.id])
    for (let parent = node.parent; parent && !seen.has(parent); parent = byId.get(parent)?.parent) {
      seen.add(parent)
      value++
    }
    return value
  }
  return [...nodes].sort((first, second) => depth(first) - depth(second))
}

export function layoutTopology(
  scene: GraphResponse,
  slots = new Map<string, LayoutSlot>(),
) {
  presetPositions(scene.nodes, slots)
  const nodes = new Map<string, Rect>()
  const children = new Map<string, string[]>()
  for (const node of scene.nodes) {
    if (!node.parent) continue
    children.set(node.parent, [...(children.get(node.parent) ?? []), node.id])
  }
  function rectangle(id: string): Rect | undefined {
    const existing = nodes.get(id)
    if (existing) return existing
    const slot = slots.get(id)
    if (!slot) return undefined
    const descendants = (children.get(id) ?? []).flatMap((child) => {
      const box = rectangle(child)
      return box ? [box] : []
    })
    const box = descendants.length === 0
      ? {
          x: slot.x + (slot.w ?? CARD_W) / 2 - CARD_W / 2,
          y: slot.y + (slot.h ?? CARD_H) / 2 - CARD_H / 2,
          w: CARD_W,
          h: CARD_H,
        }
      : {
          x: Math.min(slot.x, ...descendants.map((child) => child.x - GROUP_PAD)),
          y: Math.min(slot.y, ...descendants.map((child) => child.y - GROUP_PAD)),
          w: Math.max(...descendants.map((child) => child.x + child.w), slot.x + CARD_W) -
            Math.min(slot.x, ...descendants.map((child) => child.x - GROUP_PAD)) + GROUP_PAD,
          h: Math.max(...descendants.map((child) => child.y + child.h), slot.y + CARD_H) -
            Math.min(slot.y, ...descendants.map((child) => child.y - GROUP_PAD)) + GROUP_PAD,
        }
    nodes.set(id, box)
    return box
  }
  for (const node of scene.nodes) {
    rectangle(node.id)
  }
  const edges = scene.edges.flatMap((edge) => {
    const source = nodes.get(edge.source)
    const target = nodes.get(edge.target)
    if (!source || !target) return []
    return [{
      ...edge,
      points: [
        { x: source.x + source.w / 2, y: source.y + source.h / 2 },
        { x: target.x + target.w / 2, y: target.y + target.h / 2 },
      ],
    }]
  })
  return { nodes, edges }
}

export function fitViewport(
  nodes: Map<string, Rect>,
  size: { width: number; height: number },
  current?: Viewport,
): Viewport {
  if (current?.owned) return current
  if (nodes.size === 0) return { x: size.width / 2, y: size.height / 2, zoom: 1, owned: false }

  const boxes = [...nodes.values()]
  const left = Math.min(...boxes.map((box) => box.x))
  const top = Math.min(...boxes.map((box) => box.y))
  const right = Math.max(...boxes.map((box) => box.x + box.w))
  const bottom = Math.max(...boxes.map((box) => box.y + box.h))
  const zoom = Math.max(
    MIN_ZOOM,
    Math.min(1, (size.width - FIT_MARGIN * 2) / (right - left), (size.height - FIT_MARGIN * 2) / (bottom - top)),
  )
  let x = (size.width - (left + right) * zoom) / 2
  let y = (size.height - (top + bottom) * zoom) / 2

  const visible = boxes.some(
    (box) =>
      box.x * zoom + x + box.w * zoom > 0 &&
      box.x * zoom + x < size.width &&
      box.y * zoom + y + box.h * zoom > 0 &&
      box.y * zoom + y < size.height,
  )
  if (!visible) {
    const node = boxes.reduce((smallest, box) => box.w * box.h < smallest.w * smallest.h ? box : smallest)
    x = size.width / 2 - (node.x + node.w / 2) * zoom
    y = size.height / 2 - (node.y + node.h / 2) * zoom
  }
  return { x, y, zoom, owned: false }
}

export function zoomViewport(viewport: Viewport, zoom: number, pointer: Point): Viewport {
  const nextZoom = Math.max(ZOOM_LIMITS.minZoom, Math.min(ZOOM_LIMITS.maxZoom, zoom))
  const worldX = (pointer.x - viewport.x) / viewport.zoom
  const worldY = (pointer.y - viewport.y) / viewport.zoom
  return {
    x: pointer.x - worldX * nextZoom,
    y: pointer.y - worldY * nextZoom,
    zoom: nextZoom,
    owned: true,
  }
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
