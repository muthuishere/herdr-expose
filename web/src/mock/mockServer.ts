/**
 * MOCK SERVER — implements SPEC §3 as amended by A1 (split plane) over a fake
 * WebSocket. Enabled with VITE_MOCK=1.
 *
 * It speaks the REAL wire format: JSON in "text" messages, and genuine
 * ArrayBuffer binary messages with the fixed A1 header. Swapping to the Go
 * server is a transport swap only — zero component changes.
 *
 * It deliberately misbehaves a little (variable RTT, an occasional gap, agents
 * that block) so the degraded/blocked paths get exercised in dev.
 */

import {
  BIN_FRAME,
  BIN_GAP,
  BIN_SNAPSHOT,
  encodeServerBinary,
  encodeU64,
} from '../protocol/binary'
import type {
  AgentState,
  PaneId,
  TreeData,
  TreePane,
  ViewportMode,
} from '../protocol/types'
import type { Transport, TransportFactory } from '../net/connection'

/* ------------------------------------------------------------------ */
/* Fixtures                                                            */
/* ------------------------------------------------------------------ */

const C = {
  reset: '\x1b[0m',
  dim: '\x1b[2m',
  bold: '\x1b[1m',
  red: '\x1b[31m',
  green: '\x1b[32m',
  yellow: '\x1b[33m',
  blue: '\x1b[34m',
  magenta: '\x1b[35m',
  cyan: '\x1b[36m',
  grey: '\x1b[90m',
}

interface MockPane extends TreePane {
  /** Lines this pane emits, cycled. */
  script: string[]
  cursor: number
  /** ms between emissions; 0 = silent. */
  cadence: number
  detection?: string
}

function pane(p: Partial<MockPane> & { id: string; title: string }): MockPane {
  return {
    command: 'zsh',
    cwd: '~/muthu/gitworkspace/herdr-plugins/herdr-expose',
    script: [],
    cursor: 0,
    cadence: 0,
    cols: 100,
    rows: 30,
    ...p,
  }
}

const CLAUDE_SCRIPT = [
  `${C.magenta}✳${C.reset} ${C.bold}Exploring${C.reset} ${C.grey}(esc to interrupt)${C.reset}`,
  `  ${C.grey}⎿${C.reset} Read ${C.cyan}internal/upstream/stream.go${C.reset} ${C.grey}(214 lines)${C.reset}`,
  `  ${C.grey}⎿${C.reset} Grep ${C.cyan}"terminal.closed"${C.reset} ${C.grey}— 3 matches${C.reset}`,
  `${C.green}●${C.reset} The NDJSON reader owns the base64 decode, so everything`,
  `  downstream is ${C.bold}[]byte${C.reset}. That is the only decode in the system.`,
  `  ${C.grey}⎿${C.reset} Edit ${C.cyan}internal/core/hub.go${C.reset} ${C.green}+34${C.reset} ${C.red}-12${C.reset}`,
  `${C.yellow}⚒${C.reset} Running ${C.cyan}go test ./internal/...${C.reset}`,
  `  ${C.grey}ok${C.reset}  	github.com/muthuishere/herdr-expose/internal/core	0.412s`,
  `  ${C.grey}ok${C.reset}  	github.com/muthuishere/herdr-expose/internal/upstream	1.007s`,
]

const BUILD_SCRIPT = [
  `${C.grey}$${C.reset} go build ./...`,
  `${C.grey}$${C.reset} ${C.dim}vite build${C.reset}`,
  `${C.green}✓${C.reset} 48 modules transformed.`,
  `dist/index.html                  ${C.grey}0.62 kB${C.reset}`,
  `dist/assets/index-a91f2c.css     ${C.grey}8.41 kB │ gzip: 2.31 kB${C.reset}`,
  `dist/assets/index-7d0c31.js    ${C.grey}168.22 kB │ gzip: 54.09 kB${C.reset}`,
  `${C.green}✓${C.reset} built in ${C.bold}1.42s${C.reset}`,
]

const TEST_SCRIPT = [
  `${C.cyan}RUN${C.reset}  TestHubFanout`,
  `${C.green}PASS${C.reset} TestHubFanout ${C.grey}(0.00s)${C.reset}`,
  `${C.cyan}RUN${C.reset}  TestCoalesce16ms`,
  `${C.green}PASS${C.reset} TestCoalesce16ms ${C.grey}(0.02s)${C.reset}`,
  `${C.cyan}RUN${C.reset}  TestGapThenSnapshot`,
  `${C.red}FAIL${C.reset} TestGapThenSnapshot ${C.grey}(0.01s)${C.reset}`,
  `    hub_test.go:88: expected snapshot after gap, got frame`,
]

const LOG_SCRIPT = [
  `${C.grey}12:04:11${C.reset} ${C.blue}INFO${C.reset}  upstream connected socket=/tmp/herdr.sock`,
  `${C.grey}12:04:11${C.reset} ${C.blue}INFO${C.reset}  subscribed events=26 protocol=22`,
  `${C.grey}12:04:12${C.reset} ${C.blue}INFO${C.reset}  serve listen=127.0.0.1:21118`,
  `${C.grey}12:04:19${C.reset} ${C.yellow}WARN${C.reset}  client slow queue=64KB target=pane-3`,
  `${C.grey}12:04:19${C.reset} ${C.blue}INFO${C.reset}  gap emitted target=pane-3 bytes=18432`,
]

const PANES: MockPane[] = [
  pane({
    id: 'pane-1',
    terminal_id: 'term-a1',
    title: 'go-core',
    command: 'claude',
    script: CLAUDE_SCRIPT,
    cadence: 700,
    agent: { id: 'agent-1', kind: 'claude', state: 'working', summary: 'Editing internal/core/hub.go' },
  }),
  pane({
    id: 'pane-2',
    terminal_id: 'term-a2',
    title: 'web',
    command: 'claude',
    script: BUILD_SCRIPT,
    cadence: 1400,
    agent: { id: 'agent-2', kind: 'claude', state: 'done', summary: 'Built dist in 1.42s' },
  }),
  pane({
    id: 'pane-3',
    terminal_id: 'term-a3',
    title: 'expose',
    command: 'codex',
    script: TEST_SCRIPT,
    cadence: 900,
    agent: {
      id: 'agent-3',
      kind: 'codex',
      state: 'blocked',
      summary: 'Waiting: overwrite adapters/ngrok.js?',
    },
    detection: [
      `${C.bold}adapters/ngrok.js${C.reset} already exists and has local changes.`,
      ``,
      `  ${C.yellow}1${C.reset}) Overwrite it`,
      `  ${C.yellow}2${C.reset}) Keep mine, write ngrok.new.js`,
      `  ${C.yellow}3${C.reset}) Show me the diff first`,
      ``,
      `${C.cyan}?${C.reset} Overwrite adapters/ngrok.js? ${C.grey}(y/N)${C.reset} `,
    ].join('\r\n'),
  }),
  pane({
    id: 'pane-4',
    terminal_id: 'term-a4',
    title: 'server log',
    command: 'tail -f',
    script: LOG_SCRIPT,
    cadence: 2200,
  }),
  pane({
    id: 'pane-5',
    terminal_id: 'term-b1',
    title: 'shell',
    cwd: '~/muthu/deemwarworkspace',
    script: [`${C.grey}$${C.reset} `],
    cadence: 0,
  }),
  pane({
    id: 'pane-6',
    terminal_id: 'term-b2',
    title: 'docs',
    command: 'claude',
    script: [`${C.grey}●${C.reset} Idle. Waiting for a prompt.`],
    cadence: 0,
    agent: { id: 'agent-4', kind: 'claude', state: 'idle', summary: 'Idle' },
  }),
]

function buildTree(rev: number): TreeData {
  return {
    revision: rev,
    focused: null,
    workspaces: [
      {
        id: 'ws-1',
        title: 'herdr-expose',
        tabs: [
          { id: 'tab-1', title: 'build', panes: [strip(PANES[0]), strip(PANES[1])] },
          { id: 'tab-2', title: 'verify', panes: [strip(PANES[2]), strip(PANES[3])] },
        ],
      },
      {
        id: 'ws-2',
        title: 'deemwar',
        tabs: [{ id: 'tab-3', title: 'misc', panes: [strip(PANES[4]), strip(PANES[5])] }],
      },
    ],
  }
}

function strip(p: MockPane): TreePane {
  const { script: _s, cursor: _c, cadence: _d, detection: _t, ...rest } = p
  void _s
  void _c
  void _d
  void _t
  return { ...rest }
}

/* ------------------------------------------------------------------ */
/* The fake socket                                                     */
/* ------------------------------------------------------------------ */

const enc = new TextEncoder()

class MockSocket implements Transport {
  readyState = 0
  binaryType = 'blob'
  onopen: ((ev: unknown) => void) | null = null
  onclose: ((ev: { code?: number; reason?: string }) => void) | null = null
  onerror: ((ev: unknown) => void) | null = null
  onmessage: ((ev: { data: unknown }) => void) | null = null

  private seq = 1
  private rev = 1
  private timers: number[] = []
  private modes: Record<PaneId, ViewportMode> = {}
  private closedFlag = false
  private session = `mock-${Math.random().toString(36).slice(2, 8)}`

  constructor() {
    setTimeout(() => {
      this.readyState = 1
      this.onopen?.({})
    }, 120)
  }

  /* --- outbound helpers --- */

  private text(type: string, data: unknown) {
    if (this.closedFlag) return
    this.onmessage?.({ data: JSON.stringify({ seq: this.seq++, type, data }) })
  }

  private bin(type: number, target: string, payload: Uint8Array) {
    if (this.closedFlag) return
    this.onmessage?.({ data: encodeServerBinary(type, this.seq++, target, payload) })
  }

  private every(ms: number, fn: () => void) {
    this.timers.push(window.setInterval(fn, ms))
  }

  /* --- inbound --- */

  send(data: string | ArrayBuffer): void {
    if (typeof data !== 'string') {
      this.handleInput(data)
      return
    }
    let msg: { type?: string; data?: Record<string, unknown> }
    try {
      msg = JSON.parse(data)
    } catch {
      return
    }
    switch (msg.type) {
      case 'hello':
        this.onHello()
        break
      case 'ping':
        // Variable latency so the degraded indicator has something to show.
        setTimeout(
          () => this.text('pong', { t: msg.data?.t, upstream_ok: true }),
          18 + Math.random() * (Math.random() < 0.12 ? 900 : 90),
        )
        break
      case 'viewport': {
        const t = (msg.data?.targets ?? {}) as Record<PaneId, ViewportMode>
        // The SERVER decides; the mock honours the client's declaration as-is,
        // which is the simplest policy that is still correct.
        this.modes = t
        for (const [id, mode] of Object.entries(t)) {
          if (mode !== 'none') this.snapshot(id)
        }
        this.text('tree', this.treeWithModes())
        break
      }
      case 'command':
        setTimeout(
          () =>
            this.text('result', {
              id: msg.data?.id,
              ok: true,
              result: { mock: true, method: msg.data?.method },
            }),
          60,
        )
        break
      default:
        break
    }
  }

  close(): void {
    this.closedFlag = true
    this.readyState = 3
    for (const t of this.timers) clearInterval(t)
    this.timers = []
    this.onclose?.({ code: 1000, reason: 'mock closed' })
  }

  /* --- behaviour --- */

  private onHello() {
    this.text('welcome', {
      protocol: 'v1',
      herdr_version: '0.9.0',
      server_version: 'herdr-expose-mock',
      session: this.session,
      ring_bytes: 262144,
    })
    this.text('tree', buildTree(this.rev++))
    // Initial agent frames, so a pane that is ALREADY blocked carries its
    // detection text before any state churn. The real server must do the same:
    // the tree alone has no detection field.
    for (const p of PANES) if (p.agent) this.emitAgent(p)

    for (const p of PANES) {
      if (p.cadence > 0) this.every(p.cadence, () => this.tick(p))
    }
    // Agent state churn, so the urgency ordering is visible without a server.
    this.every(9000, () => this.churnAgents())
    // An occasional gap on the noisiest pane, then a snapshot (SPEC §3).
    this.every(21000, () => {
      const dropped = 4096 + Math.floor(Math.random() * 30000)
      this.bin(BIN_GAP, 'pane-1', encodeU64(dropped))
      setTimeout(() => this.snapshot('pane-1'), 200)
    })
  }

  private handleInput(buf: ArrayBuffer) {
    // [type=16][tlen u16][target][raw bytes]
    const dv = new DataView(buf)
    if (dv.getUint8(0) !== 16) return
    const tlen = dv.getUint16(1, false)
    let target = ''
    for (let i = 0; i < tlen; i++) target += String.fromCharCode(dv.getUint8(3 + i))
    const bytes = new Uint8Array(buf, 3 + tlen)
    const p = PANES.find((x) => x.id === target)
    if (!p) return

    // Echo like a pty would, then unblock if this pane was waiting on an answer.
    const s = new TextDecoder().decode(bytes)
    const printable = s.replace(/[\x00-\x1f\x7f]/g, (ch) =>
      ch === '\r' ? '\r\n' : ch === '\x1b' ? '' : '',
    )
    if (printable) this.bin(BIN_FRAME, target, enc.encode(printable))

    if (p.agent?.state === 'blocked' && /[\r\ny1-3]/i.test(s)) {
      p.agent = { ...p.agent, state: 'working', summary: 'Resumed after your answer' }
      p.detection = undefined
      this.bin(
        BIN_FRAME,
        target,
        enc.encode(`\r\n${C.green}●${C.reset} Got it — continuing.\r\n`),
      )
      this.emitAgent(p)
      this.text('tree', this.treeWithModes())
    }
  }

  private tick(p: MockPane) {
    const mode = this.modes[p.id] ?? 'none'
    if (mode === 'none') return
    // SUMMARY is a repaint at 1-2Hz; LIVE is an append stream (SPEC §3).
    if (mode === 'summary') {
      if (Math.random() < 0.5) return
      p.cursor = (p.cursor + 1) % p.script.length
      this.snapshot(p.id)
      return
    }
    const line = p.script[p.cursor % p.script.length]
    p.cursor++
    this.bin(BIN_FRAME, p.id, enc.encode(line + '\r\n'))
  }

  /** A SUMMARY snapshot = the visible region, a full repaint. */
  private snapshot(id: PaneId) {
    const p = PANES.find((x) => x.id === id)
    if (!p) return
    const rows = 12
    const out: string[] = []
    for (let i = 0; i < rows; i++) {
      const idx = p.cursor - rows + i
      out.push(idx < 0 ? '' : p.script[((idx % p.script.length) + p.script.length) % p.script.length])
    }
    this.bin(BIN_SNAPSHOT, id, enc.encode(out.join('\r\n') + '\r\n'))
  }

  private churnAgents() {
    const withAgents = PANES.filter((p) => p.agent)
    const p = withAgents[Math.floor(Math.random() * withAgents.length)]
    if (!p?.agent) return
    const cycle: AgentState[] = ['working', 'done', 'idle', 'blocked']
    const next = cycle[Math.floor(Math.random() * cycle.length)]
    if (next === p.agent.state) return
    p.agent = {
      ...p.agent,
      state: next,
      summary:
        next === 'blocked'
          ? 'Waiting on you'
          : next === 'done'
            ? 'Finished — unseen'
            : next === 'working'
              ? 'Working…'
              : 'Idle',
      changed_at: Date.now(),
    }
    if (next === 'blocked' && !p.detection) {
      p.detection = [
        `${C.bold}${p.title}${C.reset} needs a decision.`,
        ``,
        `${C.cyan}?${C.reset} Apply the change and continue? ${C.grey}(y/n)${C.reset} `,
      ].join('\r\n')
    }
    this.emitAgent(p)
    this.text('tree', this.treeWithModes())
  }

  private emitAgent(p: MockPane) {
    if (!p.agent) return
    this.text('agent', {
      target: p.id,
      id: p.agent.id,
      kind: p.agent.kind,
      state: p.agent.state,
      summary: p.agent.summary,
      changed_at: p.agent.changed_at ?? Date.now(),
      detection: p.agent.state === 'blocked' ? p.detection : undefined,
    })
  }

  private treeWithModes(): TreeData {
    const t = buildTree(this.rev++)
    for (const ws of t.workspaces)
      for (const tab of ws.tabs)
        for (const p of tab.panes) p.mode = this.modes[p.id] ?? 'none'
    return t
  }
}

export const mockTransportFactory: TransportFactory = () => new MockSocket()

export function isMockEnabled(): boolean {
  return import.meta.env.VITE_MOCK === '1' || import.meta.env.VITE_MOCK === 'true'
}
