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
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import type { Terminal as XTerm } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

import type { PaneId } from '../protocol/types'
import { onBytes } from '../net/byteBus'
import {
  onReconnected,
  sendInput,
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
import { attachTouch } from '../terminal/touch'
import { useElementSize } from '../hooks/useViewport'

export interface LiveTerminalProps {
  target: PaneId
  /** Local-only display size. Never reaches the PTY (B8: zoom must not resize). */
  fontSize?: number
  onRenderer?: (kind: RendererKind) => void
}

export function LiveTerminal({ target, fontSize, onRenderer }: LiveTerminalProps) {
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<XTerm | null>(null)
  const geomRef = useRef<Geometry | null>(null)
  const boxRef = useRef<{ width: number; height: number }>({ width: 0, height: 0 })
  const verifyRef = useRef<(() => void) | null>(null)
  const [ready, setReady] = useState(false)

  const localFont = fontSize ?? DEFAULT_FONT_SIZE

  /* --- create ------------------------------------------------------- */
  useEffect(() => {
    let disposed = false
    let cleanupTouch: (() => void) | undefined
    let cleanupRenderer: (() => void) | undefined
    let term: XTerm | null = null

    void (async () => {
      const [{ Terminal }, { Unicode11Addon }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/addon-unicode11'),
      ])
      if (disposed || !hostRef.current) return

      term = new Terminal({
        scrollback: 5000,
        convertEol: true,
        allowProposedApi: true,
        cursorBlink: true,
        fontFamily: FONT_FAMILY,
        fontSize: DEFAULT_FONT_SIZE,
        lineHeight: DEFAULT_LINE_HEIGHT,
        // NO theme, NO minimumContrastRatio. SPEC B8: pass host colours through.
        macOptionIsMeta: true,
        // We reimplement touch scrolling; xterm's own has regressed repeatedly.
        scrollOnUserInput: true,
      })
      const uni = new Unicode11Addon()
      term.loadAddon(uni)
      term.unicode.activeVersion = '11'

      term.open(hostRef.current)
      termRef.current = term
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

      // Keystrokes: encode to bytes, send binary, never dropped (A1 / B9).
      const enc = new TextEncoder()
      term.onData((d) => sendInput(target, enc.encode(d)))
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
  const pushGeometry = useCallback(() => {
    const t = termRef.current
    const box = boxRef.current
    if (!t || box.width < 2 || box.height < 2) return

    // Local xterm grid: fit the ACTUAL rendered cells so nothing is clipped.
    const local = fitGeometry(box, cellMetrics(t, localFont))
    // Upstream PTY grid: reference metrics, so client-local zoom/font never
    // resizes the remote terminal (B8).
    const pty = ptyGeometry(box)

    if (t.cols !== local.cols || t.rows !== local.rows) t.resize(local.cols, local.rows)
    if (!sameGeometry(geomRef.current, pty)) {
      geomRef.current = pty
      sendResize(target, pty.cols, pty.rows)
    }
  }, [target, localFont])

  const onResize = useCallback(
    (box: { width: number; height: number }) => {
      boxRef.current = box
      pushGeometry()
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
      termRef.current?.reset()
      resendViewport()
      start()
    })
    return () => {
      off()
      releaseViewport('live')
      sendUnsubscribe([target])
    }
  }, [target, ready])

  /* --- bytes -------------------------------------------------------- */
  useEffect(() => {
    if (!ready) return
    return onBytes(target, (bytes, kind) => {
      const t = termRef.current
      if (!t) return
      // A snapshot is a full repaint, not a delta.
      if (kind === 'snapshot') t.reset()
      // Raw Uint8Array: faster, and avoids decoding UTF-8 twice.
      t.write(bytes)
    })
  }, [target, ready])

  return <div className="term-host" ref={hostRef} />
}

/** Focus the hidden textarea deliberately — only from an explicit user action. */
export function focusTerminal(term: XTerm | null) {
  term?.focus()
}
