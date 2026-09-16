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
