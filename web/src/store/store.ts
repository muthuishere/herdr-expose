/**
 * ONE store, fed by the WS stream (SPEC §8 / architecture.md §"Client").
 *
 * Rules enforced here:
 *  - Tree structure is NEVER derived client-side. `tree` frames replace it whole.
 *  - Agent state comes from the server (`agent` frames + the tree). We do not
 *    compute "done"/"unseen" — the server owns seen state per connection.
 *  - Terminal bytes are held per target as a bounded text buffer; the live pane
 *    feeds xterm directly and does not read from here.
 */

import { useSyncExternalStore } from 'react'
import type {
  AgentData,
  ClosedData,
  PaneId,
  ResultData,
  TreeData,
  TreePane,
  ViewportMode,
  WelcomeData,
} from '../protocol/types'

export type LinkState = 'connecting' | 'open' | 'degraded' | 'reconnecting' | 'offline'

/** Bytes of terminal text kept per non-live target. Tiles need very little. */
const SUMMARY_BUFFER_CHARS = 8_000

export interface PaneRuntime {
  /** Latest rendered text (ANSI intact) for summary tiles / row digests. */
  text: string
  /** Server-reported bytes lost since the last snapshot. Shown, never hidden. */
  droppedBytes: number
  closed?: { reason?: string }
  updatedAt: number
}

export interface AppState {
  link: LinkState
  /** Round-trip time in ms from the last pong, or null before the first. */
  rttMs: number | null
  /** Last applied server seq; sent on reconnect to resume. */
  lastSeq: number
  welcome: WelcomeData | null
  /** Why we are not connected, when we are not. */
  lastError: string | null
  /** Consecutive failed connection attempts. */
  attempt: number

  tree: TreeData
  /** Flat index built from the tree ONLY for O(1) lookup — not structure. */
  panesById: Record<PaneId, TreePane>

  runtime: Record<PaneId, PaneRuntime>
  /** Per-pane detection text from `agent` frames when state === 'blocked'. */
  detection: Record<PaneId, string>

  /** The pane the USER has opened full-screen; drives the `viewport` frame. */
  focused: PaneId | null
  /** Panes currently on screen as tiles/rows; drives the `viewport` frame. */
  visible: PaneId[]
  /** Server-confirmed modes, echoed back via snapshots/tree. */
  serverModes: Record<PaneId, ViewportMode>

  /** Pending `command` ids -> resolvers. */
  results: Record<string, ResultData>
}

const EMPTY_TREE: TreeData = { workspaces: [] }

const initial: AppState = {
  link: 'connecting',
  rttMs: null,
  lastSeq: 0,
  welcome: null,
  lastError: null,
  attempt: 0,
  tree: EMPTY_TREE,
  panesById: {},
  runtime: {},
  detection: {},
  focused: null,
  visible: [],
  serverModes: {},
  results: {},
}

let state: AppState = initial
const listeners = new Set<() => void>()

function emit() {
  for (const l of listeners) l()
}

function set(patch: Partial<AppState>) {
  state = { ...state, ...patch }
  emit()
}

/**
 * High-frequency path (terminal output for TILES). Mutating the store on every
 * frame would re-render the list at the pane's output rate. We coalesce to one
 * notification per animation frame — the same 16ms budget the server coalesces
 * on. The live pane does not go through here at all; it gets raw bytes.
 */
let rafPending = 0
function setLazy(patch: Partial<AppState>) {
  state = { ...state, ...patch }
  if (rafPending) return
  rafPending = requestAnimationFrame(() => {
    rafPending = 0
    emit()
  })
}

export function getState(): AppState {
  return state
}

export function subscribeStore(fn: () => void): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

export function useStore<T>(selector: (s: AppState) => T): T {
  return useSyncExternalStore(
    subscribeStore,
    () => selector(state),
    () => selector(initial),
  )
}

/* ------------------------------------------------------------------ */
/* Frame application                                                   */
/* ------------------------------------------------------------------ */

function indexTree(tree: TreeData): Record<PaneId, TreePane> {
  const out: Record<PaneId, TreePane> = {}
  for (const ws of tree.workspaces ?? [])
    for (const tab of ws.tabs ?? []) for (const p of tab.panes ?? []) out[p.id] = p
  return out
}

function appendText(prev: string, add: string): string {
  const next = prev + add
  return next.length > SUMMARY_BUFFER_CHARS ? next.slice(next.length - SUMMARY_BUFFER_CHARS) : next
}

export const apply = {
  welcome(seq: number, d: WelcomeData) {
    set({ welcome: d, lastSeq: seq, link: 'open', lastError: null, attempt: 0 })
  },

  /** Replace the tree wholesale. No merging, no client-side structure. */
  tree(seq: number, d: TreeData) {
    const panesById = indexTree(d)
    const modes = { ...state.serverModes }
    for (const p of Object.values(panesById)) if (p.mode) modes[p.id] = p.mode
    // Drop runtime for panes the server no longer lists.
    const runtime: Record<PaneId, PaneRuntime> = {}
    for (const [id, r] of Object.entries(state.runtime)) if (panesById[id]) runtime[id] = r
    set({ tree: d, panesById, serverModes: modes, runtime, lastSeq: seq })
  },

  /**
   * Data-plane type 1. `text` is the streaming-decoded UTF-8 of the raw payload,
   * kept only for tiles/digests; the live pane gets the bytes via byteBus.
   */
  frame(seq: number, target: PaneId, text: string) {
    const prev = state.runtime[target]
    setLazy({
      lastSeq: seq,
      runtime: {
        ...state.runtime,
        [target]: {
          text: appendText(prev?.text ?? '', text),
          droppedBytes: prev?.droppedBytes ?? 0,
          closed: prev?.closed,
          updatedAt: Date.now(),
        },
      },
    })
  },

  /** Data-plane type 2. A snapshot REPLACES the buffer — full repaint, not a delta. */
  snapshot(seq: number, target: PaneId, text: string) {
    setLazy({
      lastSeq: seq,
      runtime: {
        ...state.runtime,
        [target]: { text, droppedBytes: 0, updatedAt: Date.now() },
      },
    })
  },

  /** Data-plane type 3, or the JSON `gap` control frame. Identical handling. */
  gap(seq: number, target: PaneId, bytesDropped: number) {
    const prev = state.runtime[target]
    setLazy({
      lastSeq: seq,
      runtime: {
        ...state.runtime,
        [target]: {
          text: prev?.text ?? '',
          droppedBytes: (prev?.droppedBytes ?? 0) + bytesDropped,
          closed: prev?.closed,
          updatedAt: Date.now(),
        },
      },
    })
  },

  closed(seq: number, d: ClosedData) {
    const prev = state.runtime[d.target]
    set({
      lastSeq: seq,
      runtime: {
        ...state.runtime,
        [d.target]: {
          text: prev?.text ?? '',
          droppedBytes: prev?.droppedBytes ?? 0,
          closed: { reason: d.reason },
          updatedAt: Date.now(),
        },
      },
    })
  },

  /**
   * Agent state change. We patch the pane's agent in place so the row updates
   * before the next full `tree` arrives — this is a mirror of server state,
   * not a derivation of it.
   */
  agent(seq: number, d: AgentData) {
    const pane = state.panesById[d.target]
    const detection =
      d.state === 'blocked'
        ? { ...state.detection, [d.target]: d.detection ?? state.detection[d.target] ?? '' }
        : omit(state.detection, d.target)
    if (!pane) {
      set({ lastSeq: seq, detection })
      return
    }
    const nextPane: TreePane = {
      ...pane,
      agent: {
        id: d.id,
        kind: d.kind ?? pane.agent?.kind,
        state: d.state,
        summary: d.summary ?? pane.agent?.summary,
        changed_at: d.changed_at ?? Date.now(),
      },
    }
    set({
      lastSeq: seq,
      detection,
      panesById: { ...state.panesById, [d.target]: nextPane },
      tree: patchTree(state.tree, nextPane),
    })
  },

  result(seq: number, d: ResultData) {
    set({ lastSeq: seq, results: { ...state.results, [d.id]: d } })
  },

  pong(seq: number, rttMs: number) {
    const link: LinkState =
      state.link === 'open' || state.link === 'degraded'
        ? rttMs > DEGRADED_RTT_MS
          ? 'degraded'
          : 'open'
        : state.link
    set({ lastSeq: seq, rttMs, link })
  },
}

export const DEGRADED_RTT_MS = 450

function omit<T>(obj: Record<string, T>, key: string): Record<string, T> {
  if (!(key in obj)) return obj
  const next = { ...obj }
  delete next[key]
  return next
}

/** Replace one pane inside the tree without changing any structure. */
function patchTree(tree: TreeData, pane: TreePane): TreeData {
  return {
    ...tree,
    workspaces: (tree.workspaces ?? []).map((ws) => ({
      ...ws,
      tabs: (ws.tabs ?? []).map((tab) => ({
        ...tab,
        panes: (tab.panes ?? []).map((p) => (p.id === pane.id ? pane : p)),
      })),
    })),
  }
}

/* ------------------------------------------------------------------ */
/* Connection-level setters                                            */
/* ------------------------------------------------------------------ */

export function setLink(link: LinkState, err?: string | null, attempt?: number) {
  set({
    link,
    lastError: err === undefined ? state.lastError : err,
    attempt: attempt ?? state.attempt,
    rttMs: link === 'offline' || link === 'reconnecting' ? null : state.rttMs,
  })
}

export function setFocused(id: PaneId | null) {
  set({ focused: id })
}

export function setVisible(ids: PaneId[]) {
  const a = state.visible
  if (a.length === ids.length && a.every((x, i) => x === ids[i])) return
  set({ visible: ids })
}

/** Reset only the volatile parts on a session change (resume impossible). */
export function resetSession() {
  set({ lastSeq: 0, runtime: {}, detection: {}, tree: EMPTY_TREE, panesById: {} })
}
