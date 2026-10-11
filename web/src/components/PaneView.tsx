/**
 * The focused pane: a header and the text view. On the phone this IS the
 * screen; on desktop it is the right-hand column.
 *
 * THERE IS NO TERMINAL VIEW. There used to be a `text | term` toggle, with a
 * consent gate in front of the terminal because attaching one could resize the
 * owner's pane for everyone looking at it. The toggle is gone, and so is the
 * whole xterm path behind it.
 *
 * Why it went, rather than staying as an option nobody was forced to use:
 *
 *   - It was not what people came for. Reading what an agent is doing, and
 *     answering it, is the job; a pixel-exact 120x40 grid is the wrong shape
 *     for a phone, where it rendered at about four pixels a character.
 *   - It was the ONLY thing in this app that could change the owner's session
 *     by being looked at. Every mitigation we built — the consent gate, the
 *     geometry notice, the render-health watchdog, "⤺ pane size" — existed to
 *     contain that one capability. Removing the capability removes the need
 *     for all of it, and "looking must not touch" stops being a rule we
 *     enforce and becomes a property of the build.
 *
 * What is lost, stated plainly: you can no longer drive a full-screen TUI
 * (vim, htop, an installer's curses UI) from the browser. You can still read
 * any pane and send it keys and prompts.
 */

import type { PaneId } from '../protocol/types'
import { useStore } from '../store/store'
import { TranscriptView } from './TranscriptView'
import { AgentBadge } from './AgentBadge'
import { formatBytes } from './PaneList'
import { folderName, shortTitle } from './paneSearch'


export function PaneView({
  target,
  onBack,
  showBack,
  showClose,
}: {
  target: PaneId
  onBack?: () => void
  showBack?: boolean
  /** Desktop only: the wordmark is the way home, but a pane you opened wants
      an explicit way to shut it — closing it is also how you stop reading it. */
  showClose?: boolean
}) {
  const pane = useStore((s) => s.panesById[target])
  // Select SCALARS, never the runtime object: that object is replaced on every
  // output frame, so subscribing to it re-renders this whole screen at the
  // pane's output rate.
  const droppedBytes = useStore((s) => s.runtime[target]?.droppedBytes ?? 0)
  const closedReason = useStore((s) => s.runtime[target]?.closed?.reason)
  const isClosed = useStore((s) => s.runtime[target]?.closed !== undefined)

  const head = (
    <header className="paneview-head">
      {showBack ? (
        <button className="iconbtn" onClick={onBack} aria-label="Back to panes">
          ‹
        </button>
      ) : null}
      <div className="paneview-titles">
        <span className="paneview-title" title={pane?.title}>
          {pane ? shortTitle(pane) : target}
        </span>
        {pane ? (
          /* The LAST segment, not a left-truncated path. Every path on this
             machine shares its first ~30 characters, so "…rkspace/agents/site"
             spent the whole line on the part that identifies nothing. The full
             path stays here as the tooltip, where it costs no pixels. */
          <span className="paneview-sub" title={pane.cwd || undefined}>
            {folderName(pane.cwd) || pane.command || ''}
          </span>
        ) : null}
      </div>
      {pane?.agent ? <AgentBadge state={pane.agent.state} /> : null}
      {showClose ? (
        <div className="paneview-tools">
          <button className="iconbtn" onClick={onBack} aria-label="Close this pane" title="Close">
            ✕
          </button>
        </div>
      ) : null}
    </header>
  )

  if (!pane) {
    return (
      <div className="paneview">
        {head}
        <p className="tr-empty" style={{ padding: 16 }}>
          Opening…
        </p>
      </div>
    )
  }

  return (
    <div className="paneview">
      {head}

      {droppedBytes ? (
        <div className="notice notice-gap">
          {formatBytes(droppedBytes)} of output was dropped under load, then repainted.
        </div>
      ) : null}
      {/* The live-pane notice moved INTO TranscriptView, because it is only
          true of one of the two views now. It warns that Herdr may scroll the
          pane under you — which has nothing to do with the conversation view,
          whose text comes from the agent's own transcript file and does not
          move when the pane does. Only the component that knows which view is
          showing can say it honestly. */}
      {isClosed ? (
        <div className="notice notice-dead">
          This pane has closed{closedReason ? `: ${closedReason}` : '.'}
        </div>
      ) : null}

      {/* The transcript renders the blocked-agent detection text and its own
          answer keys, so there is no second copy of either here. */}
      <TranscriptView target={target} />
    </div>
  )
}
