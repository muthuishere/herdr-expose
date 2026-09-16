/**
 * Wire protocol v1 — SPEC.md §3 as amended by AMENDMENT A1 (split plane).
 *
 * CONTROL PLANE — JSON in WebSocket TEXT frames, shape
 *   `{"seq":<uint64>,"type":"<name>","data":{...}}`.
 *   `seq` is monotonic PER CONNECTION and assigned by the server; client->server
 *   frames carry seq 0 (the server does not sequence the client's side).
 *   out: hello subscribe unsubscribe viewport resize command ping
 *   in:  welcome tree agent result pong gap closed
 *
 * DATA PLANE — WebSocket BINARY frames, see ./binary.ts. There is NO `frame`
 * JSON type any more and NO base64 anywhere on the client.
 *
 * SPEC does not fix the payload shapes of the JSON types beyond `viewport` and
 * `command`. Everything below marked ASSUMED is this workstream's proposal and
 * must be reconciled with workstream A.
 */

export type PaneId = string

/** Control-plane client->server frame names. `input` moved to the data plane. */
export type ClientFrameType =
  | 'hello'
  | 'subscribe'
  | 'unsubscribe'
  | 'viewport'
  | 'resize'
  | 'command'
  | 'ping'

/**
 * Control-plane server->client frame names. `frame` and `snapshot` moved to the
 * data plane. `gap` is listed by A1 in BOTH planes (JSON control list AND
 * binary type 3) — we accept it on either and treat them identically.
 */
export type ServerFrameType =
  | 'welcome'
  | 'tree'
  | 'closed'
  | 'gap'
  | 'agent'
  | 'result'
  | 'pong'

export interface Envelope<T extends string, D> {
  seq: number
  type: T
  data: D
}

/* ------------------------------------------------------------------ */
/* Agent state                                                         */
/* ------------------------------------------------------------------ */

/**
 * Urgency order, most urgent first. `done` is Herdr's idle-but-UNSEEN
 * (architecture.md §6 "Seen state") — the server derives it per connection, the
 * client never computes it.
 */
export const AGENT_STATE_ORDER = ['blocked', 'done', 'working', 'idle', 'unknown'] as const
export type AgentState = (typeof AGENT_STATE_ORDER)[number]

export function agentUrgency(s: AgentState | undefined): number {
  const i = AGENT_STATE_ORDER.indexOf((s ?? 'unknown') as AgentState)
  return i < 0 ? AGENT_STATE_ORDER.length : i
}

/* ------------------------------------------------------------------ */
/* Tree (server-authoritative; the client NEVER derives structure)     */
/* ------------------------------------------------------------------ */

/** ASSUMED shape. The client renders exactly these nodes in exactly this order. */
export interface TreePane {
  id: PaneId
  /** Stable across moves (SPEC §0); used only as a display/debug hint. */
  terminal_id?: string
  title: string
  /** e.g. "zsh", "claude", "nvim" — display only, never parsed for behaviour. */
  command?: string
  cwd?: string
  /** Present when Herdr has an agent bound to this pane. */
  agent?: TreeAgent
  /** Server-decided render mode currently in effect for this pane. */
  mode?: ViewportMode
  /** Pane is gone (terminal.closed) but still listed until the tree drops it. */
  dead?: boolean
  /** Whether this connection currently holds control (vs observe). */
  controlling?: boolean
  cols?: number
  rows?: number
}

export interface TreeAgent {
  /** Herdr agent id; may differ from pane id. */
  id: string
  /** "claude" | "codex" | ... — display only. Generic across kinds. */
  kind?: string
  state: AgentState
  /** One-line human summary the SERVER produced. Not parsed. */
  summary?: string
  /**
   * Last state transition. The server sends RFC3339 (`2026-09-16T09:35:18+05:30`);
   * older builds and the mock send unix ms. Both are accepted — subtracting a
   * string from Date.now() yields NaN, which is how "working · NaNh NaNm"
   * reached the screen.
   */
  changed_at?: number | string
}

export interface TreeTab {
  id: string
  /** Human label. Tabs are NEVER rendered as their own row — see PaneList. */
  label?: string
  title?: string
  number?: number
  panes: TreePane[]
}

export interface TreeWorkspace {
  id: string
  label?: string
  title?: string
  number?: number
  tabs: TreeTab[]
}

/**
 * One Herdr session — the top level since multi-session landed. `id` is the
 * session NAME (stable across restarts, unlike pane ids), and every pane id
 * below it is already session-qualified as `<session>/<herdr id>`.
 */
export interface TreeSession {
  id: string
  name?: string
  running?: boolean
  connected?: boolean
  focused?: boolean
  origin?: boolean
  default?: boolean
  error?: string
  focused_pane?: PaneId
  workspaces: TreeWorkspace[]
}

export interface TreeData {
  /** The current shape. */
  sessions?: TreeSession[]
  /** The pre-multi-session shape; still accepted so the client never blanks. */
  workspaces?: TreeWorkspace[]
  rev?: number
  connected?: boolean
  focused_session?: string
  /** Session-qualified, like every other target on this wire. */
  focused_pane?: PaneId
  /** Pane the SERVER considers focused, if any (legacy name). */
  focused?: PaneId | null
  /** Bumped by the server on every tree emission; display/debug only. */
  revision?: number
}

/* ------------------------------------------------------------------ */
/* Server -> client                                                    */
/* ------------------------------------------------------------------ */

export interface WelcomeData {
  /** Protocol version of this stream. Expected "v1"/1. */
  protocol?: string | number
  /** SPEC §0 / architecture.md §"schema drift": surfaced so drift is visible. */
  herdr_version?: string
  server_version?: string
  /** Server session id; changes => our resume seq is worthless. */
  session?: string
}

export interface GapData {
  target: PaneId
  bytes_dropped: number
}

export interface ClosedData {
  target: PaneId
  reason?: string
}

/** Agent state change. Payload mirrors TreeAgent plus its pane. */
export interface AgentData extends TreeAgent {
  target: PaneId
  /**
   * `agent.read --source detection` text, present when state === 'blocked'.
   * Rendered verbatim. NO per-agent parsing (SPEC §8).
   */
  detection?: string
}

export interface ResultData {
  id: string
  ok: boolean
  /** Raw Herdr socket result payload; opaque to the client. */
  result?: unknown
  error?: string
}

export interface PongData {
  /** Echo of the client's `t` so RTT is measurable without a clock sync. */
  t: number
  /** Optional server-side view of upstream health. */
  upstream_ok?: boolean
}

export type ServerFrame =
  | Envelope<'welcome', WelcomeData>
  | Envelope<'tree', TreeData>
  | Envelope<'gap', GapData>
  | Envelope<'closed', ClosedData>
  | Envelope<'agent', AgentData>
  | Envelope<'result', ResultData>
  | Envelope<'pong', PongData>

/* ------------------------------------------------------------------ */
/* Client -> server                                                    */
/* ------------------------------------------------------------------ */

export type ViewportMode = 'live' | 'summary' | 'none'

export interface HelloData {
  protocol: 'v1'
  client: string
  /**
   * B4: resume is DELETED. There is no last_seq, no ring, no replay. On
   * reconnect we re-subscribe, take the fresh snapshot and repaint. `seq` in the
   * binary header survives only for ordering and debugging.
   */
  session?: string
}

/**
 * B2 — geometry. `viewport` is render-MODE; this is SIZE. Per-connection (B1),
 * floored at 20x6, and sent BEFORE the first frame is requested: a terminal
 * without a geometry message does not work.
 */
export interface ResizeData {
  target: PaneId
  cols: number
  rows: number
}

export const MIN_COLS = 20
export const MIN_ROWS = 6

export interface SubscribeData {
  targets: PaneId[]
}
export interface UnsubscribeData {
  targets: PaneId[]
}

/**
 * SPEC §3: the client declares what it RENDERS; the server decides the real
 * mode. We never send 'live' as a request for privilege — we send it because
 * that pane is the one full-screen terminal actually on screen.
 */
export interface ViewportData {
  targets: Record<PaneId, ViewportMode>
}

export interface CommandData {
  /** Passes straight through to the Herdr socket. Never enumerated. SPEC §3. */
  method: string
  params?: Record<string, unknown>
  id: string
}

export interface PingData {
  t: number
}

export type ClientFrame =
  | Envelope<'hello', HelloData>
  | Envelope<'subscribe', SubscribeData>
  | Envelope<'unsubscribe', UnsubscribeData>
  | Envelope<'viewport', ViewportData>
  | Envelope<'resize', ResizeData>
  | Envelope<'command', CommandData>
  | Envelope<'ping', PingData>

/* ------------------------------------------------------------------ */

export function isServerFrame(v: unknown): v is ServerFrame {
  return (
    typeof v === 'object' &&
    v !== null &&
    typeof (v as { type?: unknown }).type === 'string' &&
    'data' in v
  )
}
