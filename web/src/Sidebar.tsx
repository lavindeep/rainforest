import type { Health, Preflight, Scan } from './App'

type Props = {
  health: Health | null
  preflight: Preflight | null
  scan: Scan | null
  selected: string
  onSelect: (path: string, line: number) => void
}

function Checks({ preflight }: { preflight: Preflight }) {
  const { terraform, initialized, awsProfile, awsRegion } = preflight
  return (
    <ul className="checks">
      <li>
        {terraform.found ? (
          <>
            <span className="ok">✓</span> Terraform
            {terraform.version && <span className="hint"> v{terraform.version}</span>}
          </>
        ) : (
          <>
            <span className="bad">✗</span> Terraform
            <span className="hint bad"> not found — install terraform</span>
          </>
        )}
      </li>
      <li>
        {initialized ? (
          <>
            <span className="ok">✓</span> Initialized
          </>
        ) : (
          <>
            <span className="bad">✗</span> Initialized
            <span className="hint"> run init from the dashboard — coming soon</span>
          </>
        )}
      </li>
      {awsProfile && <li className="hint">profile {awsProfile}</li>}
      {awsRegion && <li className="hint">region {awsRegion}</li>}
    </ul>
  )
}

function Navigator({ scan, selected, onSelect }: Omit<Props, 'health' | 'preflight'>) {
  if (!scan) {
    return <p className="empty">Open a Terraform workspace to browse files</p>
  }
  if (scan.files.length === 0) {
    return <p className="empty">No Terraform files in this workspace</p>
  }
  return (
    <>
      {scan.diagnostics.length > 0 && (
        <p className="diagnostics">
          {scan.diagnostics.length} parse {scan.diagnostics.length === 1 ? 'issue' : 'issues'}
        </p>
      )}
      <ul className="tree">
        {scan.files.map((file) => (
          <li key={file}>
            <button
              type="button"
              className={file === selected ? 'node current truncate' : 'node truncate'}
              title={file}
              onClick={() => onSelect(file, 0)}
            >
              {file}
            </button>
            <ul className="tree nested">
              {scan.blocks
                .filter((block) => block.file === file)
                .map((block) => (
                  <li key={`${block.file}:${block.line}`}>
                    <button
                      type="button"
                      className="node truncate"
                      title={block.address}
                      onClick={() => onSelect(block.file, block.line)}
                    >
                      <span className={`kind kind-${block.kind}`}>{block.kind}</span>{' '}
                      {block.address}
                    </button>
                  </li>
                ))}
            </ul>
          </li>
        ))}
      </ul>
    </>
  )
}

export default function Sidebar({ health, preflight, scan, selected, onSelect }: Props) {
  return (
    <aside className="sidebar">
      <h1 className="brand">Rain Forest</h1>

      <section className="preflight">
        <h2 className="section-title">Workspace</h2>
        {preflight ? <Checks preflight={preflight} /> : <p className="empty">checking…</p>}
      </section>

      <section className="navigator">
        <h2 className="section-title">Navigator</h2>
        <Navigator scan={scan} selected={selected} onSelect={onSelect} />
      </section>

      <footer className="status">
        {health ? (
          <>
            <span className="dot connected" />
            <span className="status-text">
              v{health.version}
              <span className="workspace truncate" title={health.workspace}>
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
  )
}
