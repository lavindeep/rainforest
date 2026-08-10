import { useCallback, useEffect, useRef, useState } from 'react'
import './app.css'
import PlanPanel from './PlanPanel'
import Sidebar from './Sidebar'
import SourceView from './SourceView'
import TopologyPanel from './TopologyPanel'
import type { TopologyPlanSignal } from './topology'

export type Health = { ok: boolean; version: string; workspace: string }

export type Preflight = {
  terraform: { found: boolean; path: string; version: string }
  initialized: boolean
  awsProfile: string
  awsRegion: string
}

export type Block = {
  kind: string
  type?: string
  name: string
  address: string
  file: string
  line: number
}

export type Scan = {
  files: string[]
  blocks: Block[]
  diagnostics: { file: string; line: number; summary: string }[]
}

type Source = { path: string; content: string }

const PANES = [
  { title: 'Findings', width: '33%', empty: 'Findings appear here after a plan' },
]

function getJSON<T>(url: string): Promise<T | null> {
  return fetch(url, { credentials: 'same-origin' })
    .then((r) => (r.ok ? (r.json() as Promise<T>) : null))
    .catch(() => null)
}

export default function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [preflight, setPreflight] = useState<Preflight | null>(null)
  const [scan, setScan] = useState<Scan | null>(null)
  const [source, setSource] = useState<Source | null>(null)
  const [line, setLine] = useState(0)
  const [tab, setTab] = useState<'plan' | 'source'>('plan')
  const [topologyPlanSignal, setTopologyPlanSignal] = useState<TopologyPlanSignal>({
    kind: 'settled',
    revision: 0,
  })
  const sourceRequest = useRef(0)

  useEffect(() => {
    getJSON<Health>('/api/health').then(setHealth)
    getJSON<Preflight>('/api/preflight').then(setPreflight)
    getJSON<Scan>('/api/workspace').then(setScan)
  }, [])

  const select = useCallback((path: string, at: number) => {
    const request = ++sourceRequest.current
    setTab('source')
    getJSON<Source>(`/api/file?path=${encodeURIComponent(path)}`).then((result) => {
      if (request !== sourceRequest.current) return
      setLine(at)
      setSource(result)
    })
  }, [])

  const signalTopology = useCallback((kind: TopologyPlanSignal['kind']) => {
    setTopologyPlanSignal((current) => ({ kind, revision: current.revision + 1 }))
  }, [])

  return (
    <div className="app">
      <Sidebar
        health={health}
        preflight={preflight}
        scan={scan}
        selected={source?.path ?? ''}
        onSelect={select}
      />

      <main className="strip">
        <section className="pane" style={{ width: '50%' }}>
          <header className="pane-header">Topology</header>
          <div className="pane-body topology-pane-body">
            <TopologyPanel
              scan={scan}
              planSignal={topologyPlanSignal}
              onSelectSource={select}
            />
          </div>
        </section>

        {PANES.map((pane) => (
          <section className="pane" key={pane.title} style={{ width: pane.width }}>
            <header className="pane-header">{pane.title}</header>
            <div className="pane-body">
              <p className="empty">{pane.empty}</p>
            </div>
          </section>
        ))}

        <section className="pane" style={{ width: '66%' }}>
          <header className="pane-header">Work</header>
          <div className="tabs">
            {(['plan', 'source'] as const).map((name) => (
              <button
                key={name}
                type="button"
                className={name === tab ? 'tab current' : 'tab'}
                onClick={() => setTab(name)}
              >
                {name === 'plan' ? 'Plan' : 'Source'}
              </button>
            ))}
          </div>
          <div className="pane-body">
            <div className="work-content" hidden={tab !== 'plan'}>
              <PlanPanel preflight={preflight} onTopologyLifecycle={signalTopology} />
            </div>
            <div className="work-content" hidden={tab !== 'source'}>
              {source ? (
                <SourceView path={source.path} content={source.content} line={line} />
              ) : (
                <p className="empty">Select a file in the navigator to read its source</p>
              )}
            </div>
          </div>
        </section>
      </main>
    </div>
  )
}
