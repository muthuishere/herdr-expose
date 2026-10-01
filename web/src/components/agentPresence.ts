/**
 * Whether Herdr is actually tracking an agent in this pane.
 *
 * NOT `!!pane.agent`. Herdr reports `agent_status: "unknown"` for every plain
 * shell pane — verified on 0.9.0 — so the tree carries an agent object for a
 * bare zsh split. The question this actually asks is "is there an agent whose
 * state means anything", and `unknown` is precisely the value that means it
 * does not.
 *
 * This is all that survives of paneViewMode.ts. That module existed to
 * remember which pane opened as a transcript and which as a terminal, and to
 * hold the per-session consent gate in front of the terminal. With the
 * terminal gone there is one view, so there is nothing to remember and nothing
 * to consent to.
 */

import type { TreePane } from '../protocol/types'

export function hasRealAgent(pane: TreePane | undefined): boolean {
  const state = pane?.agent?.state
  return !!state && state !== 'unknown'
}
