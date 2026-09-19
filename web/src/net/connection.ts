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
  GeometryData,
  PaneId,
  PongData,
  ResultData,
  ServerFrame,
  TranscriptData,
  TreeData,
  ViewportData,
  ViewportMode,
  WelcomeData,
} from '../protocol/types'
import { MIN_COLS, MIN_ROWS } from '../protocol/types'
import {
  apply,
  getState,
  resetSession,
  setAuthRequired,
  setLink,
  setUnreachable,
} from '../store/store'
import { pushBytes, pushGap } from './byteBus'
import { streamUrl } from './auth'
import { probeConfig } from './config'
import { countBinary, countEvent, recordViewport } from './debugStats'

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
/**
 * Blind retries are capped. Past this we still cannot reach the server and a
 * counter climbing to 40 tells the user nothing they can act on — show them an
 * actionable state with a Retry instead.
 */
const MAX_BLIND_ATTEMPTS = 8

const NOT_PAIRED_MSG = 'This device is not paired with herdr-expose.'
const LOST_PAIRING_MSG =
  'This device is no longer paired — its token was revoked, expired, or the ' +
  'server was restarted with new keys. Pair it again.'
const UNREACHABLE_MSG = "Can't reach herdr-expose — is it still running?"

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

/**
 * Generation counter. Every async probe captures it and drops its result if a
 * newer connect()/classification has started — otherwise a slow probe from an
 * abandoned attempt can resurrect a socket or a pairing screen after the fact.
 */
let generation = 0

/**
 * Boot order matters: ASK /v1/config BEFORE opening the socket.
 *
 * The server 401s an unauthenticated upgrade before it upgrades, and the
 * browser cannot see that status — so a socket is the wrong instrument for
 * "am I allowed in?". Ask over HTTP, where the answer is readable.
 */
export function connect(): void {
  stopped = false
  attempt = 0
  setUnreachable(false)
  setAuthRequired(false)
  setLink('connecting', null, 0)
  void openAfterProbe()
}

async function openAfterProbe(): Promise<void> {
  const mine = ++generation
  const probe = await probeConfig()
  if (stopped || mine !== generation) return
  if (probe.ok && probe.needsPairing) {
    // Do not open the socket at all: it would 401 and we would learn nothing.
    requirePairing(NOT_PAIRED_MSG)
    return
  }
  // A failed probe is a NETWORK fact, never an auth one. Try the socket — in
  // mock/dev there may be no /v1/config at all, and the close path classifies.
  openSocket()
}

function requirePairing(message: string): void {
  stopped = true
  clearTimeout(reconnectTimer)
  clearInterval(pingTimer)
  attempt = 0
  setAuthRequired(true, message)
  setLink('offline', message)
}

/** User-driven "try again" from the unreachable state. */
export function retryNow(): void {
  connect()
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
      requirePairing(ev.reason || LOST_PAIRING_MSG)
      return
    }
    const reason = ev?.reason || (ev?.code === 1006 ? 'connection lost' : 'disconnected')
    // 1006 tells us nothing: it is what a pre-upgrade 401 looks like AND what a
    // dropped wifi looks like. Ask /v1/config which of the two it is.
    void classifyThenReconnect(reason)
  }
}

/**
 * Classify the failure instead of blindly retrying it. A revoked device, an
 * expired token or a server token reset all look exactly like a network blip
 * from inside the WebSocket API — /v1/config is what tells them apart.
 */
async function classifyThenReconnect(reason: string): Promise<void> {
  const mine = ++generation
  setLink(attempt === 0 ? 'connecting' : 'reconnecting', reason, attempt)
  const probe = await probeConfig()
  if (stopped || mine !== generation) return
  if (probe.ok && probe.needsPairing) {
    requirePairing(LOST_PAIRING_MSG)
    return
  }
  scheduleReconnect(reason)
}

function scheduleReconnect(reason: string) {
  attempt += 1
  if (attempt > MAX_BLIND_ATTEMPTS) {
    // Stop the spinner. The server is reachable-by-DNS but not answering.
    stopped = true
    setUnreachable(true)
    setLink('offline', UNREACHABLE_MSG)
    return
  }
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
    case 'transcript':
      // Control plane, deliberately: a transcript is stripped text, not
      // terminal bytes, and must never reach the byte bus (SPEC J3).
      apply.transcript(seq, msg.data as TranscriptData)
      break
    case 'geometry':
      // The size the server ATTACHED at. The client renders to it; it does not
      // choose it. See sendResize's comment for why that inversion exists.
      apply.geometry(seq, msg.data as GeometryData)
      break
    case 'closed':
      apply.closed(seq, msg.data as ClosedData)
      break
    case 'gap': {
      const d = msg.data as GapData
      apply.gap(seq, d.target, d.bytes_dropped)
      pushGap(d.target)
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
    countBinary(m.target, 'gap', 0)
    apply.gap(m.seq, m.target, m.bytesDropped)
    pushGap(m.target)
    return
  }
  const isSnapshot = m.type === BIN_SNAPSHOT
  countBinary(m.target, isSnapshot ? 'snapshot' : 'frame', m.payload.length)
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
  for (const t of targets) countEvent('subscribe', t)
  sendControl({ seq: 0, type: 'subscribe', data: { targets } })
}

export function sendUnsubscribe(targets: PaneId[]) {
  if (targets.length === 0) return
  for (const t of targets) countEvent('unsubscribe', t)
  sendControl({ seq: 0, type: 'unsubscribe', data: { targets } })
}

/**
 * SPEC §3: report what we actually RENDER. The server decides the real mode;
 * we never ask for `live` as a privilege.
 */
export function sendViewport(targets: Record<PaneId, ViewportMode>) {
  countEvent('viewportSent')
  recordViewport(targets)
  const data: ViewportData = { targets }
  sendControl({ seq: 0, type: 'viewport', data })
}

/**
 * RESIZE IS A MUTATION. Do not send it just because a pane is on screen.
 *
 * Herdr gives a pane ONE size, shared by everyone attached to it, so fitting a
 * pane to this browser window resizes it on the owner's laptop too — their
 * agent gets a SIGWINCH, throws its screen away and redraws under their hands.
 * B2's "send geometry before the first frame" is withdrawn: the server now
 * attaches with no geometry at all and reports the pane's own size back in a
 * `geometry` frame, which is what we render to.
 *
 * Call this ONLY from an explicit, confirmed user action.
 */
export function sendResize(target: PaneId, cols: number, rows: number) {
  countEvent('resizeSent', target)
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

/**
 * Give a pane its geometry back: the server re-attaches with no --cols/--rows,
 * so herdr uses the pane's own size again. This is the default state; `match`
 * exists so a client can UNDO an explicit fit without guessing a number.
 */
export function sendMatchGeometry(target: PaneId) {
  countEvent('matchSent', target)
  sendControl({ seq: 0, type: 'resize', data: { target, cols: 0, rows: 0, match: true } })
}

/**
 * Ask for a guaranteed full repaint of a live target.
 *
 * Only the CLIENT can measure that its own grid has come out of alignment
 * (see terminal/renderHealth.ts), so only the client can ask for the repair.
 * Rate limiting lives in the watchdog: a repaint loop is indistinguishable
 * from flicker, and the server does not throttle this for us.
 */
export function sendRepaint(target: PaneId) {
  sendControl({ seq: 0, type: 'repaint', data: { target } })
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
