/// <reference types="node" />

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import cytoscape from 'cytoscape'
import type { Core } from 'cytoscape'
import {
  CARD_W,
  GRAPH_VARS,
  LABEL_CHARS,
  MIN_ZOOM,
  ZOOM_LIMITS,
  clipText,
  fitComposed,
  graphElements,
  graphNodeForSelection,
  graphStyle,
  shouldFetchTopology,
  nodeLabel,
  presetPositions,
  shouldMarkTopologyRunning,
  sourceForAddress,
  topologySignalForDone,
  topologySignalForSummary,
  unionNodes,
} from './topology.ts'
import type { GraphNode, GraphResponse, LayoutSlot, Palette } from './topology.ts'

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

function position(positions: Map<string, { x: number; y: number }>, id: string) {
  return positions.get(id) ?? assert.fail(`no position for ${id}`)
}

const cssVariables = (() => {
  const css = readFileSync(new URL('./app.css', import.meta.url), 'utf8')
  const root = css.slice(css.indexOf(':root'), css.indexOf('}'))
  return new Map([...root.matchAll(/(--[\w-]+):\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]))
})()

const palette = Object.fromEntries(
  GRAPH_VARS.map((name) => [name, cssVariables.get(name) ?? '']),
) as Palette

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

function withGraph(nodes: GraphNode[], run: (cy: Core) => void) {
  const cy = cytoscape({
    headless: true,
    styleEnabled: true,
    elements: graphElements({ view: 'current', runId: 'run-1', nodes, edges: [] }),
    style: graphStyle(palette),
    // The panel's own limits, so the fit-time clamp in fitComposed is the thing
    // under test rather than a maxZoom the tests picked to make it moot.
    ...ZOOM_LIMITS,
  })
  // Headless cytoscape has no container, so its viewport is 1x1 until the size
  // cache is primed; this stands in for an 800x600 pane.
  ;(cy as unknown as { _private: { sizeCache: { width: number; height: number } } })._private.sizeCache = {
    width: 800,
    height: 600,
  }
  cy.layout({
    name: 'preset',
    positions: Object.fromEntries(presetPositions(nodes)),
    animate: false,
    fit: false,
  }).run()
  try {
    run(cy)
  } finally {
    cy.destroy()
  }
}

test('graphElements maps compound parents and diff state classes', () => {
  const elements = graphElements(graph)

  assert.equal(elements[1].data.parent, 'vpc')
  assert.equal(elements[1].classes, 'kind-subnet state-created')
  assert.equal(elements[3].data.source, 'subnet')
  assert.equal(elements[3].classes, 'edge-dependency state-opened')
})

test('graphElements drops dependency edges into an ancestor compound', () => {
  const ids = graphElements(graph)
    .filter((element) => element.data.source)
    .map((element) => element.data.id)

  assert.deepEqual(ids, ['edge-1'])
})

test('graphElements keeps edges between cousins in different compounds', () => {
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

  const ids = graphElements(cousins)
    .filter((element) => element.data.source)
    .map((element) => element.data.id)

  assert.deepEqual(ids, ['cousins'])
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

  const elements = graphElements(malformed)
  // No parent survives, so nothing nests and no dependency edge is swallowed.
  assert.deepEqual(
    elements.filter((element) => !element.data.source).map((element) => element.data.parent),
    [undefined, undefined, undefined, undefined],
  )
  assert.deepEqual(
    elements.filter((element) => element.data.source).map((element) => element.data.id),
    ['a-b', 'self-a'],
  )

  const positions = presetPositions(malformed.nodes)
  assert.deepEqual([...positions.keys()].sort(), ['a', 'b', 'ghost', 'self'])
  assert.equal(position(positions, 'a').y, position(positions, 'b').y)
})

test('positions come from the union of the fetched views so shared nodes never move', () => {
  const current: GraphResponse = {
    view: 'current',
    runId: 'run-1',
    nodes: [node('vpc', 'vpc'), node('subnet', 'subnet', 'vpc'), node('sg', 'security_group', 'vpc')],
    edges: [],
  }
  const diff: GraphResponse = {
    view: 'diff',
    runId: 'run-1',
    nodes: [
      node('vpc', 'vpc'),
      node('new-subnet', 'subnet', 'vpc'),
      node('subnet', 'subnet', 'vpc'),
      node('sg', 'security_group', 'vpc'),
      node('worker', 'instance', 'subnet'),
    ],
    edges: [],
  }

  // Laying each view out on its own is what moved shared nodes between views.
  assert.notDeepEqual(
    position(presetPositions(current.nodes), 'sg'),
    position(presetPositions(diff.nodes), 'sg'),
  )

  // The union is ordered by view, so a node kept by a later view keeps its slot
  // and ids only that view knows about are appended after it.
  assert.deepEqual(
    unionNodes({ current, diff }).map((item) => item.id),
    ['vpc', 'subnet', 'sg', 'new-subnet', 'worker'],
  )
  // Both views render from this one map, so a shared id has exactly one slot.
  // Every id needs one, including 'subnet' — a compound in Diff, a bare card in
  // Current, and unplaceable in Current if compounds were skipped.
  const union = unionNodes({ current, diff })
  const slots = presetPositions(union)
  for (const item of union) assert.ok(slots.has(item.id), `${item.id} has no slot`)
  for (const view of [current, diff]) {
    for (const item of view.nodes) assert.ok(slots.has(item.id), `${view.view}/${item.id} unplaced`)
  }
})

test('a view arriving late appends cards instead of moving the placed ones', () => {
  const diff: GraphResponse = {
    view: 'diff',
    runId: 'run-1',
    nodes: [
      node('vpc', 'vpc'),
      node('new-subnet', 'subnet', 'vpc'),
      node('subnet', 'subnet', 'vpc'),
      node('worker', 'instance', 'subnet'),
    ],
    edges: [],
  }
  const current: GraphResponse = {
    view: 'current',
    runId: 'run-1',
    nodes: [node('vpc', 'vpc'), node('subnet', 'subnet', 'vpc'), node('sg', 'security_group', 'vpc')],
    edges: [],
  }

  // Diff answers first and Current a moment later. unionNodes always lists
  // Current first, so the second layout sees a different order for the same
  // ids — that reshuffle is what used to slide drawn cards across the pane.
  const slots = new Map<string, LayoutSlot>()
  const first = presetPositions(unionNodes({ diff }), slots)
  const second = presetPositions(unionNodes({ current, diff }), slots)

  for (const [id, at] of first) {
    assert.deepEqual(second.get(id), at, `${id} moved once Current arrived`)
  }
  // The newcomer still gets a slot, below what was already placed.
  assert.ok(second.has('sg'))
  assert.ok(position(second, 'sg').y > position(second, 'subnet').y)

  // Without the carried slots the same second layout does move them.
  assert.notDeepEqual(
    position(presetPositions(unionNodes({ current, diff })), 'subnet'),
    position(first, 'subnet'),
  )
})

test('presetPositions packs children inside their compound without overlap', () => {
  const positions = presetPositions(graph.nodes)

  assert.deepEqual([...positions.keys()].sort(), ['sg', 'subnet', 'vpc'])
  const subnet = position(positions, 'subnet')
  const sg = position(positions, 'sg')
  assert.equal(subnet.y, sg.y)
  assert.ok(Math.abs(sg.x - subnet.x) >= CARD_W)
  assert.deepEqual(presetPositions(graph.nodes), positions)
})

test('nodeLabel is two-line type/name text truncated to the card width', () => {
  assert.equal(nodeLabel(graph.nodes[1]), 'aws_subnet\npublic')
  const long = nodeLabel({
    id: 'long',
    address: 'aws_db_subnet_group_association.primary_replica_group',
    type: 'aws_db_subnet_group_association',
    name: 'primary_replica_group_member',
    kind: 'subnet',
  })
  assert.equal(long, 'aws_db_subnet_group_as…\nprimary_replica_group_…')
  for (const line of long.split('\n')) assert.equal([...line].length, LABEL_CHARS)
})

test('truncation counts code points so emoji are never split', () => {
  const label = nodeLabel({
    id: 'emoji',
    address: 'aws_instance.emoji',
    type: 'aws_instance',
    name: `bastion_${'🚀'.repeat(20)}`,
    kind: 'instance',
  })
  const name = label.split('\n')[1]

  assert.equal(Buffer.from(label, 'utf8').toString('utf8'), label)
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

  const rules = graphStyle(palette) as { selector: string; style: Record<string, unknown> }[]
  const flat = JSON.stringify(rules)
  const known = new Set<string>(Object.values(palette))
  for (const hex of flat.match(/#[0-9a-f]{6}/gi) ?? []) {
    assert.ok(known.has(hex), `${hex} is not a palette color`)
  }
  assert.ok(Object.values(palette).every((value) => flat.includes(value)), 'unused palette entry')

  const edgeWidths = rules
    .filter((rule) => rule.selector.startsWith('edge'))
    .map((rule) => rule.style.width)
  assert.deepEqual(new Set(edgeWidths), new Set([1, undefined]))
})

test('a destroyed compound keeps its tint while destroyed cards fade', () => {
  const nodes: GraphNode[] = [
    { ...node('vpc', 'vpc'), state: 'destroyed' },
    { ...node('subnet', 'subnet', 'vpc'), state: 'destroyed' },
    { ...node('sg', 'security_group', 'vpc'), state: 'created' },
  ]

  withGraph(nodes, (cy) => {
    // Fading the compound would drag the elevation tint and every child under
    // it down with it, so only the childless card carries the fade.
    assert.equal(cy.$('#vpc').numericStyle('opacity'), 1)
    assert.equal(cy.$('#subnet').numericStyle('opacity'), 0.72)
    assert.equal(cy.$('#sg').numericStyle('opacity'), 1)
    // The compound still reads as destroyed, through its border alone.
    assert.equal(cy.$('#vpc').style('border-style'), 'dashed')
    assert.equal(cy.$('#vpc').numericStyle('border-width'), 2)
  })
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

  withGraph(nodes, (cy) => {
    fitComposed(cy)
    const cards = cy.nodes().filter((element) => element.isChildless())
    assert.equal(cards.length, 8)

    const boxes = cards.map((card) => ({ id: card.id(), box: card.renderedBoundingBox() }))
    for (const { id, box } of boxes) {
      for (const value of [box.x1, box.y1, box.x2, box.y2]) {
        assert.ok(Number.isFinite(value), `${id} has a non-finite bounding box`)
      }
      assert.ok(box.x1 >= 0 && box.y1 >= 0 && box.x2 <= 800 && box.y2 <= 600, `${id} is outside the viewport`)
      assert.ok(box.w > 0 && box.h > 0, `${id} has no drawn area`)
    }
    for (const [index, a] of boxes.entries()) {
      for (const b of boxes.slice(index + 1)) {
        assert.ok(
          a.box.x2 <= b.box.x1 || b.box.x2 <= a.box.x1 || a.box.y2 <= b.box.y1 || b.box.y2 <= a.box.y1,
          `${a.id} overlaps ${b.id}`,
        )
      }
    }
  })
})

test('fitComposed leaves a real card on screen when the fit zoom bottoms out', () => {
  const nodes = [
    node('vpc', 'vpc'),
    node('a', 'instance', 'vpc'),
    node('b', 'instance', 'vpc'),
    node('c', 'instance', 'vpc'),
  ]

  withGraph(nodes, (cy) => {
    // The historical blank canvas: positions far past what minZoom can shrink.
    cy.nodes()
      .filter((element) => element.isChildless())
      .forEach((element) => {
        element.position({ x: element.position('x') * 1e6, y: element.position('y') * 1e6 })
      })
    fitComposed(cy)

    assert.ok(Number.isFinite(cy.zoom()) && cy.zoom() >= MIN_ZOOM)
    // A card, not the union bounding box: a compound spanning the viewport
    // satisfies an intersection test while every card it holds is off screen.
    const boxes = cy
      .nodes()
      .filter((element) => element.isChildless())
      .map((card) => ({ id: card.id(), box: card.renderedBoundingBox() }))
    for (const { id, box } of boxes) {
      for (const value of [box.x1, box.y1, box.x2, box.y2]) {
        assert.ok(Number.isFinite(value), `${id} has a non-finite bounding box`)
      }
    }
    const onScreen = boxes.filter(
      ({ box }) => box.x2 > 0 && box.x1 < 800 && box.y2 > 0 && box.y1 < 600,
    )
    assert.ok(
      onScreen.length > 0,
      `no card is inside the viewport: ${boxes.map(({ id, box }) => `${id}@${box.x1.toFixed(0)},${box.y1.toFixed(0)}`).join(' ')}`,
    )
    for (const [index, a] of boxes.entries()) {
      for (const b of boxes.slice(index + 1)) {
        assert.ok(
          a.box.x2 <= b.box.x1 || b.box.x2 <= a.box.x1 || a.box.y2 <= b.box.y1 || b.box.y2 <= a.box.y1,
          `${a.id} overlaps ${b.id}`,
        )
      }
    }
  })
})

test('fitComposed never magnifies a small graph past 1:1 and recenters after the clamp', () => {
  withGraph([node('lonely', 'instance')], (cy) => {
    // maxZoom is 2.5 for manual zoom, so fit really does want to magnify here
    // and the 1:1 clamp inside fitComposed is what stops it.
    cy.fit(undefined, 48)
    assert.ok(cy.zoom() > 1, 'the fit clamp is not being exercised')

    fitComposed(cy)
    assert.equal(cy.zoom(), 1)
    const box = cy.$('#lonely').renderedBoundingBox()
    assert.ok(box.x1 > 0 && box.x2 < 800 && box.y1 > 0 && box.y2 < 600)
    // Zooming out keeps the pan cytoscape computed for the magnified fit, which
    // parks the graph against the top-left corner until fitComposed recenters.
    assert.ok(Math.abs((box.x1 + box.x2) / 2 - 400) <= 1, 'graph is not centered horizontally')
    assert.ok(Math.abs((box.y1 + box.y2) / 2 - 300) <= 1, 'graph is not centered vertically')
  })
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

  for (const file of ['topology.ts', 'TopologyPanel.tsx', 'topology.css']) {
    const source = readFileSync(new URL(`./${file}`, import.meta.url), 'utf8')
    assert.ok(!dataUri.test(source), `${file} builds a data: URI`)
    assert.ok(!source.includes('dangerouslySetInnerHTML'), `${file} injects HTML`)
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
  const [element] = graphElements({ ...graph, nodes: [hostile], edges: [] })

  // Cytoscape draws data(label) as canvas text; the raw characters are safe
  // exactly because nothing encodes them into HTML or a data: URI.
  assert.equal(element.data.label, '<script>&"\'\napi <&> "\'')
  assert.ok(
    Object.values(element.data).every((value) => !value.startsWith('data:')),
    'no data: URIs in element data',
  )
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
