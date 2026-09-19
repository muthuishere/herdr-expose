/**
 * Viewport registry.
 *
 * SPEC §3's `viewport` frame is a COMPLETE declaration of what this client
 * renders — the server reads it as the whole truth, not a patch. So more than
 * one component sending its own map would clobber the others (the pane list
 * saying "six summaries" and the terminal saying "one live" would alternate).
 *
 * Every component that renders a pane registers here instead; this module owns
 * the union and is the only caller of sendViewport(). `live` always wins over
 * `summary`, and anything unregistered is simply absent — which the server
 * reads as `none`.
 *
 * We still never REQUEST live as a privilege: we declare it because a
 * full-screen terminal for that pane is genuinely on screen.
 */

import type { PaneId, ViewportMode } from '../protocol/types'
import { sendViewport } from './connection'

const sources = new Map<string, Record<PaneId, ViewportMode>>()
let lastKey = ''
let scheduled = false

/**
 * `transcript` outranks `summary` and is outranked by `live`. Opening a pane as
 * a transcript while the pane LIST still declares it a summary tile must not
 * silently demote it back to a summary poll.
 */
const RANK: Record<ViewportMode, number> = { live: 3, transcript: 2, summary: 1, none: 0 }

function merged(): Record<PaneId, ViewportMode> {
  const out: Record<PaneId, ViewportMode> = {}
  for (const map of sources.values())
    for (const [id, mode] of Object.entries(map))
      if (!out[id] || RANK[mode] > RANK[out[id]]) out[id] = mode
  return out
}

function flush() {
  scheduled = false
  const map = merged()
  const key = JSON.stringify(map)
  if (key === lastKey) return
  lastKey = key
  sendViewport(map)
}

function schedule() {
  if (scheduled) return
  scheduled = true
  // Coalesce: mount/unmount of a list and a terminal is one declaration.
  queueMicrotask(flush)
}

/** Declare what `source` renders. Pass {} to declare it renders nothing. */
export function declareViewport(source: string, map: Record<PaneId, ViewportMode>) {
  sources.set(source, map)
  schedule()
}

export function releaseViewport(source: string) {
  sources.delete(source)
  schedule()
}

/** After a reconnect the server knows nothing about us. Re-declare from scratch. */
export function resendViewport() {
  lastKey = ''
  schedule()
}
