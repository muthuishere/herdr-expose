/**
 * Renderer probe-and-degrade — SPEC B8.
 *
 * Not a config flag. The failure we are defending against is a blank black
 * rectangle on some Android drivers, which no feature-detect predicts.
 *
 * Order:
 *  1. Coarse pointer => mount Canvas FIRST. Mobile WebGL loses context or
 *     renders blank on some drivers. Fine pointer => WebGL, which is fine there.
 *  2. Probe only AFTER content has been drawn. Probing an empty buffer is a
 *     guaranteed false positive — a blank terminal really is all one colour.
 *  3. All-identical pixels => dead surface => dispose the addon, fall back to
 *     the DOM renderer.
 *  4. Persist the failure in localStorage keyed by UA, 30-day TTL, so we do not
 *     re-probe on every load.
 *  5. Re-verify once after the first resize.
 */

import type { Terminal } from '@xterm/xterm'

export type RendererKind = 'webgl' | 'canvas' | 'dom'

interface Addon {
  dispose(): void
}

const LS_KEY = 'herdr-expose.renderer-blocklist'
const TTL_MS = 30 * 24 * 60 * 60 * 1000

interface BlockEntry {
  ua: string
  kind: RendererKind
  at: number
}

function uaKey(): string {
  return navigator.userAgent.slice(0, 200)
}

function readBlocklist(): BlockEntry[] {
  try {
    const raw = localStorage.getItem(LS_KEY)
    if (!raw) return []
    const list = JSON.parse(raw) as BlockEntry[]
    const now = Date.now()
    return Array.isArray(list) ? list.filter((e) => now - e.at < TTL_MS) : []
  } catch {
    return []
  }
}

function writeBlocklist(list: BlockEntry[]) {
  try {
    localStorage.setItem(LS_KEY, JSON.stringify(list.slice(-8)))
  } catch {
    /* private mode — we simply re-probe next load */
  }
}

export function isBlocked(kind: RendererKind): boolean {
  const ua = uaKey()
  return readBlocklist().some((e) => e.ua === ua && e.kind === kind)
}

export function blockRenderer(kind: RendererKind) {
  const list = readBlocklist().filter((e) => !(e.ua === uaKey() && e.kind === kind))
  list.push({ ua: uaKey(), kind, at: Date.now() })
  writeBlocklist(list)
}

/** Width-independent: this one IS about the GPU, so coarse pointer is right. */
function prefersCanvasFirst(): boolean {
  return window.matchMedia?.('(pointer: coarse)').matches ?? false
}

export interface AttachedRenderer {
  kind: RendererKind
  /** Run after content has been drawn. Returns the kind actually in use. */
  verify(): Promise<RendererKind>
  dispose(): void
}

/**
 * Mount the best available renderer. Returns immediately with the DOM renderer
 * if everything accelerated is blocked for this UA.
 */
export async function attachRenderer(term: Terminal): Promise<AttachedRenderer> {
  const order: RendererKind[] = prefersCanvasFirst()
    ? ['canvas', 'webgl']
    : ['webgl', 'canvas']

  for (const kind of order) {
    if (isBlocked(kind)) continue
    const addon = await loadAddon(kind)
    if (!addon) continue
    try {
      term.loadAddon(addon as never)
    } catch {
      safeDispose(addon)
      blockRenderer(kind)
      continue
    }
    let verified = false
    return {
      kind,
      dispose: () => safeDispose(addon),
      async verify() {
        if (verified) return kind
        verified = true
        // One more frame so the first paint has definitely landed.
        await nextFrame()
        if (surfaceLooksDead(term)) {
          blockRenderer(kind)
          safeDispose(addon)
          return 'dom'
        }
        return kind
      },
    }
  }

  return { kind: 'dom', verify: async () => 'dom', dispose: () => {} }
}

async function loadAddon(kind: RendererKind): Promise<Addon | null> {
  try {
    if (kind === 'webgl') {
      if (!hasWebGL()) return null
      const m = await import('@xterm/addon-webgl')
      return new m.WebglAddon() as unknown as Addon
    }
    if (kind === 'canvas') {
      const m = await import('@xterm/addon-canvas')
      return new m.CanvasAddon() as unknown as Addon
    }
  } catch {
    return null
  }
  return null
}

function hasWebGL(): boolean {
  try {
    const c = document.createElement('canvas')
    return !!(c.getContext('webgl2') || c.getContext('webgl'))
  } catch {
    return false
  }
}

function safeDispose(a: Addon) {
  try {
    a.dispose()
  } catch {
    /* already gone */
  }
}

function nextFrame(): Promise<void> {
  return new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r())))
}

/**
 * Sample the accelerated canvas. All-identical pixels across a spread of points
 * means nothing was drawn — the surface is dead. The caller must only reach
 * here AFTER real content has been written.
 */
function surfaceLooksDead(term: Terminal): boolean {
  const el = term.element
  if (!el) return false
  const canvases = Array.from(el.querySelectorAll('canvas'))
  if (canvases.length === 0) return false // DOM renderer took over already

  for (const canvas of canvases) {
    if (canvas.width === 0 || canvas.height === 0) continue
    // A WebGL context cannot be read through getContext('2d'); if we cannot
    // read it, we cannot claim it is dead.
    let ctx: CanvasRenderingContext2D | null = null
    try {
      ctx = canvas.getContext('2d', { willReadFrequently: true })
    } catch {
      ctx = null
    }
    if (ctx) {
      if (!allIdentical(ctx, canvas)) return false
    } else {
      // WebGL path: ask the context whether it is still alive at all.
      const gl =
        (canvas.getContext('webgl2') as WebGL2RenderingContext | null) ??
        (canvas.getContext('webgl') as WebGLRenderingContext | null)
      if (gl && !gl.isContextLost()) return false
      if (!gl) return false
    }
  }
  return true
}

function allIdentical(ctx: CanvasRenderingContext2D, canvas: HTMLCanvasElement): boolean {
  const w = canvas.width
  const h = canvas.height
  const points: Array<[number, number]> = []
  for (let i = 1; i <= 6; i++)
    for (let j = 1; j <= 4; j++)
      points.push([Math.floor((w * i) / 7), Math.floor((h * j) / 5)])
  let first: string | null = null
  for (const [x, y] of points) {
    let d: Uint8ClampedArray
    try {
      d = ctx.getImageData(x, y, 1, 1).data
    } catch {
      return false // tainted / unreadable: do not guess
    }
    const key = `${d[0]},${d[1]},${d[2]},${d[3]}`
    if (first === null) first = key
    else if (key !== first) return false
  }
  return true
}
