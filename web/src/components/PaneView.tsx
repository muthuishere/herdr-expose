/**
 * The focused pane: header, then EITHER a transcript or a full terminal, with a
 * fixed key bar under the terminal. On the phone this IS the screen; on desktop
 * it is the right-hand column.
 *
 * SPEC J2 — two views, chosen by what the pane IS:
 *   - a pane with an AGENT opens as a TRANSCRIPT. Reflowed readable text, no
 *     geometry, so opening it on a phone does not SIGWINCH the agent into
 *     throwing its screen away.
 *   - a pane with NO agent opens as a TERMINAL, exactly as before.
 * Either is reachable on any pane from the header toggle, and the choice is
 * remembered per pane.
 */

import { useCallback, useMemo, useState } from 'react'
import type { PaneId } from '../protocol/types'
import { useStore } from '../store/store'
import { LiveTerminal } from './LiveTerminal'
import { TranscriptView } from './TranscriptView'
import { KeyBar } from './KeyBar'
import { AgentQA, QA_QUICK_KEYS } from './AgentQA'
import { AgentBadge } from './AgentBadge'
import { formatBytes } from './PaneList'
import type { RendererKind } from '../terminal/renderer'
import { DEFAULT_FONT_SIZE } from '../terminal/fit'
import {
  hasRealAgent,
  readPaneView,
  writePaneView,
  type PaneViewKind,
} from './paneViewMode'
import type { HealthReading } from '../terminal/renderHealth'

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
  // output frame, so subscribing to it re-renders this whole screen (and the
  // key bar, and the QA panel) at the pane's output rate.
  const droppedBytes = useStore((s) => s.runtime[target]?.droppedBytes ?? 0)
  const closedReason = useStore((s) => s.runtime[target]?.closed?.reason)
  const isClosed = useStore((s) => s.runtime[target]?.closed !== undefined)
  const mode = useStore((s) => s.serverModes[target])
  const [renderer, setRenderer] = useState<RendererKind | null>(null)
  // The render-health watchdog spent its repair budget and the grid is still
  // misaligned. Say so in one line rather than sitting there looking broken.
  const [health, setHealth] = useState<HealthReading | null>(null)
  // Local display size only. It never reaches the PTY (SPEC B8).
  const [fontSize, setFontSize] = useState(DEFAULT_FONT_SIZE)
  const blocked = pane?.agent?.state === 'blocked'
  const hasAgent = hasRealAgent(pane)

  /*
   * The view is resolved DURING RENDER for the target on screen, never in an
   * effect afterwards.
   *
   * This component is reused across targets (clicking another pane changes the
   * prop, it does not remount), so an effect-based reset renders one frame of
   * the PREVIOUS pane's view for the NEW pane. That is not a cosmetic flash: a
   * stale transcript frame declares `viewport: transcript` for the new target,
   * and the terminal's `resize` — which by contract precedes `viewport: live` —
   * then arrives while the server still believes the pane is a transcript. The
   * shell pane simply never went live. A frame of the wrong view is a protocol
   * bug here, not a flicker.
   */
  const [choice, setChoice] = useState<{ target: PaneId; kind: PaneViewKind } | null>(null)
  const stored = useMemo(() => readPaneView(target, hasAgent), [target, hasAgent])
  const view: PaneViewKind = choice && choice.target === target ? choice.kind : stored

  const choose = useCallback(
    (kind: PaneViewKind) => {
      writePaneView(target, kind)
      setChoice({ target, kind })
    },
    [target],
  )

  // Until the tree has told us whether this pane has an agent, render NEITHER
  // view. The default depends on that answer, and guessing "terminal" would
  // attach a PTY at this window's size to an agent pane — the exact
  // destruction transcript mode exists to prevent, done by accident during a
  // single frame of ignorance.
  if (!pane) {
    return (
      <div className="paneview">
        <header className="paneview-head">
          {showBack ? (
            <button className="iconbtn" onClick={onBack} aria-label="Back to panes">
              ‹
            </button>
          ) : null}
          <div className="paneview-titles">
            <span className="paneview-title">{target}</span>
          </div>
        </header>
        <p className="tr-empty" style={{ padding: 16 }}>
          Opening…
        </p>
      </div>
    )
  }

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
          <div className="viewtoggle" role="group" aria-label="View">
            <button
              type="button"
              className={`viewtoggle-btn${view === 'transcript' ? ' is-on' : ''}`}
              aria-pressed={view === 'transcript'}
              title="Readable text. Does not resize the pane."
              onClick={() => choose('transcript')}
            >
              text
            </button>
            <button
              type="button"
              className={`viewtoggle-btn${view === 'terminal' ? ' is-on' : ''}`}
              aria-pressed={view === 'terminal'}
              title="Exact terminal. Attaches at this window's size."
              onClick={() => choose('terminal')}
            >
              term
            </button>
          </div>
          {view === 'terminal' ? (
            <>
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
            </>
          ) : null}
        </div>
      </header>

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
      {view === 'terminal' && health ? (
        <button
          type="button"
          className="notice notice-health"
          onClick={() => choose('transcript')}
        >
          Display out of sync ({health.reasons.join(', ')}) — repairs did not settle it. Tap
          to switch to text, which has no grid to misalign.
        </button>
      ) : null}
      {view === 'terminal' && mode && mode !== 'live' ? (
        <div className="notice notice-mode">
          The server is serving this pane as <b>{mode}</b>, not live.
        </div>
      ) : null}

      {view === 'transcript' ? (
        // The transcript already renders the blocked-agent detection text and
        // its own answer keys, so the Q&A panel and the terminal key bar would
        // be a second copy of both — and the key bar's raw-input path is
        // exactly what must not run while a transcript is open.
        <TranscriptView target={target} />
      ) : (
        <>
          {blocked ? <AgentQA target={target} /> : null}
          <div className="term-wrap">
            <LiveTerminal
              target={target}
              fontSize={fontSize}
              onRenderer={setRenderer}
              onHealth={setHealth}
            />
          </div>
          <KeyBar target={target} quickKeys={blocked ? QA_QUICK_KEYS : undefined} />
          {renderer === 'dom' ? (
            <span className="renderer-note" title="Accelerated rendering was unavailable here">
              software renderer
            </span>
          ) : null}
        </>
      )}
    </div>
  )
}
