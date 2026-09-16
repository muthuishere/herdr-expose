/**
 * Predictive (local) echo — SPEC F2.1, promoted from B10 to v1.
 *
 * Paint time is ~1ms; tunnel RTT is 30-150ms. Local echo is the only thing that
 * removes that from perception.
 *
 * MOSH SEMANTICS, EXACTLY:
 *   - A prediction starts TENTATIVE and INVISIBLE. Nothing is drawn.
 *   - Once one prediction is confirmed by real server output, the epoch becomes
 *     CONFIDENT and predictions render immediately from then on.
 *   - Any single mismatch wipes every pending prediction and drops back to
 *     TENTATIVE.
 *   => The user is never shown a phantom character. The worst case is that a
 *      correctly-predicted character appears at network speed instead of
 *      instantly, which is exactly what we have today.
 *
 * WHY AN OVERLAY, NOT A BUFFER WRITE:
 * xterm has no undo. Writing a prediction into the buffer and being wrong would
 * corrupt the screen, and repairing it means diffing against a snapshot. So
 * predictions are drawn in an absolutely-positioned layer ON TOP of the
 * terminal, keyed to a cell. The xterm buffer only ever contains bytes the
 * server actually sent, so a wrong prediction can do no worse than remove an
 * overlay glyph.
 *
 * WHEN WE DO NOT PREDICT AT ALL — honesty beats coverage:
 *   - the ALTERNATE screen is active (vim, an agent TUI): a keystroke there does
 *     not echo at the cursor, so any prediction would be a guess;
 *   - the application is reading the mouse / is in an app-keypad mode;
 *   - the input is not a lone printable character (control bytes, escape
 *     sequences, paste with newlines);
 *   - the pane is not the focused live one.
 */

import type { Terminal } from '@xterm/xterm'
import { cellMetrics } from './fit'

type Epoch = 'tentative' | 'confident'

interface Prediction {
  ch: string
  /** Absolute buffer coordinates, so scrollback movement is detectable. */
  row: number
  col: number
  sentAt: number
}

/** A prediction unconfirmed for this long is abandoned; the link is not healthy. */
const PREDICTION_TTL_MS = 1200
/** Keep the echo-latency sample bounded; we only report p50/p95. */
const SAMPLE_CAP = 400

export interface EchoStats {
  /** Confirmed predictions (a real keystroke->echo measurement each). */
  count: number
  p50: number
  p95: number
  /** Predictions contradicted by server output. */
  mismatches: number
  /** Predictions that timed out with no server output at all. */
  expired: number
  epoch: Epoch
}

export class PredictiveEcho {
  private term: Terminal
  private layer: HTMLDivElement
  private pending: Prediction[] = []
  private epoch: Epoch = 'tentative'
  private samples: number[] = []
  private mismatches = 0
  private expired = 0
  private enabled = true
  private raf = 0

  constructor(term: Terminal, host: HTMLElement) {
    this.term = term
    this.layer = document.createElement('div')
    this.layer.className = 'echo-layer'
    this.layer.setAttribute('aria-hidden', 'true')
    host.appendChild(this.layer)
  }

  setEnabled(on: boolean) {
    this.enabled = on
    if (!on) this.wipe('disabled')
  }

  dispose() {
    if (this.raf) cancelAnimationFrame(this.raf)
    this.layer.remove()
    this.pending = []
  }

  /* ---------------------------------------------------------------- */
  /* Input side                                                        */
  /* ---------------------------------------------------------------- */

  /**
   * Called with the exact data about to go upstream. Returns nothing: the send
   * happens regardless. This only decides what to draw locally.
   */
  onInput(data: string): void {
    if (!this.enabled) return

    // Backspace over our own pending prediction is safe and feels excellent:
    // we put that glyph there, so we can take it back without guessing.
    if ((data === '\x7f' || data === '\b') && this.pending.length > 0) {
      this.pending.pop()
      this.render()
      return
    }

    if (!this.isPredictable(data)) {
      // Anything we cannot model invalidates the cursor position we were
      // extrapolating from. Stop predicting rather than guess.
      if (this.pending.length > 0) this.wipe('unpredictable input')
      return
    }

    const at = this.nextCell()
    if (!at) return
    this.pending.push({ ch: data, row: at.row, col: at.col, sentAt: performance.now() })
    this.render()
  }

  /**
   * A single printable character. Deliberately narrow: no control bytes, no
   * escape sequences, no multi-character pastes, no newlines.
   */
  private isPredictable(data: string): boolean {
    if (this.onAlternateScreen()) return false
    if (this.mouseActive()) return false
    if ([...data].length !== 1) return false
    const cp = data.codePointAt(0)!
    if (cp < 0x20 || cp === 0x7f) return false
    // Wide characters occupy two cells; our overlay assumes one.
    if (cp > 0x1100 && this.isWide(cp)) return false
    return true
  }

  /** Where the next predicted glyph lands: after the last one, else at the cursor. */
  private nextCell(): { row: number; col: number } | null {
    const buf = this.term.buffer.active
    const last = this.pending[this.pending.length - 1]
    if (last) {
      // Do not predict a line wrap; column arithmetic past the edge is a guess.
      if (last.col + 1 >= this.term.cols) return null
      return { row: last.row, col: last.col + 1 }
    }
    const col = buf.cursorX
    if (col >= this.term.cols) return null
    return { row: buf.baseY + buf.cursorY, col }
  }

  /* ---------------------------------------------------------------- */
  /* Output side                                                       */
  /* ---------------------------------------------------------------- */

  /**
   * Call AFTER server bytes have been written to the terminal (from write()'s
   * completion callback, so the buffer is settled).
   */
  reconcile(): void {
    if (this.pending.length === 0) {
      this.render()
      return
    }
    const buf = this.term.buffer.active
    const now = performance.now()

    while (this.pending.length > 0) {
      const p = this.pending[0]

      // The row scrolled out of the buffer: we can no longer verify it.
      const line = buf.getLine(p.row)
      if (!line) {
        this.wipe('row gone')
        return
      }

      const cell = line.getCell(p.col)
      const chars = cell?.getChars() ?? ''

      if (chars === '') {
        // Server output has not reached this cell yet. Later predictions cannot
        // be confirmed before an earlier one, so stop here.
        if (now - p.sentAt > PREDICTION_TTL_MS) {
          this.expired++
          this.wipe('timeout')
        }
        break
      }

      if (chars === p.ch) {
        // CONFIRMED. This is a real keystroke->echo measurement.
        this.samples.push(now - p.sentAt)
        if (this.samples.length > SAMPLE_CAP) this.samples.shift()
        this.pending.shift()
        // First confirmation promotes the epoch: predictions now render.
        if (this.epoch === 'tentative') this.epoch = 'confident'
        continue
      }

      // MISMATCH. One is enough: wipe everything, drop to tentative.
      this.mismatches++
      this.wipe('mismatch')
      return
    }
    this.render()
  }

  /** Server said something we did not predict, or the pane reset. Start over. */
  wipe(_reason: string): void {
    if (this.pending.length > 0 || this.epoch !== 'tentative') {
      this.pending = []
      this.epoch = 'tentative'
      this.render()
    }
  }

  stats(): EchoStats {
    const s = [...this.samples].sort((a, b) => a - b)
    const at = (q: number) => (s.length === 0 ? 0 : s[Math.min(s.length - 1, Math.floor(s.length * q))])
    return {
      count: s.length,
      p50: Math.round(at(0.5) * 10) / 10,
      p95: Math.round(at(0.95) * 10) / 10,
      mismatches: this.mismatches,
      expired: this.expired,
      epoch: this.epoch,
    }
  }

  /* ---------------------------------------------------------------- */
  /* Rendering                                                         */
  /* ---------------------------------------------------------------- */

  private render(): void {
    if (this.raf) return
    this.raf = requestAnimationFrame(() => {
      this.raf = 0
      this.paint()
    })
  }

  private paint(): void {
    // TENTATIVE means INVISIBLE. This is the whole safety property.
    if (this.epoch !== 'confident' || this.pending.length === 0) {
      if (this.layer.childNodes.length) this.layer.replaceChildren()
      return
    }

    const screen = this.term.element?.querySelector('.xterm-screen') as HTMLElement | null
    const host = this.layer.parentElement
    if (!screen || !host) return

    const m = cellMetrics(this.term, this.term.options.fontSize ?? 13)
    const sRect = screen.getBoundingClientRect()
    const hRect = host.getBoundingClientRect()
    const originX = sRect.left - hRect.left
    const originY = sRect.top - hRect.top
    const baseY = this.term.buffer.active.baseY

    const frag = document.createDocumentFragment()
    for (const p of this.pending) {
      const viewRow = p.row - baseY
      if (viewRow < 0 || viewRow >= this.term.rows) continue
      const el = document.createElement('span')
      el.className = 'echo-char'
      el.textContent = p.ch
      el.style.left = `${originX + p.col * m.width}px`
      el.style.top = `${originY + viewRow * m.height}px`
      el.style.width = `${m.width}px`
      el.style.height = `${m.height}px`
      el.style.lineHeight = `${m.height}px`
      frag.appendChild(el)
    }
    this.layer.replaceChildren(frag)
  }

  /* ---------------------------------------------------------------- */

  private onAlternateScreen(): boolean {
    return this.term.buffer.active.type === 'alternate'
  }

  private mouseActive(): boolean {
    return !!(
      this.term as unknown as {
        _core?: { coreMouseService?: { areMouseEventsActive?: boolean } }
      }
    )._core?.coreMouseService?.areMouseEventsActive
  }

  /** Cheap East-Asian-wide check; we simply decline to predict these. */
  private isWide(cp: number): boolean {
    return (
      (cp >= 0x1100 && cp <= 0x115f) ||
      (cp >= 0x2e80 && cp <= 0xa4cf) ||
      (cp >= 0xac00 && cp <= 0xd7a3) ||
      (cp >= 0xf900 && cp <= 0xfaff) ||
      (cp >= 0xfe30 && cp <= 0xfe6f) ||
      (cp >= 0xff00 && cp <= 0xff60) ||
      (cp >= 0xffe0 && cp <= 0xffe6) ||
      (cp >= 0x1f300 && cp <= 0x1f64f)
    )
  }
}
