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
 */

import type { TreePane } from '../protocol/types'
import { useElapsed, usePaneTriage } from '../hooks/usePaneTriage'

export function PaneRowSub({ pane, className = '' }: { pane: TreePane; className?: string }) {
  const triage = usePaneTriage(pane)
  const elapsed = useElapsed(triage.kind === 'activity' ? triage.since : null)

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

  // A blank line still occupies its row: fixed height, no reflow when text
  // appears or goes away.
  return (
    <span className={`row-sub ${className}`} title={triage.text || undefined}>
      {triage.text || ' '}
    </span>
  )
}
