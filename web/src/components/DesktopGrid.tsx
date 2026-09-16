/**
 * Desktop-only: a grid of SUMMARY tiles. Every tile is pre-rendered HTML —
 * never an xterm instance (SPEC §8). Clicking one promotes it to the focused
 * terminal, which is the only xterm on the page.
 *
 * Same rule as the phone list: a tile is for TRIAGE. Twenty tiles each scrolling
 * a live tail is exactly what SUMMARY mode exists to avoid — so a working pane
 * shows an indicator and its time in state, a blocked pane shows its question,
 * and only a settled pane shows text, sampled slowly and repainted only when it
 * genuinely changed.
 */

import { useMemo } from 'react'
import { agentUrgency, type PaneId, type TreePane } from '../protocol/types'
import { collectPanes } from '../protocol/tree'
import { useStore } from '../store/store'
import { AnsiBlock } from './SummaryTile'
import { AgentBadge } from './AgentBadge'
import { EmptyPanes } from './EmptyPanes'
import { useElapsed, usePaneTriage, useQuietPaneText } from '../hooks/usePaneTriage'

export function DesktopGrid({ onOpen }: { onOpen: (id: PaneId) => void }) {
  const tree = useStore((s) => s.tree)

  // Urgency order, then the server's order. Stable: a tile moves only when its
  // state changed, never because something repainted.
  const panes = useMemo(
    () =>
      collectPanes(tree).sort(
        (a, b) => agentUrgency(a.agent?.state) - agentUrgency(b.agent?.state),
      ),
    [tree],
  )

  if (panes.length === 0) return <EmptyPanes />

  return (
    <div className="grid">
      {panes.map((p) => (
        <Tile key={p.id} pane={p} onOpen={onOpen} />
      ))}
    </div>
  )
}

function Tile({ pane, onOpen }: { pane: TreePane; onOpen: (id: PaneId) => void }) {
  const triage = usePaneTriage(pane)
  const elapsed = useElapsed(triage.kind === 'activity' ? triage.since : null)
  // Body text only for a settled pane; a working pane gets none by design.
  const quiet = useQuietPaneText(pane.id, triage.kind === 'line' || triage.kind === 'none')

  return (
    <button
      className={`tile${pane.agent ? ` tile-${pane.agent.state}` : ''}`}
      onClick={() => onOpen(pane.id)}
    >
      <span className="tile-head">
        <span className="tile-title">{pane.title}</span>
        {pane.agent ? <AgentBadge state={pane.agent.state} /> : null}
      </span>

      {triage.kind === 'question' ? (
        <span className="tile-question">{triage.text}</span>
      ) : triage.kind === 'activity' ? (
        <span className="tile-activity">
          <i className="activity-dot" aria-hidden="true" />
          working{elapsed ? ` · ${elapsed}` : ''}
        </span>
      ) : (
        <AnsiBlock text={quiet} maxLines={10} maxCols={110} />
      )}
    </button>
  )
}
