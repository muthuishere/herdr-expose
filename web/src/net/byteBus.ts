/**
 * Raw-byte fan-out for the LIVE pane.
 *
 * Terminal payloads never enter the React store — they go straight from the
 * WebSocket binary frame to xterm.write(Uint8Array). The store keeps only the
 * decoded text needed for summary tiles and row digests.
 */

import type { PaneId } from '../protocol/types'

export type ByteKind = 'frame' | 'snapshot'
export type ByteSink = (bytes: Uint8Array, kind: ByteKind) => void

const sinks = new Map<PaneId, Set<ByteSink>>()

export function onBytes(target: PaneId, sink: ByteSink): () => void {
  let s = sinks.get(target)
  if (!s) {
    s = new Set()
    sinks.set(target, s)
  }
  s.add(sink)
  return () => {
    s!.delete(sink)
    if (s!.size === 0) sinks.delete(target)
  }
}

export function hasSink(target: PaneId): boolean {
  return (sinks.get(target)?.size ?? 0) > 0
}

export function pushBytes(target: PaneId, bytes: Uint8Array, kind: ByteKind): void {
  const s = sinks.get(target)
  if (!s) return
  for (const sink of s) sink(bytes, kind)
}
