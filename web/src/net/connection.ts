/**
 * The one WebSocket connection. Split plane per SPEC AMENDMENT A1:
 *   - TEXT frames  -> JSON control plane
 *   - BINARY frames -> data plane (decodeBinary)
 *
 * Responsibilities: connect, authenticate, reconnect with backoff, measure RTT,
 * and push bytes to the byte bus.
 *
 * B4: there is NO resume. On reconnect we re-subscribe, the server sends a fresh
 * snapshot, and the terminal resets and repaints. No ring, no replay, no seq
 * bookkeeping — `seq` is read for ordering/debugging only.
 *
 * It owns no UI state — everything observable lands in the store.
 */

import { BIN_GAP, BIN_SNAPSHOT, decodeBinary, encodeInput } from '../protocol/binary'
import type {
  AgentData,
  ClientFrame,
  ClosedData,
  GapData,
  PaneId,
  PongData,
  ResultData,
  ServerFrame,
  TreeData,
  ViewportData,
  ViewportMode,
  WelcomeData,
} from '../protocol/types'
import { MIN_COLS, MIN_ROWS } from '../protocol/types'
import { apply, getState, resetSession, setAuthRequired, setLink } from '../store/store'
import { pushBytes } from './byteBus'
import { streamUrl } from './auth'

export interface Transport {
  send(data: string | ArrayBuffer): void
  close(): void
  readonly readyState: number
  binaryType: string
  onopen: ((ev: unknown) => void) | null
  onclose: ((ev: { code?: number; reason?: string }) => void) | null
  onerror: ((ev: unknown) => void) | null
  onmessage: ((ev: { data: unknown }) => void) | null
}

export type TransportFactory = (url: string) => Transport

const PING_INTERVAL_MS = 4000
/** No pong within this window => the link is degraded regardless of RTT. */
const PONG_TIMEOUT_MS = 12_000
const BACKOFF_BASE_MS = 500
const BACKOFF_MAX_MS = 15_000

let ws: Transport | null = null
let factory: TransportFactory = (url) => new WebSocket(url) as unknown as Transport
let attempt = 0
let reconnectTimer: number | undefined
let pingTimer: number | undefined
let lastPongAt = 0
let stopped = false
let sessionId: string | undefined

/** Per-target streaming UTF-8 decoders so multi-byte chars are never split. */
const decoders = new Map<PaneId, TextDecoder>()
function decodeFor(target: PaneId, bytes: Uint8Array, reset: boolean): string {
  let d = decoders.get(target)
  if (!d || reset) {
    d = new TextDecoder('utf-8', { fatal: false })
    decoders.set(target, d)
  }
  return d.decode(bytes, { stream: true })
}

export function setTransportFactory(f: TransportFactory) {
  factory = f
}

export function connect(): void {
  stopped = false
  attempt = 0
  setAuthRequired(false)
  openSocket()
}

export function disconnect(): void {
  stopped = true
  clearTimeout(reconnectTimer)
  clearInterval(pingTimer)
  ws?.close()
  ws = null
  setLink('offline', 'closed by client')
}

function openSocket() {
  clearTimeout(reconnectTimer)
  if (ws) {
    ws.onclose = null
    ws.onmessage = null
    ws.onerror = null
    ws.onopen = null
    try {
      ws.close()
    } catch {
      /* already gone */
    }
  }

  setLink(attempt === 0 ? 'connecting' : 'reconnecting', null, attempt)
  const sock = factory(streamUrl())
  sock.binaryType = 'arraybuffer'
  ws = sock

  sock.onopen = () => {
    attempt = 0
    lastPongAt = Date.now()
    sendControl({
      seq: 0,
      type: 'hello',
      data: { protocol: 'v1', client: 'herdr-expose-web', session: sessionId },
    })
    startPings()
    // B4: no replay. Whatever we were rendering, ask for it again from scratch.
    for (const fn of reconnectHooks) fn()
  }

  sock.onmessage = (ev) => {
    const data = ev.data
    if (typeof data === 'string') handleControl(data)
    else if (data instanceof ArrayBuffer) handleBinary(data)
    else if (data instanceof Blob) {
      // Defensive: some transports hand back a Blob despite binaryType.
      void data.arrayBuffer().then(handleBinary)
    }
  }

  sock.onerror = () => {
    setLink(attempt === 0 ? 'connecting' : 'reconnecting', 'socket error')
  }

  sock.onclose = (ev) => {
    clearInterval(pingTimer)
    if (stopped) return
    // 1008 policy violation / 4401 / 4403 are "your credentials are the
    // problem". Retrying cannot fix that, and a reconnect loop against an auth
    // failure is just a log flood. Surface the pairing screen instead.
    if (ev?.code === 1008 || ev?.code === 4401 || ev?.code === 4403) {
      stopped = true
      setAuthRequired(true)
      setLink('offline', ev.reason || 'This device is not paired.')
      return
    }
    const reason = ev?.reason || (ev?.code === 1006 ? 'connection lost' : 'disconnected')
    scheduleReconnect(reason)
  }
}

function scheduleReconnect(reason: string) {
  attempt += 1
  // Exponential backoff with jitter, capped. SPEC §8.
  const delay = Math.min(BACKOFF_MAX_MS, BACKOFF_BASE_MS * 2 ** Math.min(attempt - 1, 6))
  const jittered = delay * (0.7 + Math.random() * 0.6)
  setLink('reconnecting', reason, attempt)
  reconnectTimer = window.setTimeout(openSocket, jittered)
}

function startPings() {
  clearInterval(pingTimer)
  pingTimer = window.setInterval(() => {
    if (!ws || ws.readyState !== 1) return
    sendControl({ seq: 0, type: 'ping', data: { t: Date.now() } })
    if (Date.now() - lastPongAt > PONG_TIMEOUT_MS) {
      // Do not pretend all is well (SPEC §8).
      setLink('degraded', 'no pong')
    }
  }, PING_INTERVAL_MS)
}

/* ------------------------------------------------------------------ */
/* Inbound                                                             */
/* ------------------------------------------------------------------ */

function handleControl(raw: string) {
  let msg: ServerFrame
  try {
    msg = JSON.parse(raw) as ServerFrame
  } catch {
    return
  }
  if (!msg || typeof msg.type !== 'string') return
  const seq = typeof msg.seq === 'number' ? msg.seq : getState().lastSeq

  switch (msg.type) {
    case 'welcome': {
      const d = msg.data as WelcomeData
      // A new server session invalidates every buffer we hold.
      if (d.session && sessionId && d.session !== sessionId) resetSession()
      sessionId = d.session ?? sessionId
      decoders.clear()
      apply.welcome(seq, d)
      break
    }
    case 'tree':
      apply.tree(seq, msg.data as TreeData)
      break
    case 'agent':
      apply.agent(seq, msg.data as AgentData)
      break
    case 'closed':
      apply.closed(seq, msg.data as ClosedData)
      break
    case 'gap': {
      const d = msg.data as GapData
      apply.gap(seq, d.target, d.bytes_dropped)
      break
    }
    case 'result':
      apply.result(seq, msg.data as ResultData)
      break
    case 'pong': {
      const d = msg.data as PongData
      lastPongAt = Date.now()
      apply.pong(seq, Math.max(0, Date.now() - (d?.t ?? Date.now())))
      break
    }
    default:
      // Unknown control types are ignored, never fatal (forward compat).
      break
  }
}

function handleBinary(buf: ArrayBuffer) {
  const m = decodeBinary(buf)
  if (!m) return
  if (m.type === BIN_GAP) {
    apply.gap(m.seq, m.target, m.bytesDropped)
    return
  }
  const isSnapshot = m.type === BIN_SNAPSHOT
  // Raw bytes to the live terminal, untouched. No decode on the hot path.
  pushBytes(m.target, m.payload, isSnapshot ? 'snapshot' : 'frame')
  // Decoded text only for tiles/digests.
  const text = decodeFor(m.target, m.payload, isSnapshot)
  if (isSnapshot) apply.snapshot(m.seq, m.target, text)
  else apply.frame(m.seq, m.target, text)
}

/* ------------------------------------------------------------------ */
/* Outbound                                                            */
/* ------------------------------------------------------------------ */

function sendControl(frame: ClientFrame) {
  if (!ws || ws.readyState !== 1) return
  ws.send(JSON.stringify(frame))
}

export function sendSubscribe(targets: PaneId[]) {
  if (targets.length === 0) return
  sendControl({ seq: 0, type: 'subscribe', data: { targets } })
}

export function sendUnsubscribe(targets: PaneId[]) {
  if (targets.length === 0) return
  sendControl({ seq: 0, type: 'unsubscribe', data: { targets } })
}

/**
 * SPEC §3: report what we actually RENDER. The server decides the real mode;
 * we never ask for `live` as a privilege.
 */
export function sendViewport(targets: Record<PaneId, ViewportMode>) {
  const data: ViewportData = { targets }
  sendControl({ seq: 0, type: 'viewport', data })
}

/**
 * B2 — geometry, per-connection, floored at 20x6. MUST be sent before the first
 * frame is requested for a target.
 */
export function sendResize(target: PaneId, cols: number, rows: number) {
  sendControl({
    seq: 0,
    type: 'resize',
    data: {
      target,
      cols: Math.max(MIN_COLS, Math.floor(cols)),
      rows: Math.max(MIN_ROWS, Math.floor(rows)),
    },
  })
}

/* --- input path ---------------------------------------------------- */

/**
 * B9 backpressure: over ~256KB buffered we drop WHEEL/scroll input but NEVER a
 * keystroke. Degrade scrolling, never typing.
 */
const BUFFER_LIMIT_BYTES = 256 * 1024

export function isCongested(): boolean {
  const amt = (ws as unknown as { bufferedAmount?: number } | null)?.bufferedAmount
  return typeof amt === 'number' && amt > BUFFER_LIMIT_BYTES
}

/**
 * B3/AMENDMENT 2 input coalescing: queueMicrotask drains BEFORE the task yields,
 * so a keystroke adds literally zero delay, while a wheel gesture's many events
 * merge into one frame. Never a timer on this path.
 */
const pending = new Map<PaneId, Uint8Array[]>()
let flushScheduled = false

function scheduleFlush() {
  if (flushScheduled) return
  flushScheduled = true
  queueMicrotask(() => {
    flushScheduled = false
    for (const [target, chunks] of pending) {
      if (chunks.length === 0) continue
      const total = chunks.reduce((n, c) => n + c.length, 0)
      const merged = new Uint8Array(total)
      let o = 0
      for (const c of chunks) {
        merged.set(c, o)
        o += c.length
      }
      ws?.send(encodeInput(target, merged))
    }
    pending.clear()
  })
}

/** Keystrokes: binary, no JSON encode on this path (AMENDMENT A1). Never dropped. */
export function sendInput(target: PaneId, bytes: Uint8Array) {
  if (!ws || ws.readyState !== 1 || bytes.length === 0) return
  const q = pending.get(target)
  if (q) q.push(bytes)
  else pending.set(target, [bytes])
  scheduleFlush()
}

/** Wheel / scroll reports. Dropped under congestion (B9). */
export function sendScrollInput(target: PaneId, bytes: Uint8Array) {
  if (isCongested()) return
  sendInput(target, bytes)
}

const textEncoder = new TextEncoder()
export function sendInputText(target: PaneId, text: string) {
  sendInput(target, textEncoder.encode(text))
}

/* --- reconnect hooks ----------------------------------------------- */

/**
 * B4: components register here to re-subscribe / reset / repaint when a new
 * socket opens. This replaces resume entirely.
 */
const reconnectHooks = new Set<() => void>()
export function onReconnected(fn: () => void): () => void {
  reconnectHooks.add(fn)
  return () => reconnectHooks.delete(fn)
}

let cmdSeq = 0
/**
 * `command` passes a Herdr socket method straight through (SPEC §3). We never
 * enumerate methods here — the caller names one.
 */
export function sendCommand(
  method: string,
  params?: Record<string, unknown>,
): Promise<ResultData> {
  const id = `c${++cmdSeq}-${Date.now().toString(36)}`
  sendControl({ seq: 0, type: 'command', data: { method, params, id } })
  return waitForResult(id)
}

function waitForResult(id: string, timeoutMs = 8000): Promise<ResultData> {
  return new Promise((resolve) => {
    const started = Date.now()
    const tick = () => {
      const r = getState().results[id]
      if (r) return resolve(r)
      if (Date.now() - started > timeoutMs)
        return resolve({ id, ok: false, error: 'timeout' })
      setTimeout(tick, 60)
    }
    tick()
  })
}
