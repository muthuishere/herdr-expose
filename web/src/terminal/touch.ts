/**
 * Touch, reimplemented — SPEC B8.
 *
 * xterm's own touch handling is incomplete and its touch scrolling has
 * regressed repeatedly (xtermjs/xterm.js#5377, #5489), so we do not use it.
 *
 * Rules encoded here:
 *  - A tap is replayed through xterm's CORE MOUSE SERVICE, never as a DOM
 *    mousedown. A synthetic mousedown focuses the hidden textarea, and a stray
 *    tap then summons the IME over the terminal.
 *  - A vertical drag scrolls SCROLLBACK on the normal buffer, and synthesizes
 *    wheel (or arrow keys, when the app asked for neither) on the ALTERNATE
 *    screen, where scrollback does not exist.
 *  - A long-press is CONSUMED, not turned into an accidental selection.
 *  - We never setPointerCapture a touch pointer — that disables the browser's
 *    gesture pipeline (and with it, scrolling anywhere else on the page).
 *  - Real mouse input passes straight through, untouched.
 */

import type { Terminal } from '@xterm/xterm'

export interface TouchOptions {
  /** Send synthesized key bytes upstream (arrow keys on the alt screen). */
  sendKeys: (data: string) => void
  /** Wheel-ish input that MAY be dropped under backpressure (SPEC B9). */
  sendScroll: (data: string) => void
  /** Called on long press, so the UI can offer a real selection affordance. */
  onLongPress?: (x: number, y: number) => void
}

interface CoreMouseService {
  areMouseEventsActive?: boolean
  triggerMouseEvent?: (ev: CoreMouseEvent) => boolean
}

interface CoreMouseEvent {
  col: number
  row: number
  x: number
  y: number
  button: number
  action: number
  ctrl: boolean
  alt: boolean
  shift: boolean
}

/** xterm's internal enums, inlined so we do not depend on a private export. */
const CoreMouseButton = { LEFT: 0, WHEEL: 4, NONE: 3 }
const CoreMouseAction = { UP: 0, DOWN: 1, MOVE: 32 }

const LONG_PRESS_MS = 480
const TAP_SLOP_PX = 10
const DRAG_START_PX = 8

export function attachTouch(term: Terminal, el: HTMLElement, opts: TouchOptions): () => void {
  let startX = 0
  let startY = 0
  let lastY = 0
  let active = false
  let dragging = false
  let longPressTimer: number | undefined
  let consumed = false
  /** Fractional line accumulator so slow drags still scroll smoothly. */
  let scrollRemainder = 0

  const core = () =>
    (term as unknown as { _core?: { coreMouseService?: CoreMouseService } })._core
      ?.coreMouseService

  const onAlternateScreen = () =>
    !!(term as unknown as { buffer?: { active?: { type?: string } } }).buffer?.active?.type &&
    term.buffer.active.type === 'alternate'

  const cellSize = () => {
    const dims = (
      term as unknown as {
        _core?: {
          _renderService?: {
            dimensions?: { css?: { cell?: { width: number; height: number } } }
          }
        }
      }
    )._core?._renderService?.dimensions?.css?.cell
    return { w: dims?.width ?? 8, h: dims?.height ?? 17 }
  }

  /** Screen point -> 1-based terminal cell, using UNSCALED CSS metrics (B8). */
  const toCell = (clientX: number, clientY: number) => {
    const rect = el.getBoundingClientRect()
    const { w, h } = cellSize()
    const col = Math.min(term.cols, Math.max(1, Math.floor((clientX - rect.left) / w) + 1))
    const row = Math.min(term.rows, Math.max(1, Math.floor((clientY - rect.top) / h) + 1))
    return { col, row }
  }

  const fireMouse = (clientX: number, clientY: number, action: number, button: number) => {
    const svc = core()
    if (!svc?.triggerMouseEvent) return false
    const { col, row } = toCell(clientX, clientY)
    return (
      svc.triggerMouseEvent({
        col,
        row,
        x: col,
        y: row,
        button,
        action,
        ctrl: false,
        alt: false,
        shift: false,
      }) ?? false
    )
  }

  const onTouchStart = (ev: TouchEvent) => {
    if (ev.touches.length !== 1) {
      // Pinch: let the browser zoom the PAGE. It must not reach the PTY (B8).
      cancel()
      return
    }
    const t = ev.touches[0]
    active = true
    dragging = false
    consumed = false
    scrollRemainder = 0
    startX = lastY = 0
    startX = t.clientX
    startY = t.clientY
    lastY = t.clientY
    longPressTimer = window.setTimeout(() => {
      if (!active || dragging) return
      consumed = true // consumed, NOT a selection
      opts.onLongPress?.(startX, startY)
    }, LONG_PRESS_MS)
  }

  const onTouchMove = (ev: TouchEvent) => {
    if (!active || ev.touches.length !== 1) return
    const t = ev.touches[0]
    const dy = t.clientY - startY
    const dx = t.clientX - startX

    if (!dragging && Math.abs(dy) > DRAG_START_PX && Math.abs(dy) > Math.abs(dx)) {
      dragging = true
      clearTimeout(longPressTimer)
    }
    if (!dragging) return

    // We are handling this gesture; stop the page from scrolling behind it.
    if (ev.cancelable) ev.preventDefault()

    const { h } = cellSize()
    const delta = (lastY - t.clientY) / h + scrollRemainder
    const lines = Math.trunc(delta)
    scrollRemainder = delta - lines
    lastY = t.clientY
    if (lines === 0) return

    const svc = core()
    if (svc?.areMouseEventsActive) {
      // The app wants mouse reports: give it real wheel events.
      const button = CoreMouseButton.WHEEL
      const action = lines > 0 ? CoreMouseAction.DOWN : CoreMouseAction.UP
      for (let i = 0; i < Math.min(Math.abs(lines), 8); i++)
        fireMouse(t.clientX, t.clientY, action, button)
      return
    }
    if (onAlternateScreen()) {
      // No scrollback on the alt screen — synthesize arrows, droppable (B9).
      const key = lines > 0 ? '\x1b[B' : '\x1b[A'
      opts.sendScroll(key.repeat(Math.min(Math.abs(lines), 8)))
      return
    }
    term.scrollLines(lines)
  }

  const onTouchEnd = (ev: TouchEvent) => {
    clearTimeout(longPressTimer)
    if (!active) return
    active = false
    const t = ev.changedTouches[0]
    if (!t || dragging || consumed) {
      dragging = false
      return
    }
    const moved = Math.hypot(t.clientX - startX, t.clientY - startY)
    if (moved > TAP_SLOP_PX) return

    // A TAP. Replay through the core mouse service only — never a DOM
    // mousedown, which would focus the hidden textarea and raise the IME.
    const svc = core()
    if (svc?.areMouseEventsActive) {
      if (ev.cancelable) ev.preventDefault()
      fireMouse(t.clientX, t.clientY, CoreMouseAction.DOWN, CoreMouseButton.LEFT)
      fireMouse(t.clientX, t.clientY, CoreMouseAction.UP, CoreMouseButton.LEFT)
    }
    // When the app is NOT reading the mouse, a tap means nothing to the pty.
    // The key bar is the input surface on touch; we deliberately do not focus.
  }

  const cancel = () => {
    clearTimeout(longPressTimer)
    active = false
    dragging = false
  }

  // Non-passive: we must be able to preventDefault a drag we are consuming.
  el.addEventListener('touchstart', onTouchStart, { passive: true })
  el.addEventListener('touchmove', onTouchMove, { passive: false })
  el.addEventListener('touchend', onTouchEnd, { passive: false })
  el.addEventListener('touchcancel', cancel, { passive: true })

  void CoreMouseAction.MOVE
  void CoreMouseButton.NONE

  return () => {
    cancel()
    el.removeEventListener('touchstart', onTouchStart)
    el.removeEventListener('touchmove', onTouchMove)
    el.removeEventListener('touchend', onTouchEnd)
    el.removeEventListener('touchcancel', cancel)
  }
}
