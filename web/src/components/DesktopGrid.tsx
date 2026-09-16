/**
 * Desktop-only: a grid of SUMMARY tiles. Every tile is pre-rendered ANSI HTML —
 * never an xterm instance (SPEC §8). Clicking one promotes it to the focused
 * terminal, which is the only xterm on the page.
 */

import { useMemo } from 'react'
import { agentUrgency, type PaneId, type TreePane } from '../protocol/types'
import { useStore } from '../store/store'
import { AnsiBlock } from './SummaryTile'
import { AgentBadge } from './AgentBadge'

export function DesktopGrid({ onOpen }: { onOpen: (id: PaneId) => void }) {
  const tree = useStore((s) => s.tree)
  const runtime = useStore((s) => s.runtime)

  const panes = useMemo(() => {
    const out: TreePane[] = []
    for (const ws of tree.workspaces ?? [])
      for (const tab of ws.tabs ?? []) out.push(...(tab.panes ?? []))
    return out.sort((a, b) => agentUrgency(a.agent?.state) - agentUrgency(b.agent?.state))
  }, [tree])

  if (panes.length === 0) {
    return (
      <div className="empty">
        <p className="empty-title">No panes yet</p>
        <p className="empty-sub">Waiting for the tree from herdr-expose.</p>
      </div>
    )
  }

  return (
    <div className="grid">
      {panes.map((p) => (
        <button
          key={p.id}
          className={`tile${p.agent ? ` tile-${p.agent.state}` : ''}`}
          onClick={() => onOpen(p.id)}
        >
          <span className="tile-head">
            <span className="tile-title">{p.title}</span>
            {p.agent ? <AgentBadge state={p.agent.state} /> : null}
          </span>
          <AnsiBlock text={runtime[p.id]?.text ?? ''} maxLines={10} maxCols={110} />
        </button>
      ))}
    </div>
  )
}
