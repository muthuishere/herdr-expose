/**
 * RENDER-HEALTH WATCHDOG — the client detects that it is rendering wrong, and
 * repairs itself. The owner is never the misalignment detector.
 *
 * WHY THIS EXISTS, AND WHY TRANSCRIPT MODE IS EXEMPT.
 * The terminal view is pinned to a character grid: cols x rows, with every
 * column resolved from a measured CSS cell width. Everything in SPEC B8 is a
 * way that pinning silently comes undone — a webfont that lands after the grid
 * was sized (the 1.30 fallback ratio only holds until the real font is
 * measured), browser zoom, a DPR change, a rotation, a container that resized
 * without anyone recomputing the fit. None of it throws. It just renders wrong,
 * forever, and looks like the product is broken.
 *
 * A TRANSCRIPT has none of this by construction: it declares no geometry, holds
 * no grid, and reflows to the viewport in CSS. There is nothing to drift out of
 * alignment, which is precisely why transcript — not a better mirror — is the
 * right default for an agent pane. This file is the cost of the other view.
 *
 * GUARDRAILS. We spent three rounds killing flicker and a thrashing watchdog
 * would hand it all back:
 *   - It acts only on a MEASURED mismatch, never on a timer alone.
 *   - At most MAX_REPAIRS_PER_MIN repairs per target, with exponential backoff.
 *   - A repair that changes nothing measurable ACCEPTS the reading and stops,
 *     until the measurement itself changes. No "detect, repair, detect the same
 *     thing, repair" loop can start.
 *   - Silent while `document.hidden`, or while the pane is not the live one.
 *   - Every detection, repair, failure and reason is counted into
 *     `window.__herdrStats()`, because that is what cracked the last flicker:
 *     we read the counters instead of guessing.
 */

import type { Terminal } from '@xterm/xterm'
import { cellMetrics, fitGeometry, ptyGeometry, type CellMetrics, type Geometry } from './fit'
import { countHealth, recordHealth } from '../net/debugStats'

/** How often we re-check while the terminal is visible and live. */
export const CHECK_INTERVAL_MS = 2000
/** Hard ceiling on repairs per target per rolling minute. */
export const MAX_REPAIRS_PER_MIN = 3
/** First backoff between repair attempts; doubles each attempt. */
const BACKOFF_BASE_MS = 2000
const BACKOFF_MAX_MS = 20_000
/** Cell metrics closer than this are the same measurement, not drift. */
const METRIC_EPSILON_PX = 0.5
/** Below this many received bytes a blank screen is simply a new attach. */
const VOID_MIN_BYTES = 4096
/** A snapshot younger than this may still be painting; do not judge it. */
const SETTLE_MS = 1200

export type HealthReason =
  | 'geometry-drift'
  | 'cell-metric-drift'
  | 'container-mismatch'
  | 'void'
  | 'wrap-damage'

export interface HealthReading {
  reasons: HealthReason[]
  /** What the container can actually fit right now. */
  fit: Geometry
  /** What the emulator currently is. */
  actual: Geometry
  /** What we last told the server. */
  declared: Geometry | null
  metrics: CellMetrics
}

export interface HealthHooks {
  term: () => Terminal | null
  host: () => HTMLElement | null
  /** Local display font size; PTY geometry never uses it (B8). */
  fontSize: () => number
  /** The last PTY geometry we sent the server, or null. */
  declared: () => Geometry | null
  /** Measured container box. */
  box: () => { width: number; height: number }
  /** Bytes received for this target since attach. */
  bytesSeen: () => number
  /** Ms since the last snapshot landed, or Infinity. */
  sinceSnapshot: () => number
  /** Re-measure and re-fit; returns true if anything actually changed. */
  refit: () => boolean
  /** Reset the emulator and ask the server for a full repaint. */
  repaint: () => void
  /** Called when the retry budget is spent and the view is still wrong. */
  onGiveUp: (reading: HealthReading) => void
  /** Called when a repair settled it, so a notice can be cleared. */
  onRecovered: () => void
}

/**
 * A stable description of "the problem I am currently looking at". Two readings
 * with the same signature are the same problem; a repair that leaves the
 * signature untouched achieved nothing and must not be retried on a loop.
 */
function signature(r: HealthReading): string {
  return [
    r.reasons.slice().sort().join(','),
    `${r.actual.cols}x${r.actual.rows}`,
    `${r.fit.cols}x${r.fit.rows}`,
    r.declared ? `${r.declared.cols}x${r.declared.rows}` : '-',
    r.metrics.width.toFixed(2),
    r.metrics.height.toFixed(2),
  ].join('|')
}

/** Inspect the buffer. Returns the reasons that are TRUE of it right now. */
export function inspect(h: HealthHooks): HealthReading | null {
  const term = h.term()
  const host = h.host()
  if (!term || !host) return null
  const box = h.box()
  if (box.width < 2 || box.height < 2) return null

  const metrics = cellMetrics(term, h.fontSize())
  const fit = fitGeometry(box, metrics)
  const actual = { cols: term.cols, rows: term.rows }
  const declared = h.declared()
  const reasons: HealthReason[] = []

  // 1 + 5. The emulator disagrees with what the container can fit. This covers
  // the container-mismatch case too: the box is the ResizeObserver's own
  // measurement, so a container that resized without a refit lands here.
  if (fit.cols !== actual.cols || fit.rows !== actual.rows) reasons.push('geometry-drift')

  // The PTY geometry we told the server disagrees with what this container now
  // implies. Reported separately because the fix is a `resize`, not a local
  // term.resize().
  const pty = ptyGeometry(box)
  if (declared && (declared.cols !== pty.cols || declared.rows !== pty.rows))
    reasons.push('container-mismatch')

  // 2. The cell box moved since the baseline: a webfont finally loaded, or
  // zoom/DPR/rotation changed. Every column is now computed from a stale
  // number, which is the B8 failure that renders a plausible-looking but
  // wrong grid.
  const base = baselines.get(term)
  if (
    base &&
    (Math.abs(base.width - metrics.width) > METRIC_EPSILON_PX ||
      Math.abs(base.height - metrics.height) > METRIC_EPSILON_PX)
  )
    reasons.push('cell-metric-drift')

  // 3 + 4 need the buffer, and a buffer that is still being written is not
  // evidence of anything.
  if (h.sinceSnapshot() > SETTLE_MS) {
    const scan = scanBuffer(term)
    // VOID. The server has sent us real content and the screen is BLANK. Note
    // how narrow this is on purpose: "content in a band with blank space below"
    // is also what a correct shell looks like after `ls`, so it is NOT treated
    // as a void. A totally empty grid that has been fed kilobytes is not
    // ambiguous.
    if (h.bytesSeen() > VOID_MIN_BYTES && scan.nonBlank === 0) reasons.push('void')
    // WRAP DAMAGE. A continuation row whose predecessor did not fill the width
    // means the buffer was wrapped at a DIFFERENT cols than the one in force.
    if (scan.wrapDamage) reasons.push('wrap-damage')
  }

  if (reasons.length === 0) return null
  return { reasons, fit, actual, declared, metrics }
}

/** Cell metrics as first measured, per terminal. */
const baselines = new WeakMap<Terminal, CellMetrics>()

export function setBaseline(term: Terminal, m: CellMetrics) {
  baselines.set(term, m)
}

interface Scan {
  nonBlank: number
  wrapDamage: boolean
}

function scanBuffer(term: Terminal): Scan {
  const buf = term.buffer.active
  let nonBlank = 0
  let wrapDamage = false
  let prevFull = true
  const top = buf.viewportY
  for (let y = top; y < top + term.rows; y++) {
    const line = buf.getLine(y)
    if (!line) continue
    const text = line.translateToString(true)
    if (text.length > 0) nonBlank++
    // A wrapped line continues the one above it, which therefore MUST have run
    // to the full width. If it did not, this buffer was laid out at a width
    // that is no longer the width.
    if (line.isWrapped && !prevFull) wrapDamage = true
    prevFull = line.translateToString(false).trimEnd().length >= term.cols
  }
  return { nonBlank, wrapDamage }
}

interface RepairState {
  attempt: number
  nextAt: number
  /** Repair timestamps inside the rolling minute. */
  window: number[]
  /** Signature we already tried and failed to change — do not retry it. */
  accepted: string | null
  lastSig: string | null
  gaveUp: boolean
}

export interface Watchdog {
  /** Run one check now. Returns the reading if a problem was seen. */
  check: (force?: boolean) => HealthReading | null
  /** Forget the accepted signature and the budget (used on reconnect/attach). */
  reset: () => void
  dispose: () => void
  state: () => { target: string } & RepairState
}

export function startWatchdog(target: string, h: HealthHooks): Watchdog {
  const st: RepairState = {
    attempt: 0,
    nextAt: 0,
    window: [],
    accepted: null,
    lastSig: null,
    gaveUp: false,
  }

  const budgetOk = (now: number) => {
    st.window = st.window.filter((t) => now - t < 60_000)
    return st.window.length < MAX_REPAIRS_PER_MIN
  }

  const check = (force = false): HealthReading | null => {
    // Quiet when nobody can see it. A hidden tab also reports a zero-size
    // container in some browsers, which would be a guaranteed false positive.
    if (!force && typeof document !== 'undefined' && document.hidden) return null
    const reading = inspect(h)
    if (!reading) {
      // Healthy. Anything we were holding against this target is stale.
      if (st.lastSig || st.gaveUp) {
        st.lastSig = null
        st.accepted = null
        st.attempt = 0
        if (st.gaveUp) {
          st.gaveUp = false
          h.onRecovered()
        }
      }
      return null
    }

    const sig = signature(reading)
    countHealth('detect', reading.reasons)
    recordHealth(target, { reasons: reading.reasons, signature: sig })

    // A repair that changed nothing must not retrigger itself forever. If the
    // problem is byte-identical to one we already spent an attempt on, this is
    // a reading we cannot improve — hold it and wait for the measurement to
    // change.
    if (st.accepted === sig) return reading

    const now = Date.now()
    if (now < st.nextAt) return reading
    if (!budgetOk(now)) {
      if (!st.gaveUp) {
        st.gaveUp = true
        countHealth('giveup', reading.reasons)
        h.onGiveUp(reading)
      }
      return reading
    }

    const sameAsLast = st.lastSig === sig
    st.lastSig = sig
    st.window.push(now)
    st.attempt += 1
    st.nextAt = now + Math.min(BACKOFF_MAX_MS, BACKOFF_BASE_MS * 2 ** (st.attempt - 1))

    // STEP 1 — re-measure and re-fit. This alone fixes the common case (a font
    // that loaded late, a container that changed) and costs no repaint.
    const changed = h.refit()
    countHealth('repair', reading.reasons)
    if (changed && !sameAsLast) {
      // Something measurably moved. Re-check on the next tick before escalating
      // to anything that resets the screen.
      return reading
    }

    // STEP 2 — the fit did not move it, so the emulator's own buffer is what is
    // wrong. Reset and repaint from a server snapshot. This is a LEGITIMATE
    // reset, unlike the per-`full`-frame reset we removed: it happens only on a
    // measured mismatch that a refit could not settle.
    if (st.attempt >= 2) {
      h.repaint()
      countHealth('repaint', reading.reasons)
    }

    // If the same signature survives a refit AND a repaint, stop fighting it.
    if (sameAsLast && st.attempt >= 3) {
      st.accepted = sig
      countHealth('failed', reading.reasons)
      if (!st.gaveUp) {
        st.gaveUp = true
        h.onGiveUp(reading)
      }
    }
    return reading
  }

  const timer = window.setInterval(() => void check(), CHECK_INTERVAL_MS)

  // Events KNOWN to invalidate a fit. A tick alone would eventually catch
  // these, but "eventually" is up to two seconds of visibly wrong rendering.
  const onVisible = () => {
    if (!document.hidden) window.setTimeout(() => void check(), 50)
  }
  const onOrientation = () => window.setTimeout(() => void check(), 250)
  document.addEventListener('visibilitychange', onVisible)
  window.addEventListener('orientationchange', onOrientation)
  // A webfont landing AFTER mount re-measures every cell. This is the single
  // most common way the grid ends up plausible but wrong.
  void document.fonts?.ready.then(() => window.setTimeout(() => void check(), 100))

  return {
    check,
    reset: () => {
      st.attempt = 0
      st.nextAt = 0
      st.window = []
      st.accepted = null
      st.lastSig = null
      st.gaveUp = false
    },
    dispose: () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
      window.removeEventListener('orientationchange', onOrientation)
    },
    state: () => ({ target, ...st }),
  }
}
