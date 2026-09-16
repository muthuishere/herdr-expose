/**
 * The phone's home screen: workspaces > tabs > panes, single column.
 *
 * Structure is rendered EXACTLY as the server sent it (SPEC §8). The only thing
 * we sort is the order of panes WITHIN a tab, by agent urgency — that is a
 * presentation choice over server-supplied state, not a derived tree.
 */

import { useEffect, useMemo } from 'react'
import { agentUrgency, type PaneId, type TreePane, type ViewportMode } from '../protocol/types'
import { useStore, type PaneRuntime } from '../store/store'
import { declareViewport, releaseViewport } from '../net/viewport'
import { ansiSummaryLine } from '../ansi/ansiToHtml'
import { AgentBadge } from './AgentBadge'

export function PaneList({
  onOpen,
  selected,
}: {
  onOpen: (id: PaneId) => void
  selected?: PaneId | null
}) {
  const tree = useStore((s) => s.tree)
  const runtime = useStore((s) => s.runtime)

  /**
   * SPEC §3: report what we actually RENDER. Rows are 'summary'; anything not
   * listed is 'none'. The SERVER decides the real mode — we never ask for live.
   */
  const rendered = useMemo(() => {
    const ids: PaneId[] = []
    for (const ws of tree.workspaces ?? [])
      for (const tab of ws.tabs ?? []) for (const p of tab.panes ?? []) ids.push(p.id)
    return ids
  }, [tree])

  useEffect(() => {
    const targets: Record<PaneId, ViewportMode> = {}
    // Rows are summaries. The focused pane's `live` comes from the terminal
    // itself; the registry merges the two, live winning.
    for (const id of rendered) targets[id] = 'summary'
    declareViewport('panelist', targets)
    return () => releaseViewport('panelist')
  }, [rendered])

  if ((tree.workspaces ?? []).length === 0) {
    return (
      <div className="empty">
        <p className="empty-title">No panes yet</p>
        <p className="empty-sub">Waiting for the tree from herdr-expose.</p>
      </div>
    )
  }

  return (
    <div className="panelist">
      {tree.workspaces.map((ws) => (
        <section key={ws.id} className="ws">
          <h2 className="ws-title">{ws.title}</h2>
          {ws.tabs.map((tab) => (
            <div key={tab.id} className="tab">
              <h3 className="tab-title">{tab.title}</h3>
              <ul className="rows">
                {[...tab.panes]
                  .sort((a, b) => agentUrgency(a.agent?.state) - agentUrgency(b.agent?.state))
                  .map((p) => (
                    <PaneRow
                      key={p.id}
                      pane={p}
                      runtime={runtime[p.id]}
                      active={p.id === selected}
                      onOpen={onOpen}
                    />
                  ))}
              </ul>
            </div>
          ))}
        </section>
      ))}
    </div>
  )
}

export function PaneRow({
  pane,
  runtime,
  active,
  onOpen,
}: {
  pane: TreePane
  runtime?: PaneRuntime
  active?: boolean
  onOpen: (id: PaneId) => void
}) {
  // The live-ish summary line: the last non-blank line of what the pane printed,
  // falling back to whatever one-liner the server put in the agent summary.
  const line = runtime?.text ? ansiSummaryLine(runtime.text) : ''
  const sub = line || pane.agent?.summary || pane.cwd || ''
  const state = pane.agent?.state

  return (
    <li>
      <button
        className={`row${active ? ' is-active' : ''}${state ? ` row-${state}` : ''}`}
        onClick={() => onOpen(pane.id)}
        aria-current={active ? 'true' : undefined}
      >
        <span className="row-head">
          <span className="row-title">{pane.title}</span>
          {state ? <AgentBadge state={state} /> : null}
        </span>
        <span className="row-sub">{sub || ' '}</span>
        <span className="row-meta">
          {pane.command ? <span className="chip">{pane.command}</span> : null}
          {pane.dead || runtime?.closed ? <span className="chip chip-dead">closed</span> : null}
          {runtime?.droppedBytes ? (
            <span className="chip chip-gap" title="Output dropped under load">
              −{formatBytes(runtime.droppedBytes)}
            </span>
          ) : null}
        </span>
      </button>
    </li>
  )
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n}B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)}KB`
  return `${(n / 1024 / 1024).toFixed(1)}MB`
}
