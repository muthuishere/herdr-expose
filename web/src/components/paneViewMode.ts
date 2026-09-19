/**
 * Which view a pane opens in, and how that choice is remembered.
 *
 * LOOKING MUST NOT TOUCH. Transcript is the default for EVERY pane now — agent
 * or plain shell — because it is the only view that cannot mutate the owner's
 * session. J2 made it the default only for agent panes, which left two doors
 * open: a shell pane opened straight into the terminal, and the `term` toggle
 * was one tap away on anything.
 *
 * A shell renders perfectly well as text. It is output like any other output,
 * and the reason to open one from a phone is almost always to read it.
 *
 * The terminal is still there, still exact, still the right tool when you need
 * a TUI or you are about to type — but it is now something you ASK for, once
 * per pane, having been told what it costs (see needsTerminalConsent).
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

/**
 * TRANSCRIPT, ALWAYS.
 *
 * `hasAgent` is still taken so callers read the same as before and so the
 * signature does not lie about what used to matter, but the answer no longer
 * depends on it: a plain shell is not a reason to attach to somebody's
 * terminal, it is just a pane whose output happens to be a shell prompt.
 */
export function defaultViewFor(_hasAgent: boolean): PaneViewKind {
  return 'transcript'
}

/**
 * Has this device already been told, this session, what the terminal view costs
 * on THIS pane?
 *
 * Terminal view attaches a real stream to a pane the owner may be working in,
 * and typing into it takes control of that pane. Neither of those is something
 * to do to someone by accident because a toggle was one tap away. So the first
 * time per pane we say so in one line and wait.
 *
 * sessionStorage, not localStorage, deliberately: "do not nag again for the
 * session" is exactly sessionStorage's lifetime, and a consent that outlived
 * the tab would be a consent nobody remembers giving.
 */
const CONSENT = 'hex.termok.'

export function hasTerminalConsent(target: PaneId): boolean {
  try {
    return sessionStorage.getItem(CONSENT + target) === '1'
  } catch {
    // No storage: ask again. Asking twice is a nuisance; attaching without
    // asking is the bug.
    return false
  }
}

export function grantTerminalConsent(target: PaneId): void {
  try {
    sessionStorage.setItem(CONSENT + target, '1')
  } catch {
    /* the view still opens; only the memory of the answer is lost */
  }
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
