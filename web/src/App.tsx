import { useEffect, useState } from 'react'
import './app.css'

type Health = { ok: boolean; version: string; workspace: string }

const PANES = [
  { title: 'Topology', width: '50%', empty: 'Graph appears here after a plan' },
  { title: 'Findings', width: '33%', empty: 'Findings appear here after a plan' },
  { title: 'Work', width: '66%', empty: 'Source, diffs and Terraform output appear here' },
]

export default function App() {
  const [health, setHealth] = useState<Health | null>(null)

  useEffect(() => {
    fetch('/api/health', { credentials: 'same-origin' })
      .then((r) => (r.ok ? r.json() : null))
      .then(setHealth)
      .catch(() => setHealth(null))
  }, [])

  return (
    <div className="app">
      <aside className="sidebar">
        <h1 className="brand">Rain Forest</h1>
        <section className="navigator">
          <h2 className="section-title">Navigator</h2>
          <p className="empty">Open a Terraform workspace to browse files</p>
        </section>
        <footer className="status">
          {health ? (
            <>
              <span className="dot connected" />
              <span>
                v{health.version}
                <span className="workspace" title={health.workspace}>
                  {health.workspace}
                </span>
              </span>
            </>
          ) : (
            <>
              <span className="dot" />
              <span>disconnected</span>
            </>
          )}
        </footer>
      </aside>

      <main className="strip">
        {PANES.map((pane) => (
          <section className="pane" key={pane.title} style={{ width: pane.width }}>
            <header className="pane-header">{pane.title}</header>
            <div className="pane-body">
              <p className="empty">{pane.empty}</p>
            </div>
          </section>
        ))}
      </main>
    </div>
  )
}
