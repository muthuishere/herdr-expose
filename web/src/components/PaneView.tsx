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

/**
 * Keep the TAIL of a long path (the part that identifies the directory) and drop
 * the head. Done in JS, not with `direction: rtl` — RTL reorders the whole run
 * and renders "~/a/b/herdr-expose" as "…b/herdr-expose/~".
 */
function truncateStart(s: string, max: number): string {
  return s.length <= max ? s : '…' + s.slice(s.length - max + 1)
}

export function PaneView({
  target,
  onBack,
  showBack,
}: {
  target: PaneId
  onBack?: () => void
  showBack?: boolean
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
        <span className="paneview-title">{pane?.title ?? target}</span>
        {pane ? (
          <span className="paneview-sub">
            {truncateStart(pane.cwd ?? pane.command ?? '', 34)}
          </span>
        ) : null}
      </div>
      {pane?.agent ? <AgentBadge state={pane.agent.state} /> : null}
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
