/**
 * The ONE xterm instance. SPEC §8: xterm is for the LIVE pane only — summary
 * tiles are pre-rendered HTML, because N xterm instances is what kills the tab.
 *
 * Wiring, in order, because the order matters:
 *   1. Create the Terminal with the B8 config (scrollback 5000, convertEol,
 *      allowProposedApi, Unicode 11, and deliberately NO theme and NO
 *      minimumContrastRatio — every SGR/OSC colour reaches the screen exactly
 *      as the host sent it).
 *   2. Attach the renderer via probe-and-degrade.
 *   3. Measure, send `resize` BEFORE declaring the viewport (B2: a terminal
 *      without a geometry message does not work).
 *   4. Declare `viewport: live` — the SERVER decides the real mode.
 *   5. Feed raw Uint8Array straight into write(); never a decoded string.
 *   6. Predictive echo on the input path (SPEC F2.1).
 *
 * Frame-cost settings (SPEC F2.3), each one deliberate:
 *   - cursorBlink FALSE — a blinking cursor repaints ~2x/sec forever on an
 *     otherwise idle terminal. The single biggest free win, and the easiest to
 *     leave switched on by accident.
 *   - smoothScrollDuration 0 — animated scrolling is frames spent on nothing.
 *   - NO minimumContrastRatio — a per-cell colour computation on every paint,
 *     and it would also distort the host's colours. Off for both reasons.
 *   - document.fonts.ready is awaited BEFORE construction, so cell metrics are
 *     measured against the real font. Otherwise the whole grid reflows on font
 *     swap, and the 1.30 cell-height estimate is seeded from the wrong face.
 */

import { memo, useCallback, useEffect, useRef, useState } from 'react'
import type { Terminal as XTerm } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

import type { PaneId } from '../protocol/types'
import { onBytes } from '../net/byteBus'
import { countEvent } from '../net/debugStats'
import {
  onReconnected,
  sendInput,
  sendRepaint,
  sendResize,
  sendScrollInput,
  sendSubscribe,
  sendUnsubscribe,
} from '../net/connection'
import { declareViewport, releaseViewport, resendViewport } from '../net/viewport'
import { attachRenderer, type RendererKind } from '../terminal/renderer'
import {
  DEFAULT_FONT_SIZE,
  DEFAULT_LINE_HEIGHT,
  FONT_FAMILY,
  cellMetrics,
  fitGeometry,
  ptyGeometry,
  sameGeometry,
  type Geometry,
} from '../terminal/fit'
import {
  setBaseline,
  startWatchdog,
  type HealthReading,
  type Watchdog,
} from '../terminal/renderHealth'
import { attachTouch } from '../terminal/touch'
import { PredictiveEcho } from '../terminal/predictiveEcho'
import { useElementSize } from '../hooks/useViewport'

export interface LiveTerminalProps {
  target: PaneId
  /** Local-only display size. Never reaches the PTY (B8: zoom must not resize). */
  fontSize?: number
  onRenderer?: (kind: RendererKind) => void
  /**
   * The render-health watchdog could not repair the view within its budget.
   * Surfaced so the pane can say so honestly instead of sitting there looking
   * broken. Called with null when a later repair settled it.
   */
  onHealth?: (reading: HealthReading | null) => void
}

/**
 * FLICKER (measured on an idle pane): Herdr emits periodic `full: true` repaints
 * and the server maps every one of them to a type-2 snapshot — roughly 3 per 10
 * seconds on a pane where nothing is happening. `term.reset()` on each of those
 * blanks the screen and redraws it, several times a second on a busy pane. That
 * IS the flicker, and a reset+full repaint is also the single most expensive
 * thing this renderer can be asked to do.
 *
 * A `full` repaint already carries its own clear/home sequences, so writing it
 * into the live buffer is seamless. We therefore reset ONLY when the buffer
 * genuinely cannot be trusted, tracked by `needsReset`:
 *   - the first snapshot after attach / target change / reconnect, and
 *   - the first snapshot after a `gap`, where bytes were really dropped.
 * Every other snapshot is just bytes.
 */
function LiveTerminalImpl({ target, fontSize, onRenderer, onHealth }: LiveTerminalProps) {
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<XTerm | null>(null)
  const geomRef = useRef<Geometry | null>(null)
  const boxRef = useRef<{ width: number; height: number }>({ width: 0, height: 0 })
  const verifyRef = useRef<(() => void) | null>(null)
  const echoRef = useRef<PredictiveEcho | null>(null)
  /** True while the buffer is untrustworthy; consumed by the next snapshot. */
  const needsResetRef = useRef(true)
  /** Render-health instrumentation, read by the watchdog (never on a hot path). */
  const bytesSeenRef = useRef(0)
  const lastSnapshotAtRef = useRef(0)
  const dogRef = useRef<Watchdog | null>(null)
  const [ready, setReady] = useState(false)

  const localFont = fontSize ?? DEFAULT_FONT_SIZE

  /* --- create ------------------------------------------------------- */
  useEffect(() => {
    let disposed = false
    // A fresh terminal holds nothing: the first snapshot must land on a reset.
    needsResetRef.current = true
    let cleanupTouch: (() => void) | undefined
    let cleanupRenderer: (() => void) | undefined
    let term: XTerm | null = null

    void (async () => {
      const [{ Terminal }, { Unicode11Addon }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/addon-unicode11'),
      ])
      // F2.3: measure cell metrics against the REAL font, never a fallback.
      // A font swap after construction reflows the entire grid.
      await fontsReady()
      if (disposed || !hostRef.current) return

      term = new Terminal({
        scrollback: 5000,
        convertEol: true,
        allowProposedApi: true,
        // F2.3: a blinking cursor is a forever-repaint on an idle terminal.
        cursorBlink: false,
        smoothScrollDuration: 0,
        fontFamily: FONT_FAMILY,
        fontSize: DEFAULT_FONT_SIZE,
        lineHeight: DEFAULT_LINE_HEIGHT,
        // NO theme, NO minimumContrastRatio (B8 fidelity AND F2.3 cost).
        macOptionIsMeta: true,
        // We reimplement touch scrolling; xterm's own has regressed repeatedly.
        scrollOnUserInput: true,
      })
      const uni = new Unicode11Addon()
      term.loadAddon(uni)
      term.unicode.activeVersion = '11'

      term.open(hostRef.current)
      termRef.current = term
      countEvent('termCreate', target)
      setReady(true)

      const attached = await attachRenderer(term)
      if (disposed) {
        attached.dispose()
        return
      }
      cleanupRenderer = () => attached.dispose()
      onRenderer?.(attached.kind)
      // Probe AFTER content is drawn — an empty buffer is a false positive.
      verifyRef.current = () => {
        void attached.verify().then((kind) => onRenderer?.(kind))
      }

      // F2.1 predictive echo. It draws nothing until a prediction has been
      // confirmed, so it can only ever make a correct character appear sooner.
      const echo = new PredictiveEcho(term, hostRef.current)
      echoRef.current = echo
      exposeEchoStats(echo)

      // Keystrokes: encode to bytes, send binary, never dropped (A1 / B9).
      const enc = new TextEncoder()
      term.onData((d) => {
        // Predict BEFORE sending: the local paint must not wait on the socket.
        echo.onInput(d)
        sendInput(target, enc.encode(d))
      })
      term.onBinary((d) => {
        const bytes = new Uint8Array(d.length)
        for (let i = 0; i < d.length; i++) bytes[i] = d.charCodeAt(i) & 0xff
        sendInput(target, bytes)
      })

      cleanupTouch = attachTouch(term, hostRef.current, {
        sendKeys: (s) => sendInput(target, enc.encode(s)),
        sendScroll: (s) => sendScrollInput(target, enc.encode(s)),
      })
    })()

    return () => {
      disposed = true
      cleanupTouch?.()
      cleanupRenderer?.()
      echoRef.current?.dispose()
      echoRef.current = null
      if (termRef.current) countEvent('termDispose', target)
      termRef.current?.dispose()
      termRef.current = null
      setReady(false)
    }
    // Recreating per target is intentional: a fresh pane is a fresh terminal.
  }, [target, onRenderer])

  /* --- local font size: display only, never upstream ---------------- */
  useEffect(() => {
    const t = termRef.current
    if (!t) return
    t.options.fontSize = localFont
  }, [localFont, ready])

  /* --- geometry ----------------------------------------------------- */
  /**
   * Re-measure and re-fit. Returns whether anything ACTUALLY changed — the
   * render-health watchdog escalates on "the refit moved nothing", so a
   * no-op fit must report itself as one rather than as a repair.
   */
  const pushGeometry = useCallback((): boolean => {
    const t = termRef.current
    const box = boxRef.current
    if (!t || box.width < 2 || box.height < 2) return false

    // Local xterm grid: fit the ACTUAL rendered cells so nothing is clipped.
    const metrics = cellMetrics(t, localFont)
    const local = fitGeometry(box, metrics)
    // Upstream PTY grid: reference metrics, so client-local zoom/font never
    // resizes the remote terminal (B8).
    const pty = ptyGeometry(box)

    let changed = false
    if (t.cols !== local.cols || t.rows !== local.rows) {
      t.resize(local.cols, local.rows)
      changed = true
    }
    if (!sameGeometry(geomRef.current, pty)) {
      geomRef.current = pty
      sendResize(target, pty.cols, pty.rows)
      changed = true
    }
    // Re-baseline only from a REAL measurement. Re-baselining from the 1.30
    // fallback would erase the very drift the watchdog exists to see.
    if (metrics.measured) setBaseline(t, metrics)
    return changed
  }, [target, localFont])

  const onResize = useCallback(
    (box: { width: number; height: number }) => {
      boxRef.current = box
      pushGeometry()
      // A container change is one of the known fit invalidators (B8). Check
      // immediately rather than waiting up to a tick to notice.
      dogRef.current?.check()
      // Re-verify the renderer once after the first resize (B8).
      const v = verifyRef.current
      if (v) {
        verifyRef.current = null
        v()
      }
    },
    [pushGeometry],
  )
  useElementSize(hostRef, onResize)

  /* --- subscribe / viewport ----------------------------------------- */
  useEffect(() => {
    if (!ready) return
    const start = () => {
      sendSubscribe([target])
      // B2: geometry BEFORE the first frame is requested.
      const g = geomRef.current ?? ptyGeometry(boxRef.current)
      geomRef.current = g
      sendResize(target, g.cols, g.rows)
      // SPEC §3: declare what we RENDER. The server decides the real mode.
      declareViewport('live', { [target]: 'live' })
    }
    start()
    // B4: no resume. Re-subscribe, take the fresh snapshot, reset, repaint.
    const off = onReconnected(() => {
      // Do NOT blank the screen here: that leaves an empty terminal on display
      // until the new snapshot arrives. Mark it untrusted and let the first
      // snapshot do the reset, so the repaint is a single frame.
      needsResetRef.current = true
      // A new socket is a new world: the repair budget and any accepted-bad
      // reading belong to the old one.
      dogRef.current?.reset()
      // The screen we were predicting against is gone.
      echoRef.current?.wipe('reconnect')
      resendViewport()
      start()
    })
    return () => {
      off()
      releaseViewport('live')
      sendUnsubscribe([target])
    }
  }, [target, ready])

  /* --- render health ------------------------------------------------ */
  /*
   * THE TERMINAL VIEW IS THE ONLY VIEW THAT NEEDS THIS. It is pinned to a
   * character grid measured in CSS pixels, and SPEC B8 is a catalogue of ways
   * that pinning silently comes undone. A transcript declares no geometry and
   * reflows in CSS, so it has nothing to drift — which is the argument for it
   * being the default on an agent pane, not merely a nicer one.
   */
  useEffect(() => {
    if (!ready) return
    const dog = startWatchdog(target, {
      term: () => termRef.current,
      host: () => hostRef.current,
      fontSize: () => localFont,
      declared: () => geomRef.current,
      box: () => boxRef.current,
      bytesSeen: () => bytesSeenRef.current,
      sinceSnapshot: () =>
        lastSnapshotAtRef.current === 0 ? Infinity : Date.now() - lastSnapshotAtRef.current,
      refit: () => pushGeometry(),
      repaint: () => {
        // Reset on the SNAPSHOT, not here: blanking now would leave an empty
        // terminal on display until the server's repaint arrives. Marking the
        // buffer untrusted makes the next snapshot a single-frame repaint,
        // which is the same discipline the reconnect path uses.
        needsResetRef.current = true
        echoRef.current?.wipe('repaint')
        sendRepaint(target)
      },
      onGiveUp: (reading) => onHealth?.(reading),
      onRecovered: () => onHealth?.(null),
    })
    dogRef.current = dog
    return () => {
      dogRef.current = null
      dog.dispose()
      onHealth?.(null)
    }
  }, [target, ready, localFont, pushGeometry, onHealth])

  /**
   * Diagnostic hooks, in the same spirit as __herdrStats / __herdrEcho: SPEC
   * F2.5, measure rather than assert.
   *
   * `__herdrDisturbGrid` puts the emulator at a size the container does not
   * imply — the exact geometry-drift condition the watchdog exists to catch.
   * It is how the acceptance suite proves the repair happens AND that it
   * happens ONCE, which no amount of staring at the screen can establish.
   */
  useEffect(() => {
    const w = window as unknown as {
      __herdrCheckRender?: () => unknown
      __herdrDisturbGrid?: (deltaCols: number) => unknown
    }
    w.__herdrCheckRender = () => dogRef.current?.check(true) ?? null
    w.__herdrDisturbGrid = (deltaCols = 13) => {
      const t = termRef.current
      if (!t) return null
      t.resize(Math.max(2, t.cols + deltaCols), t.rows)
      return { cols: t.cols, rows: t.rows }
    }
    return () => {
      delete w.__herdrCheckRender
      delete w.__herdrDisturbGrid
    }
  }, [])

  /* --- bytes -------------------------------------------------------- */
  useEffect(() => {
    if (!ready) return
    return onBytes(target, (bytes, kind) => {
      const t = termRef.current
      if (!t) return
      bytesSeenRef.current += bytes.length
      if (kind === 'gap') {
        // Bytes were genuinely lost: what is on screen no longer matches the
        // host. The next snapshot repaints from a clean buffer.
        needsResetRef.current = true
        echoRef.current?.wipe('gap')
        return
      }
      if (kind === 'snapshot') {
        lastSnapshotAtRef.current = Date.now()
        // Only the untrusted-buffer case resets. A routine `full` repaint is
        // written straight in — it clears and homes itself.
        if (needsResetRef.current) {
          needsResetRef.current = false
          countEvent('reset', target)
          t.reset()
        }
        echoRef.current?.wipe('snapshot')
      }
      // Raw Uint8Array: faster, and avoids decoding UTF-8 twice.
      // Reconcile in write()'s callback, once the buffer has settled.
      t.write(bytes, () => echoRef.current?.reconcile())
    })
  }, [target, ready])

  return <div className="term-host" ref={hostRef} />
}

/**
 * Memoised on the props that actually matter. A re-render of the parent must
 * never be able to dispose and recreate the xterm instance — a remount looks
 * exactly like flicker and throws away the scrollback with it.
 */
export const LiveTerminal = memo(LiveTerminalImpl)
LiveTerminal.displayName = 'LiveTerminal'

/**
 * Wait for the terminal font, but never block the terminal forever on it — a
 * blocked font load must not mean a blank pane.
 */
function fontsReady(timeoutMs = 1500): Promise<void> {
  const ready = document.fonts?.ready
  if (!ready) return Promise.resolve()
  return Promise.race([
    ready.then(() => undefined),
    new Promise<void>((r) => setTimeout(r, timeoutMs)),
  ])
}

/**
 * SPEC F2.5: measure, do not assert. The live echo-latency histogram is read
 * from the console or the harness as `__herdrEcho()`.
 */
function exposeEchoStats(echo: PredictiveEcho) {
  const w = window as unknown as {
    __herdrEcho?: () => unknown
    __herdrEchoOff?: () => void
    __herdrEchoOn?: () => void
  }
  w.__herdrEcho = () => echo.stats()
  // A/B switch for the latency harness — predictive echo has to be MEASURED
  // against itself being off, not asserted.
  w.__herdrEchoOff = () => echo.setEnabled(false)
  w.__herdrEchoOn = () => echo.setEnabled(true)
}

/** Focus the hidden textarea deliberately — only from an explicit user action. */
export function focusTerminal(term: XTerm | null) {
  term?.focus()
}
