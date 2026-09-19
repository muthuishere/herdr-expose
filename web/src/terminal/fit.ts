/**
 * Geometry — SPEC B8.
 *
 * Two rules that cost days if you get them wrong:
 *
 *  1. NEVER `transform: scale()` the xterm surface. xterm resolves a cell as
 *     (clientX - screenRect.left) / UNSCALED css cell width and does not
 *     compensate for transforms, so every mouse report, selection and link
 *     hit-test silently offsets. We fit by recomputing cols/rows instead.
 *
 *  2. The cell-height fallback ratio is 1.30, NOT the configured lineHeight
 *     (1.15). xterm ceils the font's line box. Using bare lineHeight yields
 *     ~13% too many rows, and because this estimate seeds the PTY before the
 *     renderer has measured anything, those extra rows push agent output
 *     straight into scrollback. Err high.
 *
 * Also: PTY geometry must stay independent of client-local fontSize, so browser
 * zoom / pinch / Ctrl+± never resizes the remote terminal. See ptyGeometry().
 */

import type { Terminal } from '@xterm/xterm'
import { MIN_COLS, MIN_ROWS } from '../protocol/types'

/** xterm ceils the line box; this is the safe over-estimate. */
export const CELL_HEIGHT_FALLBACK_RATIO = 1.3
export const DEFAULT_FONT_SIZE = 13
export const DEFAULT_LINE_HEIGHT = 1.15
export const FONT_FAMILY =
  "'SFMono-Regular', ui-monospace, 'JetBrains Mono', Menlo, Consolas, 'Liberation Mono', monospace"

export interface CellMetrics {
  width: number
  height: number
  /** true when the numbers came from the live renderer, not the fallback. */
  measured: boolean
}

interface CoreDims {
  css?: { cell?: { width?: number; height?: number } }
  actualCellWidth?: number
  actualCellHeight?: number
}

/**
 * Prefer the renderer's own measurement; fall back to a canvas text measure
 * plus the 1.30 ratio. Both are in CSS pixels, unscaled.
 */
export function cellMetrics(term: Terminal | null, fontSize: number): CellMetrics {
  const core = (term as unknown as { _core?: { _renderService?: { dimensions?: CoreDims } } })
    ?._core?._renderService?.dimensions
  const w = core?.css?.cell?.width ?? core?.actualCellWidth
  const h = core?.css?.cell?.height ?? core?.actualCellHeight
  if (w && h && w > 1 && h > 1) return { width: w, height: h, measured: true }

  return {
    width: measureCharWidth(fontSize),
    height: Math.ceil(fontSize * CELL_HEIGHT_FALLBACK_RATIO),
    measured: false,
  }
}

let measureCanvas: HTMLCanvasElement | null = null
const charWidthCache = new Map<number, number>()

function measureCharWidth(fontSize: number): number {
  const hit = charWidthCache.get(fontSize)
  if (hit) return hit
  let w = fontSize * 0.6
  try {
    measureCanvas ??= document.createElement('canvas')
    const ctx = measureCanvas.getContext('2d')
    if (ctx) {
      ctx.font = `${fontSize}px ${FONT_FAMILY}`
      const m = ctx.measureText('W'.repeat(10))
      if (m.width > 0) w = m.width / 10
    }
  } catch {
    /* fall through to the ratio estimate */
  }
  charWidthCache.set(fontSize, w)
  return w
}

export interface Geometry {
  cols: number
  rows: number
}

/**
 * Cols/rows that fit `box` at the given metrics. Floored at 20x6 (SPEC B2).
 * Rows are floored (never ceiled) so the last line is always fully visible.
 */
export function fitGeometry(
  box: { width: number; height: number },
  metrics: CellMetrics,
): Geometry {
  const cols = Math.max(MIN_COLS, Math.floor(box.width / Math.max(1, metrics.width)))
  const rows = Math.max(MIN_ROWS, Math.floor(box.height / Math.max(1, metrics.height)))
  return { cols, rows }
}

/**
 * The geometry we report UPSTREAM. Deliberately computed from the *reference*
 * font size, not the user's local one: browser zoom, pinch-zoom and a DPR change
 * must never fire a logical resize at the PTY (SPEC B8).
 */
export function ptyGeometry(box: { width: number; height: number }): Geometry {
  return fitGeometry(box, {
    width: measureCharWidth(DEFAULT_FONT_SIZE),
    height: Math.ceil(DEFAULT_FONT_SIZE * CELL_HEIGHT_FALLBACK_RATIO),
    measured: false,
  })
}

export function sameGeometry(a: Geometry | null, b: Geometry): boolean {
  return !!a && a.cols === b.cols && a.rows === b.rows
}

/**
 * SCALE TO THE PANE, DO NOT RESIZE THE PANE.
 *
 * The pane already has a size — the owner's size. Rather than telling herdr to
 * make it ours (which changes it on their laptop, mid-keystroke), we keep the
 * grid at `cols x rows` and pick the largest font size at which that grid fits
 * the box we have. On a phone that font is small; a small terminal is a
 * cosmetic problem, and reflowing somebody's live agent is not.
 *
 * Floored at MIN_FONT_SIZE and capped at MAX_FONT_SIZE: past the floor the grid
 * simply overflows and `.term-wrap` scrolls, which is honest, whereas a 2px
 * font is a blank rectangle that looks like a bug.
 */
export const MIN_FONT_SIZE = 5
export const MAX_FONT_SIZE = 22

export function fontSizeToFit(
  box: { width: number; height: number },
  cols: number,
  rows: number,
): number {
  if (box.width < 2 || box.height < 2 || cols < 1 || rows < 1) return DEFAULT_FONT_SIZE
  let best = MIN_FONT_SIZE
  // Integer sizes only: xterm rounds cell metrics, so a fractional size buys
  // nothing and costs a re-measure.
  for (let size = MAX_FONT_SIZE; size >= MIN_FONT_SIZE; size--) {
    const w = measureCharWidth(size) * cols
    const h = Math.ceil(size * CELL_HEIGHT_FALLBACK_RATIO) * rows
    if (w <= box.width && h <= box.height) {
      best = size
      break
    }
  }
  return best
}
