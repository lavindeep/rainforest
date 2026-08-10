import { useCallback, useEffect, useRef, useState } from 'react'
import type { Preflight } from './App'

type Line = { runId: string; seq: number; stream: string; text: string }
type Counts = { add: number; change: number; destroy: number }
type Done = { runId: string; state: string; exitCode: number; counts: Counts | null; err: string }
type Change = { address: string; type: string; name: string; actions: string[] }

type Summary = {
  runId: string
  state: string
  exitCode: number
  run: { argv: string[]; cwd: string; terraformVersion: string }
  counts: Counts | null
  noChanges: boolean
  showError: string
  changes: Change[]
  err: string
}

type Phase = 'connecting' | 'idle' | 'running' | 'done'

const DEFAULT_ARGV = ['terraform', 'plan', '-input=false', '-no-color', '-out=<planfile>']

const MAX_LINES = 10000

const BADGES: Record<string, { label: string; tone: string }> = {
  create: { label: 'Create', tone: 'create' },
  update: { label: 'Update', tone: 'update' },
  delete: { label: 'Destroy', tone: 'destroy' },
  'create,delete': { label: 'Replace', tone: 'replace' },
}

function badge(actions: string[]) {
  return (
    BADGES[[...actions].sort().join(',')] ?? { label: actions.join('+') || 'No-op', tone: 'other' }
  )
}

function banner(done: Done) {
  if (done.state === 'succeeded') return { tone: 'ok', text: 'Plan succeeded' }
  if (done.state === 'failed') return { tone: 'bad', text: `Plan failed — exit ${done.exitCode}` }
  if (done.state === 'canceled') return { tone: 'off', text: 'Plan canceled' }
  return { tone: 'bad', text: 'Plan errored' }
}

function blockedReason(preflight: Preflight | null) {
  if (!preflight) return 'checking preflight…'
  if (!preflight.terraform.found) return 'terraform not found — install terraform to run a plan'
  if (!preflight.initialized) return 'workspace not initialized — run terraform init first'
  return ''
}

export default function PlanPanel({ preflight }: { preflight: Preflight | null }) {
  const [phase, setPhase] = useState<Phase>('connecting')
  const [lines, setLines] = useState<Line[]>([])
  const [truncated, setTruncated] = useState(false)
  const [done, setDone] = useState<Done | null>(null)
  const [summary, setSummary] = useState<Summary | null>(null)
  const [argv, setArgv] = useState(DEFAULT_ARGV)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [lost, setLost] = useState(false)
  const [cancels, setCancels] = useState(0)

  const source = useRef<EventSource | null>(null)
  const log = useRef<HTMLDivElement>(null)
  const pinned = useRef(true)
  const mounted = useRef(false)
  const activeRun = useRef('')
  const generation = useRef(0)
  const runRevision = useRef(0)
  const connectRef = useRef<(epoch: number) => void>(() => undefined)

  const isCurrent = useCallback(
    (epoch: number) => mounted.current && generation.current === epoch,
    [],
  )

  const closeCurrent = useCallback(() => {
    source.current?.close()
    source.current = null
  }, [])

  const nextGeneration = useCallback(() => ++generation.current, [])

  const resetRunUI = useCallback(() => {
    setLines([])
    setTruncated(false)
    setDone(null)
    setSummary(null)
    setArgv(DEFAULT_ARGV)
    setError('')
    setNotice('')
    setLost(false)
    setCancels(0)
    pinned.current = true
  }, [])

  const adoptRunUI = useCallback(() => {
    runRevision.current++
    resetRunUI()
  }, [resetRunUI])

  const loadSummary = useCallback(
    async (epoch: number, expectedRunId = '') => {
      try {
        const response = await fetch('/api/plan/summary', { credentials: 'same-origin' })
        const body: unknown = await response.json()
        if (!isCurrent(epoch)) return false
        if (expectedRunId !== '' && expectedRunId !== activeRun.current) return false
        if (response.status === 404) {
          activeRun.current = ''
          setNotice('')
          setPhase('idle')
          return false
        }
        if (response.status === 409) {
          const running = body as { runId?: string }
          if (running.runId && activeRun.current !== '' && running.runId !== activeRun.current) {
            adoptRunUI()
          }
          activeRun.current = running.runId ?? ''
          setNotice(
            running.runId
              ? `reconnected to running plan ${running.runId}`
              : 'plan still running — summary not ready yet',
          )
          setPhase('running')
          return true
        }
        if (!response.ok) {
          setError((body as { error?: string }).error ?? `summary failed (${response.status})`)
          return false
        }
        const result = body as Summary
        const changedRun = activeRun.current !== '' && result.runId !== activeRun.current
        if (changedRun) adoptRunUI()
        activeRun.current = result.runId
        setNotice('')
        setSummary(result)
        setDone({
          runId: result.runId,
          state: result.state,
          exitCode: result.exitCode,
          counts: result.state === 'succeeded' ? result.counts : null,
          err: result.err,
        })
        setArgv(result.run.argv)
        setPhase('done')
        return changedRun
      } catch {
        if (!isCurrent(epoch)) return false
        setError('summary request failed — could not reach the server')
        return false
      }
    },
    [adoptRunUI, isCurrent],
  )

  const connect = useCallback(
    (epoch: number) => {
      closeCurrent()
      const events = new EventSource('/api/plan/events')
      source.current = events

      const close = () => {
        events.close()
        if (source.current === events) source.current = null
      }

      events.addEventListener('none', () => {
        if (!isCurrent(epoch) || source.current !== events) return
        setLost(false)
        setNotice('')
        close()
        setPhase('idle')
      })
      events.addEventListener('truncated', (event) => {
        if (!isCurrent(epoch) || source.current !== events) return
        const truncatedRun = JSON.parse((event as MessageEvent<string>).data) as { runId: string }
        const changedRun = activeRun.current !== '' && activeRun.current !== truncatedRun.runId
        if (changedRun) {
          adoptRunUI()
          setPhase('running')
        }
        activeRun.current = truncatedRun.runId
        setLost(false)
        setTruncated(true)
      })
      events.addEventListener('line', (event) => {
        if (!isCurrent(epoch) || source.current !== events) return
        const line = JSON.parse((event as MessageEvent<string>).data) as Line
        const changedRun = activeRun.current !== '' && activeRun.current !== line.runId
        if (changedRun) adoptRunUI()
        activeRun.current = line.runId
        setLost(false)
        setPhase((current) => (changedRun || current !== 'done' ? 'running' : current))
        if (line.seq > MAX_LINES) setTruncated(true)
        setLines((current) =>
          line.seq <= (current.at(-1)?.seq ?? 0) ? current : [...current, line].slice(-MAX_LINES),
        )
      })
      events.addEventListener('done', (event) => {
        if (!isCurrent(epoch) || source.current !== events) return
        const completed = JSON.parse((event as MessageEvent<string>).data) as Done
        if (activeRun.current !== '' && activeRun.current !== completed.runId) adoptRunUI()
        const terminalEpoch = nextGeneration()
        activeRun.current = completed.runId
        setLost(false)
        setNotice('')
        close()
        setDone(completed)
        setPhase('done')
        void (async () => {
          const reconnect = await loadSummary(terminalEpoch, completed.runId)
          if (!isCurrent(terminalEpoch)) return
          if (reconnect) connectRef.current(terminalEpoch)
        })()
      })
      events.onerror = () => {
        if (!isCurrent(epoch) || source.current !== events) return
        setLost(true)
      }
    },
    [adoptRunUI, closeCurrent, isCurrent, loadSummary, nextGeneration],
  )

  useEffect(() => {
    connectRef.current = connect
  }, [connect])

  useEffect(() => {
    mounted.current = true
    const epoch = nextGeneration()
    void (async () => {
      await loadSummary(epoch)
      if (!isCurrent(epoch)) return
      connect(epoch)
    })()
    return () => {
      mounted.current = false
      nextGeneration()
      closeCurrent()
    }
  }, [closeCurrent, connect, isCurrent, loadSummary, nextGeneration])

  useEffect(() => {
    if (pinned.current && log.current) log.current.scrollTop = log.current.scrollHeight
  })

  function onScroll() {
    const element = log.current
    if (element) {
      pinned.current = element.scrollHeight - element.scrollTop - element.clientHeight < 24
    }
  }

  async function start(epoch: number) {
    try {
      const response = await fetch('/api/plan', { method: 'POST', credentials: 'same-origin' })
      const body = (await response.json()) as { runId?: string; argv?: string[]; error?: string }
      if (!isCurrent(epoch)) return
      if (response.status === 202) {
        activeRun.current = body.runId ?? ''
        setNotice('')
        setArgv(body.argv ?? DEFAULT_ARGV)
        connect(epoch)
        return
      }
      if (response.status === 409 && body.runId) {
        activeRun.current = body.runId
        setNotice(`a plan is already running — showing run ${body.runId}`)
        connect(epoch)
        return
      }
      activeRun.current = ''
      setError(body.error ?? `plan failed to start (${response.status})`)
      setPhase('idle')
    } catch {
      if (!isCurrent(epoch)) return
      activeRun.current = ''
      setError('plan request failed — could not reach the server')
      setPhase('idle')
    }
  }

  function run() {
    const epoch = nextGeneration()
    closeCurrent()
    activeRun.current = 'pending'
    resetRunUI()
    setPhase('running')
    void start(epoch)
  }

  async function cancel(epoch: number, revision: number) {
    setCancels((count) => count + 1)
    try {
      const response = await fetch('/api/plan', { method: 'DELETE', credentials: 'same-origin' })
      const body = (await response.json()) as { error?: string }
      if (!isCurrent(epoch) || runRevision.current !== revision) return
      if (!response.ok) {
        setError(body.error ?? `cancel failed (${response.status})`)
      }
    } catch {
      if (!isCurrent(epoch) || runRevision.current !== revision) return
      setError('cancel request failed — could not reach the server')
    }
  }

  const blocked = blockedReason(preflight)
  const running = phase === 'running'
  const succeeded = (summary?.state ?? done?.state) === 'succeeded'
  const counts = succeeded ? (summary?.counts ?? done?.counts ?? null) : null

  return (
    <div className="plan">
      <div className="plan-run">
        <code className="plan-cmd truncate" title={argv.join(' ')}>
          {argv.join(' ')}
        </code>
        {running ? (
          <button
            type="button"
            className="plan-button danger"
            onClick={() => void cancel(generation.current, runRevision.current)}
          >
            {cancels > 0 ? 'Force kill' : 'Cancel'}
          </button>
        ) : (
          <button type="button" className="plan-button" disabled={blocked !== ''} onClick={run}>
            Run
          </button>
        )}
      </div>
      {blocked && !running && <p className="plan-reason">{blocked}</p>}

      {error && <p className="plan-error">{error}</p>}
      {notice && <p className="plan-notice">{notice}</p>}
      {lost && <p className="plan-notice">connection lost — reconnecting…</p>}

      {phase === 'idle' && lines.length === 0 ? (
        <p className="empty">No plan has been run yet.</p>
      ) : (
        <div className="plan-log" ref={log} onScroll={onScroll}>
          {truncated && <div className="plan-line dropped">…earlier output dropped…</div>}
          {lines.map((line) => (
            <div key={line.seq} className={`plan-line ${line.stream}`}>
              {line.text || ' '}
            </div>
          ))}
          {running && lines.length === 0 && <div className="plan-line system">starting…</div>}
        </div>
      )}

      {done && <p className={`plan-banner ${banner(done).tone}`}>{banner(done).text}</p>}

      {done?.err && done.err !== summary?.showError && <p className="plan-error">{done.err}</p>}

      {counts && succeeded && !summary?.noChanges && (
        <div className="plan-chips">
          <span className="chip add">+{counts.add}</span>
          <span className="chip change">~{counts.change}</span>
          <span className="chip destroy">−{counts.destroy}</span>
        </div>
      )}

      {succeeded && summary?.noChanges && (
        <p className="plan-calm">No changes — infrastructure matches the configuration.</p>
      )}

      {summary && summary.showError !== '' && (
        <p className="plan-warn">Could not read the plan file: {summary.showError}</p>
      )}

      {summary && summary.changes.length > 0 && (
        <ul className="plan-changes">
          {summary.changes.map((change) => {
            const { label, tone } = badge(change.actions)
            return (
              <li key={change.address} className={`plan-change ${tone}`}>
                <span className="plan-address truncate" title={change.address}>
                  {change.address}
                </span>
                <span className="plan-type">{change.type}</span>
                <span className={`plan-badge ${tone}`}>{label}</span>
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}
