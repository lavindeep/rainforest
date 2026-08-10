/// <reference types="node" />

import assert from 'node:assert/strict'
import test from 'node:test'
import {
  graphElements,
  graphNodeForSelection,
  shouldFetchTopology,
  nodeCardDataUri,
  shouldMarkTopologyRunning,
  sourceForAddress,
  topologySignalForSummary,
} from './topology.ts'
import type { GraphResponse } from './topology.ts'

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
  ],
  edges: [
    {
      id: 'edge-1',
      source: 'subnet',
      target: 'vpc',
      kind: 'dependency',
      state: 'opened',
    },
  ],
}

test('graphElements maps compound parents and diff state classes', () => {
  const elements = graphElements(graph)

  assert.equal(elements[1].data.parent, 'vpc')
  assert.equal(elements[1].classes, 'kind-subnet state-created')
  assert.equal(elements[2].data.source, 'subnet')
  assert.equal(elements[2].classes, 'edge-dependency state-opened')
})

test('nodeCardDataUri escapes untrusted graph labels inside SVG', () => {
  const uri = nodeCardDataUri({
    id: 'unsafe',
    address: 'aws_instance.unsafe',
    type: '<script>&"\'',
    name: 'api <&> "\'',
    kind: 'instance',
  })
  const svg = decodeURIComponent(uri.slice(uri.indexOf(',') + 1))

  assert.match(svg, /&lt;script&gt;&amp;&quot;&apos;/)
  assert.match(svg, /api &lt;&amp;&gt; &quot;&apos;/)
  assert.doesNotMatch(svg, /<script>/)
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
