/**
 * Viewport hooks — SPEC B8.
 *
 * Two separate concerns that are easy to conflate:
 *
 *  1. SHELL SELECTION is WIDTH-ONLY at 900px, evaluated SYNCHRONOUSLY on first
 *     render so a phone never flashes the desktop header. `pointer: coarse` is
 *     the wrong signal — desktop-mode phones and touch laptops report a fine
 *     pointer.
 *
 *  2. SOFT KEYBOARD: only `visualViewport.height` shrinks. `100dvh` and
 *     `window.innerHeight` both keep reporting the full screen when the iOS
 *     keyboard opens. We drive `--app-height` and `--keyboard-inset` from
 *     visualViewport, use an 80px threshold to reject browser-chrome-collapse
 *     noise, and coalesce in an rAF because iOS fires a burst of resize+scroll
 *     events for the whole keyboard animation.
 */

import { useEffect, useState } from 'react'

export const MOBILE_BREAKPOINT_PX = 900

/** Width-only. Read synchronously; no effect, no flash of the wrong shell. */
function readIsMobile(): boolean {
  if (typeof window === 'undefined') return true
  return window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT_PX - 1}px)`).matches
}

export function useIsMobile(): boolean {
  const [isMobile, setIsMobile] = useState<boolean>(readIsMobile)
  useEffect(() => {
    const mq = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT_PX - 1}px)`)
    const on = () => setIsMobile(mq.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])
  return isMobile
}

/** Below this, a height change is browser chrome collapsing, not a keyboard. */
const KEYBOARD_THRESHOLD_PX = 80

export interface ViewportMetrics {
  /** Usable height in CSS px (visualViewport when available). */
  height: number
  /** Height stolen by the soft keyboard, 0 when it is closed. */
  keyboardInset: number
  keyboardOpen: boolean
}

let installed = false

/**
 * Install the global viewport driver exactly once. It writes CSS variables, so
 * layout can react without React re-rendering on every keyboard animation frame.
 */
export function installViewportDriver(onChange?: (m: ViewportMetrics) => void): () => void {
  if (installed) return () => {}
  installed = true

  const vv = window.visualViewport
  const root = document.documentElement
  /** Tallest height seen with the keyboard closed — the reference for the inset. */
  let baseHeight = vv?.height ?? window.innerHeight
  let raf = 0

  const measure = () => {
    raf = 0
    const h = vv?.height ?? window.innerHeight
    if (h > baseHeight) baseHeight = h
    const rawInset = baseHeight - h
    const keyboardOpen = rawInset > KEYBOARD_THRESHOLD_PX
    const keyboardInset = keyboardOpen ? Math.round(rawInset) : 0

    root.style.setProperty('--app-height', `${Math.round(h)}px`)
    root.style.setProperty('--keyboard-inset', `${keyboardInset}px`)
    root.classList.toggle('kb-open', keyboardOpen)
    onChange?.({ height: h, keyboardInset, keyboardOpen })
  }

  // iOS fires a burst of resize+scroll for the whole keyboard animation.
  const schedule = () => {
    if (raf) return
    raf = requestAnimationFrame(measure)
  }

  measure()
  vv?.addEventListener('resize', schedule)
  vv?.addEventListener('scroll', schedule)
  window.addEventListener('resize', schedule)
  window.addEventListener('orientationchange', () => {
    // Orientation invalidates the baseline entirely.
    baseHeight = 0
    schedule()
  })

  return () => {
    installed = false
    vv?.removeEventListener('resize', schedule)
    vv?.removeEventListener('scroll', schedule)
    window.removeEventListener('resize', schedule)
  }
}

export function useViewportMetrics(): ViewportMetrics {
  const [m, setM] = useState<ViewportMetrics>(() => ({
    height: window.visualViewport?.height ?? window.innerHeight,
    keyboardInset: 0,
    keyboardOpen: false,
  }))
  useEffect(() => installViewportDriver(setM), [])
  return m
}

/** ResizeObserver as a hook, for terminal geometry. */
export function useElementSize<T extends HTMLElement>(
  ref: React.RefObject<T>,
  onResize: (box: { width: number; height: number }) => void,
) {
  useEffect(() => {
    const el = ref.current
    if (!el) return
    let raf = 0
    const ro = new ResizeObserver((entries) => {
      const e = entries[0]
      if (!e) return
      if (raf) return
      raf = requestAnimationFrame(() => {
        raf = 0
        const r = e.target.getBoundingClientRect()
        onResize({ width: r.width, height: r.height })
      })
    })
    ro.observe(el)
    return () => {
      if (raf) cancelAnimationFrame(raf)
      ro.disconnect()
    }
  }, [ref, onResize])
}
