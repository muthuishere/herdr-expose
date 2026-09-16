/**
 * The focused pane: a full-screen terminal with a fixed key bar under it.
 * On the phone this IS the screen; on desktop it is the right-hand column.
 */

import { useState } from 'react'
import type { PaneId } from '../protocol/types'
import { useStore } from '../store/store'
import { LiveTerminal } from './LiveTerminal'
import { KeyBar } from './KeyBar'
import { AgentQA, QA_QUICK_KEYS } from './AgentQA'
import { AgentBadge } from './AgentBadge'
import { formatBytes } from './PaneList'
import type { RendererKind } from '../terminal/renderer'
import { DEFAULT_FONT_SIZE } from '../terminal/fit'

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
  const runtime = useStore((s) => s.runtime[target])
  const mode = useStore((s) => s.serverModes[target])
  const [renderer, setRenderer] = useState<RendererKind | null>(null)
  // Local display size only. It never reaches the PTY (SPEC B8).
  const [fontSize, setFontSize] = useState(DEFAULT_FONT_SIZE)
  const blocked = pane?.agent?.state === 'blocked'

  return (
    <div className="paneview">
      <header className="paneview-head">
        {showBack ? (
          <button className="iconbtn" onClick={onBack} aria-label="Back to panes">
            ‹
          </button>
        ) : null}
        <div className="paneview-titles">
          <span className="paneview-title">{pane?.title ?? target}</span>
          <span className="paneview-sub">
            {truncateStart(pane?.cwd ?? pane?.command ?? '', 34)}
          </span>
        </div>
        {pane?.agent ? <AgentBadge state={pane.agent.state} /> : null}
        <div className="paneview-tools">
          <button
            className="iconbtn"
            aria-label="Smaller text"
            onClick={() => setFontSize((f) => Math.max(9, f - 1))}
          >
            A−
          </button>
          <button
            className="iconbtn"
            aria-label="Bigger text"
            onClick={() => setFontSize((f) => Math.min(22, f + 1))}
          >
            A+
          </button>
        </div>
      </header>

      {runtime?.droppedBytes ? (
        <div className="notice notice-gap">
          {formatBytes(runtime.droppedBytes)} of output was dropped under load, then repainted.
        </div>
      ) : null}
      {runtime?.closed ? (
        <div className="notice notice-dead">
          This pane has closed{runtime.closed.reason ? `: ${runtime.closed.reason}` : '.'}
        </div>
      ) : null}
      {mode && mode !== 'live' ? (
        <div className="notice notice-mode">
          The server is serving this pane as <b>{mode}</b>, not live.
        </div>
      ) : null}

      {blocked ? <AgentQA target={target} /> : null}

      <div className="term-wrap">
        <LiveTerminal target={target} fontSize={fontSize} onRenderer={setRenderer} />
      </div>

      <KeyBar target={target} quickKeys={blocked ? QA_QUICK_KEYS : undefined} />

      {renderer === 'dom' ? (
        <span className="renderer-note" title="Accelerated rendering was unavailable here">
          software renderer
        </span>
      ) : null}
    </div>
  )
}
