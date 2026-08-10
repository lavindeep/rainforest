import { useEffect, useState } from 'react'
import './app.css'
import Sidebar from './Sidebar'
import SourceView from './SourceView'

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
  { title: 'Topology', width: '50%', empty: 'Graph appears here after a plan' },
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

  useEffect(() => {
    getJSON<Health>('/api/health').then(setHealth)
    getJSON<Preflight>('/api/preflight').then(setPreflight)
    getJSON<Scan>('/api/workspace').then(setScan)
  }, [])

  function select(path: string, at: number) {
    setLine(at)
    getJSON<Source>(`/api/file?path=${encodeURIComponent(path)}`).then(setSource)
  }

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
          <div className="pane-body">
            {source ? (
              <SourceView path={source.path} content={source.content} line={line} />
            ) : (
              <p className="empty">Source, diffs and Terraform output appear here</p>
            )}
          </div>
        </section>
      </main>
    </div>
  )
}
