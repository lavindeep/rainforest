/// <reference types="node" />

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  CARD_W,
  EDGE_LABEL_HEIGHT,
  EDGE_LABEL_WIDTH,
  LABEL_CHARS,
  ZOOM_LIMITS,
  clipText,
  edgeLabelPoint,
  graphNodeForSelection,
  glyphForNode,
  fitViewport,
  layoutTopology,
  nodesByContainmentDepth,
  normalizeGraphResponse,
  shouldFetchTopology,
  sceneForGraph,
  shouldMarkTopologyRunning,
  sourceForAddress,
  topologySignalForDone,
  topologySignalForSummary,
  zoomViewport,
} from './topology.ts'
import type { GraphNode, GraphResponse, LayoutSlot, Viewport } from './topology.ts'
import {
  annotationFor,
  annotationLabel,
  annotationsReducer,
  createAnnotationsState,
  emptyAnnotationsDocument,
  getAnnotations,
  limitAnnotationText,
  putAnnotations,
  updateAnnotation,
} from './annotations.ts'
import type { AnnotationsDocument } from './annotations.ts'

const graph: GraphResponse = {
  view: 'diff',
  runId: 'run-1',
  nodes: [
    {
      id: 'vpc',
      address: 'aws_vpc.main',
      type: 'aws_vpc',
      name: 'main',
      kind: 'vpc',
      state: 'unchanged',
    },
    {
      id: 'subnet',
      address: 'aws_subnet.public[0]',
      type: 'aws_subnet',
      name: 'public',
      kind: 'subnet',
      parent: 'vpc',
      state: 'created',
    },
    {
      id: 'sg',
      address: 'aws_security_group.web',
      type: 'aws_security_group',
      name: 'web',
      kind: 'security_group',
      parent: 'vpc',
    },
  ],
  edges: [
    {
      id: 'edge-1',
      source: 'subnet',
      target: 'sg',
      kind: 'dependency',
      state: 'opened',
    },
    {
      id: 'edge-2',
      source: 'subnet',
      target: 'vpc',
      kind: 'dependency',
    },
  ],
}

test('graph responses normalize null collections to empty arrays', () => {
  const normalized = normalizeGraphResponse({
    view: 'proposed',
    runId: 'run-empty',
    nodes: null,
    edges: null,
  })

  assert.deepEqual(normalized, {
    view: 'proposed',
    runId: 'run-empty',
    nodes: [],
    edges: [],
  })
  const scene = sceneForGraph(normalized)
  assert.deepEqual(scene.nodes, [])
  assert.deepEqual(scene.edges, [])
})

function node(id: string, kind: string, parent?: string): GraphNode {
  return {
    id,
    address: `aws_${kind}.${id}`,
    type: `aws_${kind}`,
    name: id,
    kind,
    ...(parent ? { parent } : {}),
  }
}

const cssVariables = (() => {
  const css = readFileSync(new URL('./app.css', import.meta.url), 'utf8')
  const root = css.slice(css.indexOf(':root'), css.indexOf('}'))
  return new Map([...root.matchAll(/(--[\w-]+):\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]))
})()

const GRAPH_VARS = [
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

const TILE_VARS = [
  '--tile-network',
  '--tile-subnet',
  '--tile-route',
  '--tile-gateway',
  '--tile-security',
  '--tile-compute',
  '--tile-interface',
  '--tile-database',
  '--tile-storage',
  '--tile-generic',
] as const

const palette = Object.fromEntries(GRAPH_VARS.map((name) => [name, cssVariables.get(name) ?? ''])) as Record<
  (typeof GRAPH_VARS)[number],
  string
>

function relativeLuminance(hex: string) {
  const channels = hex.match(/\w\w/g) ?? assert.fail(`not a hex color: ${hex}`)
  const [r, g, b] = channels.map((pair) => {
    const value = parseInt(pair, 16) / 255
    return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function contrast(a: string, b: string) {
  const [first, second] = [relativeLuminance(a) + 0.05, relativeLuminance(b) + 0.05]
  return Math.max(first, second) / Math.min(first, second)
}

test('scene preserves valid containment and diff state', () => {
  const scene = sceneForGraph(graph)

  assert.equal(scene.nodes[1].parent, 'vpc')
  assert.equal(scene.nodes[1].state, 'created')
  assert.deepEqual(scene.edges.map((edge) => [edge.id, edge.state]), [['edge-1', 'opened']])
})

test('scene keeps edges between cousins in different compounds', () => {
  const cousins: GraphResponse = {
    view: 'current',
    runId: 'run-1',
    nodes: [
      node('vpc', 'vpc'),
      node('subnet-a', 'subnet', 'vpc'),
      node('subnet-b', 'subnet', 'vpc'),
      node('instance-a', 'instance', 'subnet-a'),
      node('instance-b', 'instance', 'subnet-b'),
    ],
    edges: [
      { id: 'cousins', source: 'instance-a', target: 'instance-b', kind: 'dependency' },
      { id: 'to-vpc', source: 'instance-a', target: 'vpc', kind: 'dependency' },
      { id: 'to-subnet', source: 'subnet-b', target: 'vpc', kind: 'dependency' },
    ],
  }

  assert.deepEqual(sceneForGraph(cousins).edges.map((edge) => edge.id), ['cousins'])
})

test('cyclic and self-referential parents degrade to roots instead of looping', () => {
  const malformed: GraphResponse = {
    view: 'current',
    runId: 'run-1',
    nodes: [
      node('a', 'vpc', 'b'),
      node('b', 'subnet', 'a'),
      node('self', 'instance', 'self'),
      node('ghost', 'instance', 'missing'),
    ],
    edges: [
      { id: 'a-b', source: 'a', target: 'b', kind: 'dependency' },
      { id: 'self-a', source: 'self', target: 'a', kind: 'dependency' },
    ],
  }

  const scene = sceneForGraph(malformed)
  // No parent survives, so nothing nests and no dependency edge is swallowed.
  assert.deepEqual(
    scene.nodes.map((item) => item.parent),
    [undefined, undefined, undefined, undefined],
  )
  assert.deepEqual(scene.edges.map((edge) => edge.id), ['a-b', 'self-a'])
  assert.deepEqual([...layoutTopology(scene).nodes.keys()].sort(), ['a', 'b', 'ghost', 'self'])
})

test('excessively deep containment degrades safely instead of overflowing the stack', () => {
  const nodes = Array.from({ length: 3000 }, (_, index) =>
    node(`node-${index}`, 'instance', index === 0 ? undefined : `node-${index - 1}`),
  )
  const scene = sceneForGraph({ view: 'current', runId: 'run-1', nodes, edges: [] })

  assert.doesNotThrow(() => layoutTopology(scene))
  assert.ok(scene.nodes.some((item, index) => index > 32 && item.parent === undefined))
})

test('compound paint order is outermost to innermost regardless of API order', () => {
  const ordered = nodesByContainmentDepth([
    node('subnet', 'subnet', 'vpc'),
    node('vpc', 'vpc'),
  ])

  assert.deepEqual(ordered.map((item) => item.id), ['vpc', 'subnet'])
})

test('a view arriving late appends cards without moving shared nodes', () => {
  const current: GraphResponse = {
    view: 'current',
    runId: 'run-1',
    nodes: [node('a', 'instance'), node('b', 'instance')],
    edges: [],
  }
  const proposed: GraphResponse = {
    view: 'proposed',
    runId: 'run-1',
    nodes: [node('new', 'instance'), node('b', 'instance'), node('a', 'instance')],
    edges: [],
  }

  const slots = new Map<string, LayoutSlot>()
  const first = layoutTopology(sceneForGraph(current), slots).nodes
  const second = layoutTopology(sceneForGraph(proposed), slots).nodes

  for (const [id, at] of first) {
    const later = second.get(id)
    assert.deepEqual(later, at, `${id} moved once Proposed arrived`)
  }
  assert.ok((second.get('new')?.y ?? 0) > (second.get('a')?.y ?? 0))
})

test('seeding slots from Diff keeps leaf-compound transitions stable and separated', () => {
  const current: GraphResponse = {
    view: 'current',
    runId: 'run-1',
    nodes: [
      node('vpc', 'vpc'),
      node('subnet-a', 'subnet', 'vpc'),
      node('subnet-b', 'subnet', 'vpc'),
    ],
    edges: [],
  }
  const diff: GraphResponse = {
    ...current,
    view: 'diff',
    nodes: [
      ...current.nodes,
      ...['one', 'two', 'three'].map((id) => node(id, 'instance', 'subnet-a')),
    ],
  }
  const slots = new Map<string, LayoutSlot>()
  const canonical = layoutTopology(sceneForGraph(diff), slots)
  const currentLayout = layoutTopology(sceneForGraph(current), slots)
  const restored = layoutTopology(sceneForGraph(diff), slots)
  const leaf = currentLayout.nodes.get('subnet-a') ?? assert.fail('missing current subnet-a')
  const compound = restored.nodes.get('subnet-a') ?? assert.fail('missing diff subnet-a')
  const sibling = restored.nodes.get('subnet-b') ?? assert.fail('missing subnet-b')

  assert.deepEqual(restored, canonical)
  assert.deepEqual(
    { x: leaf.x + leaf.w / 2, y: leaf.y + leaf.h / 2 },
    { x: compound.x + compound.w / 2, y: compound.y + compound.h / 2 },
  )
  assert.ok(!boxesOverlap(compound, sibling), 'sibling overlaps transitioned compound')
})

test('layoutTopology packs leaf cards without overlap', () => {
  const positions = layoutTopology(sceneForGraph(graph)).nodes

  assert.deepEqual([...positions.keys()].sort(), ['sg', 'subnet', 'vpc'])
  const subnet = positions.get('subnet') ?? assert.fail('missing subnet')
  const sg = positions.get('sg') ?? assert.fail('missing sg')
  assert.equal(subnet.y, sg.y)
  assert.ok(subnet.x + subnet.w <= sg.x || sg.x + sg.w <= subnet.x)
  assert.deepEqual(layoutTopology(sceneForGraph(graph)).nodes, positions)
})

test('glyphs map semantic kinds and simple generic resource types', () => {
  const cases = [
    ['vpc', 'aws_vpc', 'network'],
    ['subnet', 'aws_subnet', 'subnet'],
    ['route-table', 'aws_route_table', 'route'],
    ['route', 'aws_route', 'route'],
    ['route-table-association', 'aws_route_table_association', 'route'],
    ['internet-gateway', 'aws_internet_gateway', 'gateway'],
    ['nat-gateway', 'aws_nat_gateway', 'gateway'],
    ['security-group', 'aws_security_group', 'security'],
    ['security-group-rule', 'aws_security_group_rule', 'security'],
    ['instance', 'aws_instance', 'compute'],
    ['eni', 'aws_network_interface', 'interface'],
    ['generic', 'aws_db_instance', 'database'],
    ['generic', 'aws_s3_bucket', 'storage'],
    ['generic', 'terraform_data', 'generic'],
  ] as const

  for (const [kind, type, glyph] of cases) {
    assert.equal(glyphForNode({ ...node('resource', kind), type }), glyph)
  }

  for (const type of [
    'aws_db_parameter_group',
    'aws_s3_bucket_policy',
    'aws_s3_bucket_notification',
    'google_storage_bucket_iam_policy',
  ]) {
    assert.equal(glyphForNode({ ...node('resource', 'generic'), type }), 'generic')
  }
})

function boxesOverlap(
  first: { x: number; y: number; w: number; h: number },
  second: { x: number; y: number; w: number; h: number },
) {
  return first.x < second.x + second.w && first.x + first.w > second.x &&
    first.y < second.y + second.h && first.y + first.h > second.y
}

function segmentCrossesBox(
  start: { x: number; y: number },
  end: { x: number; y: number },
  box: { x: number; y: number; w: number; h: number },
) {
  if (start.x === end.x) {
    return start.x > box.x && start.x < box.x + box.w &&
      Math.max(start.y, end.y) > box.y && Math.min(start.y, end.y) < box.y + box.h
  }
  return start.y > box.y && start.y < box.y + box.h &&
    Math.max(start.x, end.x) > box.x && Math.min(start.x, end.x) < box.x + box.w
}

function denseCompoundGraph(nodeCount: 45 | 80, edgeCount: 170 | 310) {
  const groups = nodeCount === 45 ? 5 : 10
  const childrenPerGroup = nodeCount === 45 ? 8 : 7
  const nodes: GraphNode[] = []
  for (let group = 0; group < groups; group++) {
    nodes.push(node(`group-${group}`, 'subnet'))
    for (let child = 0; child < childrenPerGroup; child++) {
      nodes.push(node(`child-${group}-${child}`, 'instance', `group-${group}`))
    }
  }
  const edges = Array.from({ length: edgeCount }, (_, index) => {
    const sourceGroup = index % groups
    const sourceChild = Math.floor(index / groups) % childrenPerGroup
    const targetGroup = (
      sourceGroup + 1 + Math.floor(index / (groups * childrenPerGroup)) % (groups - 1)
    ) % groups
    const targetChild = (index * 3 + 1) % childrenPerGroup
    return {
      id: `dependency-${index}`,
      source: `child-${sourceGroup}-${sourceChild}`,
      target: `child-${targetGroup}-${targetChild}`,
      kind: 'dependency' as const,
    }
  })
  return sceneForGraph({ view: 'current', runId: 'dense', nodes, edges })
}

function assertEdgeClearsCards(
  scene: GraphResponse,
  layout: ReturnType<typeof layoutTopology>,
  edgeId: string,
) {
  const conflicts = edgeCardConflicts(scene, layout, edgeId)
  assert.equal(conflicts.label, false, `${edgeId} label overlaps a card`)
  assert.equal(conflicts.route, false, `${edgeId} route crosses a card`)
}

function edgeCardConflicts(
  scene: GraphResponse,
  layout: ReturnType<typeof layoutTopology>,
  edgeId: string,
) {
  const edge = layout.edges.find((candidate) => candidate.id === edgeId) ??
    assert.fail(`missing ${edgeId}`)
  const compounds = new Set(scene.nodes.flatMap((item) => item.parent ? [item.parent] : []))
  const leafBoxes = [...layout.nodes].filter(([id]) => !compounds.has(id))
  const labelPoint = edgeLabelPoint(edge.points)
  const labelBox = {
    x: labelPoint.x - EDGE_LABEL_WIDTH / 2,
    y: labelPoint.y - EDGE_LABEL_HEIGHT / 2,
    w: EDGE_LABEL_WIDTH,
    h: EDGE_LABEL_HEIGHT,
  }
  let route = false
  let label = false
  for (const [id, box] of leafBoxes) {
    label ||= boxesOverlap(labelBox, box)
    if (id === edge.source || id === edge.target) continue
    for (let index = 1; index < edge.points.length; index++) {
      route ||= segmentCrossesBox(edge.points[index - 1], edge.points[index], box)
    }
  }
  return { route, label }
}

test('edge routes and labels clear adjacent and intervening cards', () => {
  for (const [name, target] of [['adjacent', 'b'], ['skipped', 'c']] as const) {
    const nodes = [node('a', 'instance'), node('b', 'instance'), node('c', 'instance')]
    const scene = sceneForGraph({
      view: 'current',
      runId: 'run-1',
      nodes,
      edges: [{ id: name, source: 'a', target, kind: 'dependency' }],
    })
    const slots = new Map<string, LayoutSlot>([
      ['a', { x: 0, y: 0, w: 172, h: 58 }],
      ['b', { x: 208, y: 0, w: 172, h: 58 }],
      ['c', { x: 416, y: 0, w: 172, h: 58 }],
    ])
    const layout = layoutTopology(scene, slots)
    const edge = layout.edges[0]
    const label = edgeLabelPoint(edge.points)
    const labelBox = {
      x: label.x - EDGE_LABEL_WIDTH / 2,
      y: label.y - EDGE_LABEL_HEIGHT / 2,
      w: EDGE_LABEL_WIDTH,
      h: EDGE_LABEL_HEIGHT,
    }
    for (const [id, box] of layout.nodes) {
      assert.ok(!boxesOverlap(labelBox, box), `${name} label overlaps ${id}`)
      if (id === edge.source || id === edge.target) continue
      for (let index = 1; index < edge.points.length; index++) {
        const [start, end] = [edge.points[index - 1], edge.points[index]]
        const segment = {
          x: Math.min(start.x, end.x),
          y: Math.min(start.y, end.y),
          w: Math.max(1, Math.abs(start.x - end.x)),
          h: Math.max(1, Math.abs(start.y - end.y)),
        }
        assert.ok(!boxesOverlap(segment, box), `${name} route crosses ${id}`)
      }
    }
  }
})

test('the mock-sized topology stays near 1:1 in its 506x696 pane', () => {
  const nodes = [
    node('vpc', 'vpc'),
    node('public', 'subnet', 'vpc'),
    node('private', 'subnet', 'vpc'),
    node('igw', 'internet-gateway', 'vpc'),
    node('sg', 'security-group', 'vpc'),
    node('web', 'instance', 'public'),
    node('nat', 'nat-gateway', 'public'),
  ]
  const layout = layoutTopology(sceneForGraph({ view: 'current', runId: 'run-1', nodes, edges: [] }))

  assert.ok(fitViewport(layout.nodes, { width: 506, height: 696 }).zoom >= 0.9)
})

test('fitViewport keeps repeated edge lanes and labels inside the canvas', () => {
  const nodes = [node('a', 'instance'), node('b', 'instance'), node('c', 'instance')]
  const scene = sceneForGraph({
    view: 'current',
    runId: 'run-1',
    nodes,
    edges: Array.from({ length: 4 }, (_, index) => ({
      id: `edge-${index}`,
      source: 'a',
      target: 'c',
      kind: 'dependency' as const,
    })),
  })
  const slots = new Map<string, LayoutSlot>([
    ['a', { x: 0, y: 0, w: 172, h: 58 }],
    ['b', { x: 0, y: 94, w: 172, h: 58 }],
    ['c', { x: 0, y: 188, w: 172, h: 58 }],
  ])
  const layout = layoutTopology(scene, slots)
  const size = { width: 506, height: 696 }
  const viewport = fitViewport(layout.nodes, size, undefined, layout.edges)

  for (const edge of layout.edges) {
    for (const point of edge.points) {
      const x = point.x * viewport.zoom + viewport.x
      const y = point.y * viewport.zoom + viewport.y
      assert.ok(x >= 0 && x <= size.width, `${edge.id} path is clipped horizontally`)
      assert.ok(y >= 0 && y <= size.height, `${edge.id} path is clipped vertically`)
    }
    const label = edgeLabelPoint(edge.points)
    const left = (label.x - EDGE_LABEL_WIDTH / 2) * viewport.zoom + viewport.x
    const right = (label.x + EDGE_LABEL_WIDTH / 2) * viewport.zoom + viewport.x
    const top = (label.y - EDGE_LABEL_HEIGHT / 2) * viewport.zoom + viewport.y
    const bottom = (label.y + EDGE_LABEL_HEIGHT / 2) * viewport.zoom + viewport.y
    assert.ok(left >= 0 && right <= size.width, `${edge.id} label is clipped horizontally`)
    assert.ok(top >= 0 && bottom <= size.height, `${edge.id} label is clipped vertically`)
  }
})

test('dense dependency routes stay inside compact panes at minimum zoom', () => {
  for (const [nodeCount, edgeCount] of [[45, 170], [80, 310]] as const) {
    const scene = denseCompoundGraph(nodeCount, edgeCount)
    const layout = layoutTopology(scene)
    const size = { width: 506, height: 696 }
    const viewport = fitViewport(layout.nodes, size, undefined, layout.edges)

    assert.equal(scene.nodes.length, nodeCount)
    assert.equal(scene.edges.length, edgeCount)
    assert.ok(viewport.zoom >= ZOOM_LIMITS.minZoom)
    let routeCrossings = 0
    let labelOverlaps = 0
    for (const edge of layout.edges) {
      const conflicts = edgeCardConflicts(scene, layout, edge.id)
      routeCrossings += Number(conflicts.route)
      labelOverlaps += Number(conflicts.label)
      const label = edgeLabelPoint(edge.points)
      const probes = [
        ...edge.points,
        { x: label.x - EDGE_LABEL_WIDTH / 2, y: label.y - EDGE_LABEL_HEIGHT / 2 },
        { x: label.x + EDGE_LABEL_WIDTH / 2, y: label.y + EDGE_LABEL_HEIGHT / 2 },
      ]
      for (const point of probes) {
        const x = point.x * viewport.zoom + viewport.x
        const y = point.y * viewport.zoom + viewport.y
        assert.ok(x >= 0 && x <= size.width, `${nodeCount}/${edgeCount} ${edge.id} clips x=${x}`)
        assert.ok(y >= 0 && y <= size.height, `${nodeCount}/${edgeCount} ${edge.id} clips y=${y}`)
      }
    }
    assert.ok(routeCrossings <= edgeCount * 0.4, `${routeCrossings} routes cross cards`)
    assert.ok(labelOverlaps <= edgeCount * 0.15, `${labelOverlaps} labels overlap cards`)
  }
})

for (const edgeId of [
  'dependency-1',
  'dependency-8',
  'dependency-12',
  'dependency-21',
] as const) {
  test(`bounded fallback searches for a clear lane for ${edgeId}`, () => {
    const scene = denseCompoundGraph(45, 170)
    assertEdgeClearsCards(scene, layoutTopology(scene), edgeId)
  })
}

test('truncation counts code points so emoji are never split', () => {
  const name = clipText(`bastion_${'🚀'.repeat(20)}`, LABEL_CHARS)

  assert.equal(Buffer.from(name, 'utf8').toString('utf8'), name)
  assert.ok(!/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/.test(name))
  assert.equal([...name].length, LABEL_CHARS)
  assert.ok(name.endsWith('🚀…'))
  assert.equal(clipText('🚀🚀🚀', 2), '🚀…')
  assert.equal(clipText('short', 60), 'short')
})

test('topology colors come from the palette with a visible elevation step', () => {
  for (const name of GRAPH_VARS) {
    assert.match(palette[name], /^#[0-9a-f]{6}$/, `${name} must be a palette hex`)
  }
  const pane = cssVariables.get('--panel') ?? assert.fail('missing --panel')
  const ladder = [pane, palette['--elev-1'], palette['--elev-2'], palette['--elev-3']]
  for (let step = 1; step < ladder.length; step++) {
    assert.ok(
      contrast(ladder[step - 1], ladder[step]) >= 1.25,
      `elevation step ${step} is ${contrast(ladder[step - 1], ladder[step]).toFixed(3)}:1`,
    )
  }
  assert.ok(contrast(palette['--card-border'], palette['--elev-3']) >= 1.35)
  assert.ok(contrast(palette['--text'], palette['--elev-3']) >= 4.5)
  assert.ok(contrast(palette['--muted'], palette['--elev-3']) >= 4.5)
  assert.ok(contrast(palette['--muted'], palette['--elev-1']) >= 4.5)
  for (const state of ['--green', '--amber', '--orange', '--red'] as const) {
    assert.ok(contrast(palette[state], palette['--elev-3']) >= 3, `${state} is low contrast`)
  }
  assert.ok(contrast(palette['--card-border'], palette['--elev-3']) >= 3)
  assert.ok(contrast(palette['--edge'], cssVariables.get('--panel') ?? '') >= 3)
  const glyph = cssVariables.get('--tile-glyph') ?? assert.fail('missing --tile-glyph')
  for (const tile of TILE_VARS) {
    const fill = cssVariables.get(tile) ?? assert.fail(`missing ${tile}`)
    assert.ok(contrast(glyph, fill) >= 3, `${tile} glyph is low contrast`)
  }
  assert.ok(
    contrast(
      cssVariables.get('--edge-label-text') ?? '',
      cssVariables.get('--edge-label-bg') ?? '',
    ) >= 4.5,
  )
  assert.ok(
    contrast(
      cssVariables.get('--edge-label-border') ?? '',
      cssVariables.get('--edge-label-bg') ?? '',
    ) >= 3,
  )

  const css = readFileSync(new URL('./topology.css', import.meta.url), 'utf8')
  for (const name of GRAPH_VARS) assert.ok(css.includes(`var(${name})`), `${name} is unused`)
  assert.ok(!/(?:#[0-9a-f]{3,8}\b|(?:rgb|hsl)a?\()/i.test(css), 'topology.css hardcodes a color')
})

test('a destroyed compound keeps its tint while destroyed cards fade', () => {
  const css = readFileSync(new URL('./topology.css', import.meta.url), 'utf8')
  assert.match(
    css,
    /\.topology-node\.state-destroyed:not\(\.topology-compound\) > \.topology-node-surface\s*{[^}]*fill-opacity:/s,
  )
  const stateStyles = css.slice(
    css.indexOf('.topology-node.state-created'),
    css.indexOf('.topology-status'),
  )
  assert.doesNotMatch(stateStyles, /(^|[^-])opacity\s*:/m)
})

test('rendered geometry stays finite, inside the viewport and non-overlapping', () => {
  const nodes = [
    node('vpc', 'vpc'),
    node('subnet-a', 'subnet', 'vpc'),
    node('subnet-b', 'subnet', 'vpc'),
    node('subnet-c', 'subnet', 'vpc'),
    node('subnet-d', 'subnet', 'vpc'),
    ...['w', 'x', 'y', 'z'].map((id) => node(`instance-${id}`, 'instance', 'subnet-a')),
    node('instance-b', 'instance', 'subnet-b'),
    node('instance-c', 'instance', 'subnet-c'),
    node('instance-d', 'instance', 'subnet-d'),
    node('lonely', 'security_group'),
  ]

  const scene = sceneForGraph({ view: 'current', runId: 'run-1', nodes, edges: [] })
  const layout = layoutTopology(scene)
  const viewport = fitViewport(layout.nodes, { width: 800, height: 600 })
  const compounds = new Set(scene.nodes.flatMap((item) => (item.parent ? [item.parent] : [])))
  const boxes = [...layout.nodes]
    .filter(([id]) => !compounds.has(id))
    .map(([id, box]) => ({
      id,
      x1: box.x * viewport.zoom + viewport.x,
      y1: box.y * viewport.zoom + viewport.y,
      x2: (box.x + box.w) * viewport.zoom + viewport.x,
      y2: (box.y + box.h) * viewport.zoom + viewport.y,
    }))
  assert.equal(boxes.length, 8)
  for (const box of boxes) {
    for (const value of [box.x1, box.y1, box.x2, box.y2]) {
      assert.ok(Number.isFinite(value), `${box.id} has a non-finite bounding box`)
    }
    assert.ok(box.x1 >= 0 && box.y1 >= 0 && box.x2 <= 800 && box.y2 <= 600)
  }
  for (const [index, a] of boxes.entries()) {
    for (const b of boxes.slice(index + 1)) {
      assert.ok(
        a.x2 <= b.x1 || b.x2 <= a.x1 || a.y2 <= b.y1 || b.y2 <= a.y1,
        `${a.id} overlaps ${b.id}`,
      )
    }
  }
})

test('sceneForGraph validates containment and filters only ancestor dependencies', () => {
  const malformed: GraphResponse = {
    view: 'diff',
    runId: 'run-1',
    nodes: [
      { ...node('vpc', 'vpc'), state: 'destroyed' },
      { ...node('subnet-a', 'subnet', 'vpc'), state: 'changed' },
      node('subnet-b', 'subnet', 'vpc'),
      { ...node('instance-a', 'instance', 'subnet-a'), state: 'created' },
      node('instance-b', 'instance', 'subnet-b'),
      node('cycle-a', 'instance', 'cycle-b'),
      node('cycle-b', 'instance', 'cycle-a'),
      node('ghost', 'instance', 'missing'),
    ],
    edges: [
      { id: 'ancestor', source: 'instance-a', target: 'vpc', kind: 'dependency', state: 'closed' },
      { id: 'parent', source: 'instance-a', target: 'subnet-a', kind: 'dependency' },
      { id: 'cousins', source: 'instance-a', target: 'instance-b', kind: 'dependency', state: 'opened' },
    ],
  }

  const scene = sceneForGraph(malformed)
  assert.equal(scene.nodes.find((item) => item.id === 'subnet-a')?.parent, 'vpc')
  assert.deepEqual(
    ['cycle-a', 'cycle-b', 'ghost'].map(
      (id) => scene.nodes.find((item) => item.id === id)?.parent,
    ),
    [undefined, undefined, undefined],
  )
  assert.deepEqual(scene.edges.map((edge) => [edge.id, edge.state]), [['cousins', 'opened']])
  assert.deepEqual(
    ['vpc', 'subnet-a', 'instance-a'].map((id) => [
      id,
      scene.nodes.find((item) => item.id === id)?.state,
    ]),
    [
      ['vpc', 'destroyed'],
      ['subnet-a', 'changed'],
      ['instance-a', 'created'],
    ],
  )
})

test('layoutTopology is finite, deterministic, enclosing, and stable across late views', () => {
  const current: GraphResponse = {
    view: 'current',
    runId: 'run-1',
    nodes: [node('vpc', 'vpc'), node('subnet', 'subnet', 'vpc'), node('sg', 'security_group', 'vpc')],
    edges: [],
  }
  const diff: GraphResponse = {
    view: 'diff',
    runId: 'run-1',
    nodes: [...current.nodes, node('worker', 'instance', 'subnet')],
    edges: [{ id: 'worker-sg', source: 'worker', target: 'sg', kind: 'dependency' }],
  }
  const slots = new Map<string, LayoutSlot>()
  const first = layoutTopology(sceneForGraph(current), slots)
  const late = layoutTopology(sceneForGraph(diff), slots)

  assert.deepEqual(layoutTopology(sceneForGraph(diff)), layoutTopology(sceneForGraph(diff)))
  for (const [id, box] of late.nodes) {
    for (const value of [box.x, box.y, box.w, box.h]) {
      assert.ok(Number.isFinite(value), `${id} has non-finite geometry`)
    }
    assert.ok(box.w > 0 && box.h > 0)
  }
  assert.deepEqual(late.nodes.get('sg'), first.nodes.get('sg'), 'shared leaf moved')

  for (const [parentId, childId] of [['vpc', 'subnet'], ['subnet', 'worker']]) {
    const parent = late.nodes.get(parentId) ?? assert.fail(`missing ${parentId}`)
    const child = late.nodes.get(childId) ?? assert.fail(`missing ${childId}`)
    assert.ok(
      child.x >= parent.x && child.y >= parent.y &&
        child.x + child.w <= parent.x + parent.w && child.y + child.h <= parent.y + parent.h,
      `${parentId} does not enclose ${childId}`,
    )
  }
  for (const edge of late.edges) {
    assert.equal(edge.points.length, 4)
    for (const point of edge.points) {
      assert.ok(Number.isFinite(point.x) && Number.isFinite(point.y), `${edge.id} has a non-finite point`)
    }
    for (let index = 1; index < edge.points.length; index++) {
      const [a, b] = [edge.points[index - 1], edge.points[index]]
      assert.ok(a.x === b.x || a.y === b.y, `${edge.id} segment ${index} is diagonal`)
    }

    const source = late.nodes.get(edge.source) ?? assert.fail(`missing ${edge.source}`)
    const target = late.nodes.get(edge.target) ?? assert.fail(`missing ${edge.target}`)
    const onBoundary = (point: { x: number; y: number }, box: typeof source) =>
      point.x >= box.x && point.x <= box.x + box.w &&
      point.y >= box.y && point.y <= box.y + box.h &&
      (point.x === box.x || point.x === box.x + box.w || point.y === box.y || point.y === box.y + box.h)
    assert.ok(onBoundary(edge.points[0], source), `${edge.id} starts away from its source boundary`)
    assert.ok(onBoundary(edge.points.at(-1) ?? assert.fail('missing endpoint'), target), `${edge.id} ends away from its target boundary`)

    const segments = edge.points.slice(1).map((point, index) => [edge.points[index], point] as const)
    const length = ([a, b]: (typeof segments)[number]) => Math.abs(a.x - b.x) + Math.abs(a.y - b.y)
    const longest = segments.reduce((best, segment) => length(segment) > length(best) ? segment : best)
    const label = edgeLabelPoint(edge.points)
    const [a, b] = longest
    assert.ok(
      (a.x === b.x && label.x === a.x && label.y >= Math.min(a.y, b.y) && label.y <= Math.max(a.y, b.y)) ||
      (a.y === b.y && label.y === a.y && label.x >= Math.min(a.x, b.x) && label.x <= Math.max(a.x, b.x)),
      `${edge.id} label is not on its longest segment`,
    )
  }
})

test('fitViewport fits 800x600, caps one card at 1, and survives x1e6 sabotage', () => {
  const layout = layoutTopology(sceneForGraph(graph))
  const fitted = fitViewport(layout.nodes, { width: 800, height: 600 })
  assert.ok(fitted.zoom >= ZOOM_LIMITS.minZoom && fitted.zoom <= 1)

  for (const [id, box] of layout.nodes) {
    const x1 = box.x * fitted.zoom + fitted.x
    const y1 = box.y * fitted.zoom + fitted.y
    assert.ok(x1 >= 0 && y1 >= 0, `${id} starts outside the viewport`)
    assert.ok(x1 + box.w * fitted.zoom <= 800 && y1 + box.h * fitted.zoom <= 600)
  }

  const one = fitViewport(new Map([['one', { x: 0, y: 0, w: CARD_W, h: 58 }]]), {
    width: 800,
    height: 600,
  })
  assert.equal(one.zoom, 1)

  const sabotaged = new Map(
    [...layout.nodes].map(([id, box]) => [id, { ...box, x: box.x * 1e6, y: box.y * 1e6 }]),
  )
  const rescued = fitViewport(sabotaged, { width: 800, height: 600 })
  assert.ok([rescued.x, rescued.y, rescued.zoom].every(Number.isFinite))
  assert.ok(rescued.zoom >= ZOOM_LIMITS.minZoom)
  assert.ok(
    [...sabotaged.values()].some((box) => {
      const x = box.x * rescued.zoom + rescued.x
      const y = box.y * rescued.zoom + rescued.y
      return x + box.w * rescued.zoom > 0 && x < 800 && y + box.h * rescued.zoom > 0 && y < 600
    }),
    'no node is inside the viewport',
  )
})

test('zoomViewport clamps around the pointer and fit preserves an owned viewport on resize', () => {
  const start: Viewport = { x: 120, y: 80, zoom: 1.75, owned: false }
  const pointer = { x: 300, y: 240 }
  const world = {
    x: (pointer.x - start.x) / start.zoom,
    y: (pointer.y - start.y) / start.zoom,
  }
  const zoomed = zoomViewport(start, 2, pointer)

  assert.deepEqual(
    { x: world.x * zoomed.zoom + zoomed.x, y: world.y * zoomed.zoom + zoomed.y },
    pointer,
  )
  assert.equal(zoomed.owned, true)
  assert.equal(zoomViewport(start, 0.001, pointer).zoom, ZOOM_LIMITS.minZoom)
  assert.equal(zoomViewport(start, 100, pointer).zoom, ZOOM_LIMITS.maxZoom)

  const owned: Viewport = { x: -125, y: 42, zoom: 1.75, owned: true }
  const nodes = layoutTopology(sceneForGraph(graph)).nodes
  assert.deepEqual(fitViewport(nodes, { width: 1200, height: 700 }, owned), owned)
})

test('topology sources render text only, never markup or URIs', () => {
  // RFC 2397 makes the media type optional, so `data:,payload` is a live URI
  // that a `data:[a-z]` scan waves through. Whitespace after the colon is the
  // one thing that cannot start a URI, and it is what object keys look like.
  const dataUri = /\bdata:(?!\s)/i
  for (const uri of ['data:,x', 'data:text/plain,x', 'src="data:image/svg+xml;base64,AA"', '"data:" + payload']) {
    assert.ok(dataUri.test(uri), `${uri} should be caught`)
  }
  for (const safe of ['data: {', 'const shape = { data: {} }', 'data:\n  value']) {
    assert.ok(!dataUri.test(safe), `${safe} should not be caught`)
  }

  for (const file of [
    'annotations.ts',
    'AnnotationEditor.tsx',
    'topology.ts',
    'TopologyPanel.tsx',
    'SvgTopology.tsx',
    'TopologyGlyph.tsx',
    'topology.css',
  ]) {
    const source = readFileSync(new URL(`./${file}`, import.meta.url), 'utf8')
    assert.ok(!dataUri.test(source), `${file} builds a data: URI`)
    assert.ok(!source.includes('dangerouslySetInnerHTML'), `${file} injects HTML`)
    assert.ok(!source.includes('<foreignObject'), `${file} embeds HTML in SVG`)
    assert.ok(!/\b(?:href|xlinkHref)\s*=/.test(source), `${file} creates a dynamic SVG link`)
    if (file !== 'topology.css') {
      assert.ok(!/#[0-9a-fA-F]{6}\b/.test(source), `${file} hardcodes a color outside the palette`)
    }
  }
})

test('hostile resource names stay plain label strings, never markup or URIs', () => {
  const hostile = {
    id: 'unsafe',
    address: 'aws_instance.unsafe',
    type: '<script>&"\'',
    name: 'api <&> "\'',
    kind: 'instance',
  }
  const [item] = sceneForGraph({ ...graph, nodes: [hostile], edges: [] }).nodes

  assert.equal(item.type, '<script>&"\'')
  assert.equal(item.name, 'api <&> "\'')
  assert.ok(Object.values(item).every((value) => !String(value).startsWith('data:')))
})

test('sourceForAddress matches exact and indexed root resources only', () => {
  const blocks = [
    {
      kind: 'resource',
      name: 'web',
      address: 'aws_instance.web',
      file: 'main.tf',
      line: 12,
    },
    {
      kind: 'module',
      name: 'network',
      address: 'module.network',
      file: 'main.tf',
      line: 30,
    },
  ]

  assert.deepEqual(sourceForAddress('aws_instance.web', blocks), { path: 'main.tf', line: 12 })
  assert.deepEqual(sourceForAddress('aws_instance.web["blue"]', blocks), {
    path: 'main.tf',
    line: 12,
  })
  assert.equal(sourceForAddress('module.network.aws_subnet.public', blocks), null)
  assert.equal(sourceForAddress('aws_instance.missing', blocks), null)
})

test('topologySignalForSummary selects Diff only for a usable changed plan', () => {
  assert.equal(
    topologySignalForSummary({ state: 'succeeded', noChanges: false, showError: '' }),
    'changed',
  )
  assert.equal(
    topologySignalForSummary({ state: 'succeeded', noChanges: true, showError: '' }),
    'settled',
  )
  assert.equal(
    topologySignalForSummary({ state: 'succeeded', noChanges: false, showError: 'show failed' }),
    'settled',
  )
  assert.equal(
    topologySignalForSummary({ state: 'failed', noChanges: false, showError: '' }),
    'settled',
  )
})

test('done settles only the pending or matching topology run before summary selects Diff', () => {
  assert.equal(topologySignalForDone('run-1', 'pending'), 'settled')
  assert.equal(topologySignalForDone('run-1', 'run-1'), 'settled')
  assert.equal(topologySignalForDone('run-1', ''), null)
  assert.equal(topologySignalForDone('', ''), null)
  assert.equal(topologySignalForDone('run-1', 'run-2'), null)
  assert.equal(
    topologySignalForSummary({ state: 'succeeded', noChanges: false, showError: '' }),
    'changed',
  )
})

test('graphNodeForSelection resolves only nodes in the active graph', () => {
  assert.equal(graphNodeForSelection(graph.nodes, 'subnet')?.address, 'aws_subnet.public[0]')
  assert.equal(graphNodeForSelection(graph.nodes, ''), null)
  assert.equal(graphNodeForSelection(graph.nodes, 'missing'), null)
})

test('completed summary suppresses running signals from SSE replay', () => {
  assert.equal(shouldMarkTopologyRunning('run-1', ''), true)
  assert.equal(shouldMarkTopologyRunning('run-1', 'run-2'), true)
  assert.equal(shouldMarkTopologyRunning('run-1', 'run-1'), false)
})

test('running plan reuses Current instead of fetching during Terraform work', () => {
  assert.equal(shouldFetchTopology('current', true), false)
  assert.equal(shouldFetchTopology('current', false), true)
  assert.equal(shouldFetchTopology('diff', false), true)
})

const savedAnnotations: AnnotationsDocument = {
  version: 1,
  nodes: {
    orphan: { label: 'Keep me', description: 'Not in the active graph' },
    edited: { label: 'Old', description: 'Old description' },
  },
  edges: { 'old-edge': { label: 'Traffic', description: '' } },
}

test('annotation updates preserve orphan entries and delete only an empty edited entry', () => {
  const updated = updateAnnotation(savedAnnotations, { kind: 'node', key: 'edited' }, {
    label: 'New',
    description: 'New description',
  })
  const deleted = updateAnnotation(updated, { kind: 'node', key: 'edited' }, {
    label: '',
    description: '',
  })

  assert.deepEqual(annotationFor(updated, { kind: 'node', key: 'edited' }), {
    label: 'New',
    description: 'New description',
  })
  assert.deepEqual(deleted, {
    version: 1,
    nodes: { orphan: savedAnnotations.nodes.orphan },
    edges: savedAnnotations.edges,
  })
  assert.deepEqual(annotationFor(deleted, { kind: 'edge', key: 'missing' }), {
    label: '',
    description: '',
  })
})

test('annotation labels fall back for whitespace and text limits count code points', () => {
  const whitespace = updateAnnotation(
    emptyAnnotationsDocument(),
    { kind: 'node', key: 'node' },
    { label: '   ', description: '' },
  )

  assert.equal(annotationLabel(whitespace, { kind: 'node', key: 'node' }, 'default'), 'default')
  assert.equal(limitAnnotationText('🙂'.repeat(81), 80), '🙂'.repeat(80))
  assert.equal(limitAnnotationText('界'.repeat(4001), 4000), '界'.repeat(4000))
})

test('annotation GET and PUT use same-origin credentials and expose server errors', async () => {
  const calls: Array<[RequestInfo | URL, RequestInit | undefined]> = []
  const okFetch: typeof fetch = async (input, init) => {
    calls.push([input, init])
    return new Response(JSON.stringify(savedAnnotations), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  assert.deepEqual(await getAnnotations(okFetch), savedAnnotations)
  assert.deepEqual(await putAnnotations(emptyAnnotationsDocument(), okFetch), savedAnnotations)
  assert.deepEqual(calls, [
    ['/api/annotations', { credentials: 'same-origin' }],
    ['/api/annotations', {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(emptyAnnotationsDocument()),
    }],
  ])

  const denied: typeof fetch = async () => new Response(
    JSON.stringify({ error: 'annotations file must be a regular file' }),
    { status: 400, headers: { 'content-type': 'application/json' } },
  )
  await assert.rejects(() => getAnnotations(denied), /annotations file must be a regular file/)
  await assert.rejects(() => putAnnotations(savedAnnotations, denied), /annotations file must be a regular file/)
})

test('annotation responses normalize omitted fields and reject invalid wire shapes', async () => {
  const partial: typeof fetch = async () => new Response(JSON.stringify({
    version: 1,
    nodes: { labelOnly: { label: 'Label' }, empty: {} },
    edges: { descriptionOnly: { description: 'Description' } },
  }))
  assert.deepEqual(await getAnnotations(partial), {
    version: 1,
    nodes: { labelOnly: { label: 'Label', description: '' } },
    edges: { descriptionOnly: { label: '', description: 'Description' } },
  })

  for (const body of [
    { version: 2, nodes: {}, edges: {} },
    { version: 1, nodes: {}, edges: {}, extra: true },
    { version: 1, nodes: null, edges: {} },
    { version: 1, nodes: { '': { label: 'empty key' } }, edges: {} },
    { version: 1, nodes: { ['x'.repeat(1025)]: { label: 'long key' } }, edges: {} },
    { version: 1, nodes: { invalid: { label: 3 } }, edges: {} },
    { version: 1, nodes: { invalid: { label: '🙂'.repeat(81) } }, edges: {} },
    { version: 1, nodes: { invalid: { description: '界'.repeat(4001) } }, edges: {} },
  ]) {
    const invalid: typeof fetch = async () => new Response(JSON.stringify(body))
    await assert.rejects(() => getAnnotations(invalid), /invalid annotations response/)
  }
})

test('edge markers keep a fixed size and selected diff nodes keep their state color', () => {
  const svg = readFileSync(new URL('./SvgTopology.tsx', import.meta.url), 'utf8')
  assert.equal(svg.match(/markerUnits="userSpaceOnUse"/g)?.length, 3)

  const css = readFileSync(new URL('./topology.css', import.meta.url), 'utf8')
  const selected = [...css.matchAll(
    /\.topology-node\.selected > \.topology-node-surface,[\s\S]*?\{([^}]*)\}/g,
  )].at(-1)?.[1] ?? assert.fail('missing selected node style')
  assert.doesNotMatch(selected, /stroke\s*:/)
})

test('only the edge pointer target keeps an approximately 24px hit width while zooming', () => {
  const svg = readFileSync(new URL('./SvgTopology.tsx', import.meta.url), 'utf8')
  assert.match(
    svg,
    /className="topology-edge-hit"[^>]*vectorEffect="non-scaling-stroke"/,
  )
  assert.equal(svg.match(/vectorEffect="non-scaling-stroke"/g)?.length, 1)

  const css = readFileSync(new URL('./topology.css', import.meta.url), 'utf8')
  const hit = css.match(/\.topology-edge-hit\s*{([^}]*)}/s)?.[1] ?? assert.fail('missing edge hit style')
  assert.match(hit, /stroke-width:\s*24(?:px)?\s*;/)
})

test('annotation detail and textarea are bounded inside compact topology panes', () => {
  const css = readFileSync(new URL('./topology.css', import.meta.url), 'utf8')
  const detail = css.match(/\.topology-detail\s*{([^}]*)}/s)?.[1] ?? assert.fail('missing detail style')
  assert.match(detail, /max-height:\s*calc\(100%\s*-\s*24px\)\s*;/)
  assert.match(detail, /overflow-y:\s*auto\s*;/)

  const textarea = [...css.matchAll(/\.topology-annotation textarea\s*{([^}]*)}/gs)].at(-1)?.[1] ??
    assert.fail('missing annotation textarea style')
  assert.match(textarea, /max-height:\s*120px\s*;/)

  const panel = readFileSync(new URL('./TopologyPanel.tsx', import.meta.url), 'utf8')
  const stageStart = panel.indexOf('<div className="topology-stage">')
  const detailStart = panel.indexOf('<aside className="topology-detail"')
  assert.ok(stageStart >= 0 && detailStart >= 0)
  const tags = /<div\b|<\/div>/g
  tags.lastIndex = stageStart
  let depth = 0
  let stageEnd = -1
  for (let match = tags.exec(panel); match; match = tags.exec(panel)) {
    depth += match[0] === '</div>' ? -1 : 1
    if (depth === 0) {
      stageEnd = match.index
      break
    }
  }
  assert.ok(detailStart > stageStart && detailStart < stageEnd, 'detail must be inside topology-stage')
})

test('annotation editor does not use UTF-16 maxLength limits', () => {
  const source = readFileSync(new URL('./AnnotationEditor.tsx', import.meta.url), 'utf8')
  assert.doesNotMatch(source, /\bmaxLength=/)
})

test('a delayed annotation load cannot overwrite an edit', () => {
  let state = createAnnotationsState()
  state = annotationsReducer(state, { type: 'load-start', requestId: 1, revision: 0 })
  state = annotationsReducer(state, {
    type: 'edit',
    target: { kind: 'node', key: 'edited' },
    annotation: { label: 'Local', description: '' },
  })
  state = annotationsReducer(state, {
    type: 'load-success',
    requestId: 1,
    revision: 0,
    document: savedAnnotations,
  })

  assert.equal(annotationFor(state.document, { kind: 'node', key: 'edited' }).label, 'Local')
  assert.equal(state.dirty, true)
  assert.equal(state.loaded, false)
})

test('an older same-revision load cannot overwrite the newest response', () => {
  const newest: AnnotationsDocument = {
    version: 1,
    nodes: { newest: { label: 'Newest', description: '' } },
    edges: {},
  }
  let state = createAnnotationsState()
  state = annotationsReducer(state, { type: 'load-start', requestId: 1, revision: 0 })
  state = annotationsReducer(state, { type: 'load-start', requestId: 2, revision: 0 })
  state = annotationsReducer(state, {
    type: 'load-success',
    requestId: 2,
    revision: 0,
    document: newest,
  })
  state = annotationsReducer(state, {
    type: 'load-success',
    requestId: 1,
    revision: 0,
    document: savedAnnotations,
  })

  assert.deepEqual(state.document, newest)
  assert.equal(state.loaded, true)
})

test('a current load failure leaves annotations unavailable for saving', () => {
  let state = createAnnotationsState(savedAnnotations)
  state = annotationsReducer(state, { type: 'load-start', requestId: 1, revision: 0 })
  state = annotationsReducer(state, {
    type: 'load-failure',
    requestId: 1,
    revision: 0,
    error: 'cannot read annotations file',
  })

  assert.equal(state.loaded, false)
  assert.equal(state.saved, false)
  assert.equal(state.error, 'cannot read annotations file')
})

test('editing during a save keeps dirty state when the older response arrives', () => {
  let state = createAnnotationsState(savedAnnotations)
  state = annotationsReducer(state, {
    type: 'edit',
    target: { kind: 'node', key: 'edited' },
    annotation: { label: 'First edit', description: '' },
  })
  state = annotationsReducer(state, { type: 'save-start', requestId: 1, revision: state.revision })
  const savingRevision = state.revision
  state = annotationsReducer(state, {
    type: 'edit',
    target: { kind: 'node', key: 'edited' },
    annotation: { label: 'Newer edit', description: '' },
  })
  state = annotationsReducer(state, {
    type: 'save-success',
    requestId: 1,
    revision: savingRevision,
    document: savedAnnotations,
  })

  assert.equal(annotationFor(state.document, { kind: 'node', key: 'edited' }).label, 'Newer edit')
  assert.equal(state.dirty, true)
  assert.equal(state.saved, false)
})

test('an older same-revision save cannot overwrite the newest response', () => {
  const newest = updateAnnotation(savedAnnotations, { kind: 'node', key: 'edited' }, {
    label: 'Normalized newest',
    description: '',
  })
  let state = createAnnotationsState(savedAnnotations)
  state = annotationsReducer(state, {
    type: 'edit',
    target: { kind: 'node', key: 'edited' },
    annotation: { label: 'Draft', description: '' },
  })
  state = annotationsReducer(state, { type: 'save-start', requestId: 1, revision: 1 })
  state = annotationsReducer(state, { type: 'save-start', requestId: 2, revision: 1 })
  state = annotationsReducer(state, {
    type: 'save-success',
    requestId: 2,
    revision: 1,
    document: newest,
  })
  state = annotationsReducer(state, {
    type: 'save-success',
    requestId: 1,
    revision: 1,
    document: savedAnnotations,
  })

  assert.deepEqual(state.document, newest)
  assert.equal(state.saved, true)
})

test('failed annotation saves retain edits and surface the error', () => {
  let state = createAnnotationsState(savedAnnotations)
  state = annotationsReducer(state, {
    type: 'edit',
    target: { kind: 'edge', key: 'old-edge' },
    annotation: { label: 'Edited traffic', description: '' },
  })
  state = annotationsReducer(state, { type: 'save-start', requestId: 1, revision: state.revision })
  state = annotationsReducer(state, {
    type: 'save-failure',
    requestId: 1,
    revision: state.revision,
    error: 'disk full',
  })

  assert.equal(annotationFor(state.document, { kind: 'edge', key: 'old-edge' }).label, 'Edited traffic')
  assert.equal(state.dirty, true)
  assert.equal(state.error, 'disk full')
  assert.equal(state.saved, false)
})

test('successful saves accept normalization and a fresh load restores the saved document', () => {
  let state = createAnnotationsState(savedAnnotations)
  state = annotationsReducer(state, {
    type: 'edit',
    target: { kind: 'node', key: 'edited' },
    annotation: { label: '', description: '' },
  })
  state = annotationsReducer(state, { type: 'save-start', requestId: 1, revision: state.revision })
  const normalized = updateAnnotation(savedAnnotations, { kind: 'node', key: 'edited' }, {
    label: '',
    description: '',
  })
  state = annotationsReducer(state, {
    type: 'save-success',
    requestId: 1,
    revision: state.revision,
    document: normalized,
  })

  assert.deepEqual(state.document, normalized)
  assert.equal(state.dirty, false)
  assert.equal(state.error, '')
  assert.equal(state.saved, true)

  let restored = createAnnotationsState()
  restored = annotationsReducer(restored, { type: 'load-start', requestId: 1, revision: 0 })
  restored = annotationsReducer(restored, {
    type: 'load-success',
    requestId: 1,
    revision: 0,
    document: normalized,
  })
  assert.deepEqual(restored.document, normalized)
  assert.equal(restored.dirty, false)
  assert.equal(restored.loaded, true)
  assert.equal(restored.saved, false)
})
