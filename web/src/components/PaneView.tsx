/**
 * The focused pane: header, then EITHER a transcript or a full terminal, with a
 * fixed key bar under the terminal. On the phone this IS the screen; on desktop
 * it is the right-hand column.
 *
 * LOOKING MUST NOT TOUCH (supersedes J2's "terminal for a pane with no agent"):
 *   - EVERY pane opens as a TRANSCRIPT, agent or plain shell. It declares no
 *     geometry, takes no control and polls a read-only buffer, so opening a
 *     pane from the browser cannot move anything on the owner's laptop.
 *   - The TERMINAL is still one tap away and still exact, but it is now
 *     something you ask for: the first time on a pane we say in one line what
 *     it does, and wait. After that, for this session, we do not ask again.
 * The choice is remembered per pane.
 */

import { useCallback, useMemo, useRef, useState } from 'react'
import type { PaneId } from '../protocol/types'
import { useStore } from '../store/store'
import { LiveTerminal } from './LiveTerminal'
import { TranscriptView } from './TranscriptView'
import { KeyBar } from './KeyBar'
import { AgentQA, QA_QUICK_KEYS } from './AgentQA'
import { AgentBadge } from './AgentBadge'
import { formatBytes } from './PaneList'
import type { RendererKind } from '../terminal/renderer'
import { ptyGeometry } from '../terminal/fit'
import { sendMatchGeometry, sendResize } from '../net/connection'
import {
  grantTerminalConsent,
  hasRealAgent,
  hasTerminalConsent,
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
  // Local display NUDGE only, in font-size steps on top of the size the
  // terminal fits itself to. It never reaches the PTY (SPEC B8).
  const [fontStep, setFontStep] = useState(0)
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
  const requested: PaneViewKind = choice && choice.target === target ? choice.kind : stored

  /*
   * THE CONSENT GATE. A remembered `terminal` preference is not a licence to
   * attach on sight either: consent is per SESSION, so a pane that was in
   * terminal view yesterday still asks once today. Until it is granted the
   * transcript stays on screen, which means the pane is readable the whole
   * time the question is up — there is no blank "are you sure" screen.
   */
  const [asking, setAsking] = useState<PaneId | null>(null)
  const consented = hasTerminalConsent(target)
  const view: PaneViewKind = requested === 'terminal' && !consented ? 'transcript' : requested

  /** What we are actually rendering at, and whether it is the pane's own size. */
  const [geom, setGeom] = useState<{
    cols: number
    rows: number
    source: 'pane' | 'client'
  } | null>(null)

  const choose = useCallback(
    (kind: PaneViewKind) => {
      if (kind === 'terminal' && !hasTerminalConsent(target)) {
        // Do NOT write the preference yet. A tap that was answered "no" must
        // not leave the pane remembering that it wants a terminal.
        setAsking(target)
        return
      }
      setAsking(null)
      writePaneView(target, kind)
      setChoice({ target, kind })
    },
    [target],
  )

  /**
   * The explicit, confirmed mutation. Everything else in this component is
   * read-only by construction; this is the one action that changes the pane
   * for every client attached to it, and it is reversible (⤺ pane size).
   */
  const termWrapRef = useRef<HTMLDivElement>(null)
  const fitToWindow = useCallback(() => {
    const el = termWrapRef.current
    if (!el) return
    const g = ptyGeometry({ width: el.clientWidth, height: el.clientHeight })
    sendResize(target, g.cols, g.rows)
  }, [target])

  const confirmTerminal = useCallback(() => {
    grantTerminalConsent(target)
    setAsking(null)
    writePaneView(target, 'terminal')
    setChoice({ target, kind: 'terminal' })
  }, [target])

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
                onClick={() => setFontStep((f) => Math.max(-8, f - 1))}
              >
                A−
              </button>
              <button
                className="iconbtn"
                aria-label="Bigger text"
                onClick={() => setFontStep((f) => Math.min(9, f + 1))}
              >
                A+
              </button>
              {/* THE ONLY CONTROL IN THIS APP THAT RESIZES SOMEBODY ELSE'S
                  TERMINAL. It is a deliberate, named, reversible action rather
                  than a side effect of opening a pane. */}
              {geom?.source === 'client' ? (
                <button
                  className="iconbtn"
                  aria-label="Give the pane its own size back"
                  title="Stop imposing this window's size on the pane"
                  onClick={() => sendMatchGeometry(target)}
                >
                  ⤺ pane size
                </button>
              ) : (
                <button
                  className="iconbtn"
                  aria-label="Fit the pane to my window"
                  title="Resizes this pane for EVERYONE attached to it, including your laptop"
                  onClick={() => fitToWindow()}
                >
                  ⤢ fit
                </button>
              )}
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
      {asking === target ? (
        // ONE LINE, and it states the real cost rather than a scary one.
        // Measured on herdr 0.9.0: observing a pane does not change its size,
        // but a pane has ONE size shared by everyone attached to it, so
        // "fit to my window" reflows it on the owner's laptop — and typing
        // takes control of the pane they are sitting in front of.
        <div className="notice notice-confirm" role="alertdialog" aria-label="Open the terminal?">
          <span>
            Terminal view attaches to this pane. It renders at the pane&apos;s own size and
            leaves it alone, but typing takes control of it, and <b>Fit to my window</b> would
            resize it for everyone looking — including your laptop. Text view never attaches.
          </span>
          <span className="notice-actions">
            <button type="button" className="key key-sm key-yes" onClick={confirmTerminal}>
              Show terminal
            </button>
            <button type="button" className="key key-sm" onClick={() => setAsking(null)}>
              Stay in text
            </button>
          </span>
        </div>
      ) : null}
      {view === 'terminal' && geom ? (
        <p className="paneview-geom">
          {geom.source === 'pane'
            ? `Rendering at the pane's own size, ${geom.cols}x${geom.rows}. Nothing on your laptop moved.`
            : `You fitted this pane to ${geom.cols}x${geom.rows}. Everyone attached to it, including your laptop, sees that size.`}
        </p>
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
          <div className="term-wrap" ref={termWrapRef}>
            <LiveTerminal
              target={target}
              fontSize={fontStep}
              onRenderer={setRenderer}
              onHealth={setHealth}
              onGeometry={setGeom}
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
