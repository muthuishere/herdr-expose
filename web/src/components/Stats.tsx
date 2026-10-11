/**
 * The at-a-glance line: how many panes are in each state, right now.
 *
 * The grid already shows every pane, but counting tiles is work, and the one
 * question someone opens this app to answer is "is anything waiting on me?".
 * That number goes first and is the only one that ever shouts.
 *
 * Counts come from the SAME tree the grid renders, so they cannot disagree with
 * what is on screen — no second source, no derived cache to go stale.
 *
 * A zero is omitted rather than shown as "0 blocked". A row of zeroes reads as
 * a dashboard with nothing in it; the states that exist are the interesting
 * ones, and "needs you" is the only count worth pinning even at zero, because
 * its ABSENCE is the reassuring signal people come here for.
 */

import { useMemo } from 'react'
import type { AgentState } from '../protocol/types'
import { collectPanes } from '../protocol/tree'
import { useStore } from '../store/store'

/**
 * `shell` is not an agent state — it is this component's own word for "a pane
 * with no agent bound", which Herdr reports as `unknown`. The filter speaks
 * Herdr's language, so the translation happens here, at the only place that
 * ever invented the other word.
 */
const FILTER_STATE: Record<string, AgentState> = {
  blocked: 'blocked',
  working: 'working',
  idle: 'idle',
  done: 'done',
  shell: 'unknown',
}

export function Stats({
  onPick,
  active,
}: {
  onPick?: (state: AgentState) => void
  /** The state currently filtered on, so the chip can show it is engaged. */
  active?: AgentState | null
}) {
  const tree = useStore((s) => s.tree)

  const counts = useMemo(() => {
    const panes = collectPanes(tree)
    const by: Record<string, number> = {}
    let agents = 0
    for (const p of panes) {
      const st = p.agent?.state
      // `unknown` is Herdr's answer for a plain shell, not an agent state.
      if (st && st !== 'unknown') {
        agents++
        by[st] = (by[st] ?? 0) + 1
      } else {
        by.shell = (by.shell ?? 0) + 1
      }
    }
    return { by, agents, panes: panes.length, sessions: tree?.sessions?.length ?? 0 }
  }, [tree])

  if (counts.panes === 0) return null

  const blocked = counts.by.blocked ?? 0
  // Order is deliberate: urgency first, then activity, then the rest.
  const rest: Array<[string, string]> = [
    ['working', 'working'],
    ['idle', 'idle'],
    ['done', 'done'],
    ['shell', 'shell'],
  ]

  return (
    <div className="stats" role="status" aria-label="What is happening right now">
      <button
        type="button"
        className={`stat stat-blocked${blocked > 0 ? ' is-on' : ''}${
          active === 'blocked' ? ' is-active' : ''
        }`}
        onClick={() => onPick?.('blocked')}
        disabled={blocked === 0}
        aria-pressed={active === 'blocked'}
      >
        <b>{blocked}</b>
        <span>needs you</span>
      </button>
      {rest.map(([key, label]) =>
        counts.by[key] ? (
          <button
            type="button"
            key={key}
            className={`stat stat-${key}${
              active === FILTER_STATE[key] ? ' is-active' : ''
            }`}
            onClick={() => onPick?.(FILTER_STATE[key])}
            aria-pressed={active === FILTER_STATE[key]}
          >
            <b>{counts.by[key]}</b>
            <span>{label}</span>
          </button>
        ) : null,
      )}
      <span className="stat stat-quiet">
        <b>{counts.panes}</b>
        <span>
          {counts.panes === 1 ? 'pane' : 'panes'}
          {counts.sessions > 1 ? ` in ${counts.sessions} sessions` : ''}
        </span>
      </span>
    </div>
  )
}
