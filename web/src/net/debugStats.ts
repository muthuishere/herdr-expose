/**
 * Cheap always-on counters, read from the console or a harness as
 * `__herdrStats()`. SPEC F2.5: measure, do not assert.
 *
 * This exists because "it flickers" has at least four different causes that
 * look identical on screen — snapshots arriving at N/s, a terminal reset per
 * snapshot, an xterm remount, or the same target streamed twice. One object
 * with per-target counts tells them apart in one read instead of a guess.
 *
 * Counting is a Map increment on the binary path; it never touches the payload.
 */

type Counter = Record<string, number>

const started = Date.now()
const perTarget = new Map<string, Counter>()
const global: Counter = {
  reset: 0,
  termCreate: 0,
  termDispose: 0,
  subscribe: 0,
  unsubscribe: 0,
  viewportSent: 0,
  // Counted per target as well, because "did opening this pane resize it?" is
  // the one question transcript mode exists to answer, and a count is a proof
  // where a screenshot is an opinion.
  resizeSent: 0,
}

/**
 * Render-health counters. The watchdog is only trustworthy if it is READABLE:
 * "it fixed itself" and "it is repairing in a loop" look identical on screen
 * and are one counter apart here. `repair` is an attempt, `repaint` is the
 * escalation that resets the emulator, `failed` means a repair could not change
 * the measurement, `giveup` means the budget was spent and the user was told.
 */
const health: Counter = {
  detect: 0,
  repair: 0,
  repaint: 0,
  failed: 0,
  giveup: 0,
}
const healthReasons: Counter = {}
const healthLast: Record<string, { reasons: string[]; signature: string; at: number }> = {}

export type HealthCounter = keyof typeof health

export function countHealth(name: HealthCounter, reasons: readonly string[] = []) {
  health[name] += 1
  for (const r of reasons) healthReasons[r] = (healthReasons[r] ?? 0) + 1
}

export function recordHealth(
  target: string,
  d: { reasons: readonly string[]; signature: string },
) {
  healthLast[target] = { reasons: [...d.reasons], signature: d.signature, at: Date.now() }
}

function bucket(target: string): Counter {
  let c = perTarget.get(target)
  if (!c) {
    c = { frame: 0, snapshot: 0, gap: 0, bytes: 0 }
    perTarget.set(target, c)
  }
  return c
}

export function countBinary(target: string, kind: 'frame' | 'snapshot' | 'gap', bytes: number) {
  const c = bucket(target)
  c[kind] += 1
  c.bytes += bytes
}

export function countEvent(name: keyof typeof global, target?: string) {
  global[name] += 1
  if (target) {
    const c = bucket(target)
    c[name] = (c[name] ?? 0) + 1
  }
}

/** The last viewport map actually sent — the merged declaration, verbatim. */
let lastViewport: Record<string, string> = {}
export function recordViewport(map: Record<string, string>) {
  lastViewport = { ...map }
}

export interface HerdrStats {
  seconds: number
  global: Counter
  viewport: Record<string, string>
  targets: Record<string, Counter & { snapshotsPerSec: number; framesPerSec: number }>
  /** Render-health watchdog: counts, per-reason counts, and the last reading. */
  health: {
    counts: Counter
    reasons: Counter
    last: Record<string, { reasons: string[]; signature: string; at: number }>
  }
}

export function stats(): HerdrStats {
  const seconds = Math.max(0.001, (Date.now() - started) / 1000)
  const targets: HerdrStats['targets'] = {}
  for (const [t, c] of perTarget) {
    targets[t] = {
      ...c,
      snapshotsPerSec: round(c.snapshot / seconds),
      framesPerSec: round(c.frame / seconds),
    }
  }
  return {
    seconds: round(seconds),
    global: { ...global },
    viewport: { ...lastViewport },
    targets,
    health: {
      counts: { ...health },
      reasons: { ...healthReasons },
      last: JSON.parse(JSON.stringify(healthLast)) as HerdrStats['health']['last'],
    },
  }
}

function round(n: number): number {
  return Math.round(n * 100) / 100
}

declare global {
  interface Window {
    __herdrStats?: () => HerdrStats
  }
}

if (typeof window !== 'undefined') window.__herdrStats = stats
