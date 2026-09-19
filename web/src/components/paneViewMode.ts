/**
 * Which view a pane opens in, and how that choice is remembered.
 *
 * SPEC J2: transcript is the default for a pane WITH an agent, terminal is the
 * default for a pane with none. The terminal path is unchanged and is still the
 * right tool for a shell, a TUI, or any moment that needs exactness — it is the
 * DEFAULT that moves, not the capability.
 *
 * The choice is per pane and sticky, because "I want the terminal for the build
 * pane and the transcript for Claude" is the normal state of affairs, not an
 * exception. localStorage can throw (private mode, blocked site data), so every
 * access is wrapped and the default survives a failure.
 */

import type { PaneId, TreePane } from '../protocol/types'

export type PaneViewKind = 'transcript' | 'terminal'

const KEY = 'hex.paneview.'

/**
 * Whether Herdr is actually tracking an agent in this pane.
 *
 * NOT `!!pane.agent`. Herdr reports `agent_status: "unknown"` for every plain
 * shell pane — verified on 0.9.0 — so the tree carries an agent object for a
 * bare zsh split. Keying the default view on that object's existence made
 * every shell open as a transcript, which is both useless and, because the
 * terminal then never went live, indistinguishable from a broken pane.
 *
 * The question the default actually asks is "is there an agent whose state
 * means anything", and `unknown` is precisely the value that means it does
 * not.
 */
export function hasRealAgent(pane: TreePane | undefined): boolean {
  const state = pane?.agent?.state
  return !!state && state !== 'unknown'
}

export function defaultViewFor(hasAgent: boolean): PaneViewKind {
  return hasAgent ? 'transcript' : 'terminal'
}

export function readPaneView(target: PaneId, hasAgent: boolean): PaneViewKind {
  try {
    const v = localStorage.getItem(KEY + target)
    if (v === 'transcript' || v === 'terminal') return v
  } catch {
    /* no storage: the default is still correct */
  }
  return defaultViewFor(hasAgent)
}

export function writePaneView(target: PaneId, kind: PaneViewKind): void {
  try {
    localStorage.setItem(KEY + target, kind)
  } catch {
    /* the session still works; only the memory of the choice is lost */
  }
}
