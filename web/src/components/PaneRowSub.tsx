/**
 * The one sub-line of a triage row, and of a summary tile.
 *
 * What it renders is decided by AGENT STATE, never by what happens to be in the
 * output buffer at this millisecond (see hooks/usePaneTriage.ts):
 *
 *   blocked -> the question, in full contrast. The one case where the preview is
 *              the point: it is actionable and it does not move.
 *   working -> no output at all. A calm indicator and time-in-state, ticking
 *              once a second. Ten text replacements a second inform nobody.
 *   else    -> the last line, which on an idle or finished pane is static.
 *
 * THE "else" CASE HAD A HOLE, and a phone screenshot is what found it. On an
 * idle Claude pane the last visible line IS the agent's own summary, which is
 * also what the row renders as its title — so 31 of the 33 rows on this
 * machine read
 *
 *     Claudemod creation
 *     Claudemod creation
 *
 * and spent half of every row saying the same thing twice. When the line would
 * only repeat the name, this shows HOW LONG THE PANE HAS BEEN IDLE instead —
 * which is both new information and the thing the owner acts on: `herdr-expose
 * agents` leads with it and ends with "26 can be closed".
 */

import type { TreePane } from '../protocol/types'
import { toMillis, useElapsed, usePaneTriage } from '../hooks/usePaneTriage'

export function PaneRowSub({
  pane,
  className = '',
  name,
}: {
  pane: TreePane
  className?: string
  /** What the row already shows as its title; never repeated down here. */
  name?: string
}) {
  const triage = usePaneTriage(pane)
  const elapsed = useElapsed(triage.kind === 'activity' ? triage.since : null)
  // Unconditional: a hook cannot hide behind the branch below.
  const settledSince = triage.kind === 'activity' ? null : toMillis(pane.agent?.changed_at)
  const idleFor = useElapsed(settledSince)

  if (triage.kind === 'activity') {
    return (
      <span className={`row-sub row-sub-activity ${className}`}>
        <i className="activity-dot" aria-hidden="true" />
        working{elapsed ? ` · ${elapsed}` : ''}
      </span>
    )
  }

  if (triage.kind === 'question') {
    return (
      <span className={`row-sub row-sub-question ${className}`} title={triage.text}>
        {triage.text}
      </span>
    )
  }

  // Normalised, because "Claudemod creation" and "Claudemod creation " are the
  // same sentence and only one of them should reach the screen.
  const squash = (s: string) => s.trim().toLowerCase().replace(/\s+/g, ' ')
  const repeats = Boolean(name) && squash(triage.text) === squash(name ?? '')

  if ((repeats || !triage.text) && pane.agent) {
    // The duration is OPTIONAL and often absent, by design: the server only
    // dates a transition it actually watched, so a freshly started share
    // knows this pane is idle without knowing since when. "idle" alone is
    // the true subset of "idle 3h40m" — never a zero, never a guess.
    const state = pane.agent.state
    const word = state === 'done' ? 'finished' : state === 'idle' ? 'idle' : state
    return (
      <span className={`row-sub row-sub-quiet ${className}`}>
        {word}
        {idleFor ? ` ${idleFor}` : ''}
      </span>
    )
  }

  // A blank line still occupies its row: fixed height, no reflow when text
  // appears or goes away.
  return (
    <span className={`row-sub ${className}`} title={triage.text || undefined}>
      {repeats ? ' ' : triage.text || ' '}
    </span>
  )
}
