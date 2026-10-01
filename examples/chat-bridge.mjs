#!/usr/bin/env node
/**
 * Drive your agents from a chat app. One bridge, any channel.
 *
 *   node examples/chat-bridge.mjs --channel console
 *   node examples/chat-bridge.mjs --channel telegram        # needs TELEGRAM_BOT_TOKEN
 *   node examples/chat-bridge.mjs --channel console --url http://HOST:PORT --code ABC123
 *
 * THE DESIGN DECISION, because the obvious one is wrong:
 *
 * Do NOT mirror a terminal into a chat channel. A pane emits constantly, a
 * 120x40 screen pasted every few seconds is unreadable, and the channel will
 * rate-limit you into a backlog that arrives after it stopped being true.
 *
 * So this bridge is a NOTIFIER with a terminal attached, not a terminal in a
 * chat window:
 *
 *   - an agent going `blocked` is pushed IMMEDIATELY, with its question. That
 *     is the event a human is actually waiting for.
 *   - everything else is a digest on a timer, and only when something changed.
 *   - the full screen is sent only when somebody asks for it (`/tail`).
 *
 * A CHANNEL is two functions, `poll()` and `send()`. That is the entire
 * contract — Telegram, Teams, Discord, Slack and a webhook all fit it, and
 * none of them needs to know anything about Herdr. Add one at the bottom.
 */

const args = Object.fromEntries(
  process.argv.slice(2).flatMap((a, i, all) =>
    a.startsWith('--') ? [[a.slice(2), all[i + 1]?.startsWith('--') ? true : (all[i + 1] ?? true)]] : [],
  ),
)

const BASE = (args.url ?? 'http://127.0.0.1:21118').replace(/\/$/, '')
const DIGEST_MS = Number(args.digest ?? 30000)

/* ------------------------------------------------------------------ channels */

/**
 * `console` exists so the bridge can be run and tested with no credentials,
 * no bot and no network beyond the local daemon. It is the same contract the
 * real channels implement, so a change proved here is proved for all of them.
 */
function consoleChannel() {
  const inbox = []
  process.stdin.setEncoding('utf8')
  process.stdin.on('data', (chunk) => {
    for (const line of chunk.split('\n')) if (line.trim()) inbox.push({ text: line.trim(), thread: 'stdin' })
  })
  return {
    name: 'console',
    async poll() { return inbox.splice(0) },
    async send(text) { console.log('\n' + text + '\n') },
  }
}

/**
 * Telegram. getUpdates long-polls with an offset; sendMessage replies.
 *
 * The token comes from the environment and is never written down, never
 * logged, and never put in a URL this program prints.
 */
function telegramChannel() {
  // Project-scoped name first, so this does not fight over a generic variable
  // with every other Telegram tool on the machine.
  const token = process.env.HERDR_EXPOSE_TELEGRAM_TOKEN ?? process.env.TELEGRAM_BOT_TOKEN
  if (!token) throw new Error('set HERDR_EXPOSE_TELEGRAM_TOKEN (or TELEGRAM_BOT_TOKEN)')
  const api = (m, q = '') => `https://api.telegram.org/bot${token}/${m}${q}`
  let offset = 0
  let chat = args.chat ? String(args.chat) : null
  let greeted = true

  return {
    name: 'telegram',
    async poll() {
      // timeout=25 is a LONG poll: the request parks until something arrives,
      // so this is not a 1Hz hammer on Telegram's API.
      const r = await fetch(api('getUpdates', `?timeout=25&offset=${offset}`)).catch(() => null)
      if (!r?.ok) return []
      const j = await r.json()
      const out = []
      for (const u of j.result ?? []) {
        offset = u.update_id + 1
        const msg = u.message ?? u.channel_post
        if (!msg?.text) continue
        // First person to speak owns the thread. The startup banner was sent
        // before any chat was known, so it went nowhere -- greet here instead,
        // which is also the first moment there is somebody to greet.
        if (!chat) { chat = String(msg.chat.id); greeted = false }
        out.push({ text: msg.text.trim(), thread: String(msg.chat.id), from: msg.from?.username })
      }
      return out
    },
    async send(text, thread) {
      const to = thread ?? chat
      if (!to) { console.error('[out] dropped (no chat bound yet)'); return }
      console.error(`[out] ${to}: ${text.slice(0, 60).replace(/\n/g, ' ')}`)
      await fetch(api('sendMessage'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        // 4096 is Telegram's hard limit; truncate rather than get a 400.
        body: JSON.stringify({ chat_id: to, text: text.slice(0, 4000), disable_web_page_preview: true }),
      }).catch(() => {})
    },
  }
}

const CHANNELS = { console: consoleChannel, telegram: telegramChannel }

/* -------------------------------------------------------------------- herdr */

function panesOf(node, out = []) {
  if (!node || typeof node !== 'object') return out
  for (const p of node.panes ?? []) out.push(p)
  for (const k of ['sessions', 'workspaces', 'tabs', 'children']) for (const c of node[k] ?? []) panesOf(c, out)
  return out
}

async function pairToken() {
  if (!args.code) return null
  const r = await fetch(`${BASE}/v1/pair`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code: args.code, name: `chat-bridge/${args.channel ?? 'console'}` }),
  })
  if (!r.ok) throw new Error(`pair failed: ${r.status}`)
  return (await r.json()).token
}

const make = CHANNELS[args.channel ?? 'console']
if (!make) throw new Error(`unknown --channel. have: ${Object.keys(CHANNELS).join(', ')}`)
const channel = make()

const tok = await pairToken()
const ws = new WebSocket(BASE.replace(/^http/, 'ws') + '/v1/stream',
  tok ? { headers: { Authorization: `Bearer ${tok}` } } : undefined)

let seq = 0
const send = (f) => ws.readyState === 1 && ws.send(JSON.stringify({ seq: ++seq, ...f }))
const call = (method, params) => send({ type: 'command', data: { id: `c-${seq + 1}`, method, params } })

/** pane id -> last state we told anyone about. The digest is a DIFF, not a dump. */
const lastState = new Map()
let panes = []
let following = null     // pane id this chat is driving
let tailUntil = 0        // epoch ms; while in the future, transcripts are forwarded
let muted = false
let dirty = false        // something changed since the last digest

const label = (p) => (p.agent?.state && p.agent.state !== 'unknown' ? p.agent.state : 'shell')

/*
 * Pane ids are session-qualified ("openjevx/w1:p1") and the pane half is only
 * unique WITHIN a session -- every session starts at w1. Dropping the session
 * to save width put two different panes on screen as "w1:p1", which is worse
 * than long: it is wrong. So the session comes back as soon as there is more
 * than one of them, and is dropped only when it cannot be ambiguous.
 */
let multiSession = false
const short = (id) => (multiSession ? id : id.split('/').pop())

function listing() {
  if (!panes.length) return 'No panes.'
  return panes
    .map((p, i) => `${String(i + 1).padStart(2)}. ${label(p).padEnd(7)} ${short(p.id)}  ${(p.title ?? '').slice(0, 34)}`)
    .join('\n')
}

ws.onopen = () => channel.send(
  `herdr bridge up on ${BASE}${tok ? '' : ' (local)'}\n` +
  `/ls  /follow N  /say ...  /key y  /tail 60  /mute  /help`)

ws.onmessage = (ev) => {
  if (typeof ev.data !== 'string') return
  const m = JSON.parse(ev.data)

  if (m.type === 'tree') {
    panes = panesOf(m.data)
    multiSession = new Set(panes.map((p) => p.id.split('/')[0])).size > 1
    for (const p of panes) {
      const now = label(p)
      const was = lastState.get(p.id)
      lastState.set(p.id, now)
      if (was === undefined || was === now) continue
      dirty = true
      /*
       * BLOCKED PREEMPTS THE TIMER. Waiting up to DIGEST_MS to tell somebody an
       * agent is stuck is the one delay that makes the whole bridge pointless:
       * the agent sits idle for exactly as long as the digest interval.
       */
      if (now === 'blocked' && !muted) {
        channel.send(`NEEDS YOU — ${short(p.id)}\n${(p.title ?? '').slice(0, 60)}\n\n/follow by id, then /key y`)
        send({ type: 'subscribe', data: { targets: { [p.id]: 'transcript' } } })
      }
    }
  }

  if (m.type === 'transcript') {
    const t = m.data.target
    // Only forward a screen when it was ASKED for: a blocked agent's question,
    // or an explicit /tail. Otherwise this becomes the mirror we refused to be.
    const wanted = Date.now() < tailUntil && t === following
    const isBlocked = lastState.get(t) === 'blocked'
    if (!muted && (wanted || isBlocked)) {
      const tail = m.data.text.trimEnd().split('\n').slice(-18).join('\n')
      channel.send(`${short(t)}\n\n${tail}`)
    }
  }
}

ws.onerror = (e) => channel.send(`socket error: ${e.message ?? e}`)
ws.onclose = () => channel.send('bridge disconnected')

/* ------------------------------------------------------------------ commands */

async function handle(text, thread) {
  const [cmd, ...rest] = text.split(/\s+/)
  const arg = rest.join(' ')
  const reply = (s) => channel.send(s, thread)

  switch (cmd) {
    case '/help':
      return reply('/ls  /follow N|id  /say TEXT  /key K  /tail SEC  /mute  /unmute  /who')
    case '/ls':
      return reply(listing())
    case '/who':
      return reply(following ? `following ${following}` : 'following nothing — /ls then /follow N')
    case '/follow': {
      const n = Number(arg)
      const p = Number.isFinite(n) && n >= 1 ? panes[n - 1] : panes.find((x) => x.id.endsWith(arg))
      if (!p) return reply(`no such pane. /ls first.`)
      following = p.id
      send({ type: 'subscribe', data: { targets: { [p.id]: 'transcript' } } })
      return reply(`following ${short(p.id)} (${label(p)}). /say to prompt it, /key to answer it.`)
    }
    case '/say':
      if (!following) return reply('/follow one first.')
      if (!arg) return reply('/say what?')
      call('agent.prompt', { pane_id: following, text: arg })
      return reply(`sent to ${short(following)}`)
    case '/key':
      if (!following) return reply('/follow one first.')
      call('agent.send_keys', { pane_id: following, keys: [arg || 'enter'] })
      return reply(`${arg || 'enter'} -> ${short(following)}`)
    case '/tail': {
      if (!following) return reply('/follow one first.')
      const secs = Math.min(Number(arg) || 30, 300)
      tailUntil = Date.now() + secs * 1000
      return reply(`mirroring ${short(following)} for ${secs}s`)
    }
    case '/mute': muted = true; return reply('muted')
    case '/unmute': muted = false; return reply('unmuted')
    default:
      // Bare text is a prompt for whatever you are following. That is the
      // thing people actually want to type, so it is not behind a command.
      if (!cmd.startsWith('/') && following) {
        call('agent.prompt', { pane_id: following, text })
        return reply(`sent to ${short(following)}`)
      }
      /*
       * Anything else, including the "hi" everybody opens with. Answering a
       * greeting with "unknown. /help" is a dead end -- the first message to a
       * bot is the moment to show what it can do, so this answers with the
       * actual panes rather than a menu about panes.
       */
      return reply(
        `herdr bridge — ${panes.length} pane(s)\n\n${listing()}\n\n` +
        `/follow N to pick one, then just type to prompt it.\n` +
        `/key y to answer a blocked agent · /tail 60 to watch · /help`)
  }
}

/* ----------------------------------------------------------------- the loops */

setInterval(async () => {
  for (const msg of await channel.poll()) {
    // stderr, always: a bridge you cannot see receiving is a bridge you cannot
    // debug. The token is never part of this line.
    console.error(`[in] ${msg.thread}${msg.from ? ' @' + msg.from : ''}: ${msg.text}`)
    try { await handle(msg.text, msg.thread) } catch (e) { channel.send(`error: ${e.message}`) }
  }
}, args.channel === 'telegram' ? 100 : 400)

// The digest. Quiet by construction: it sends nothing unless a state actually
// changed, so a machine full of idle panes produces no traffic at all.
setInterval(() => {
  if (!dirty || muted) return
  dirty = false
  const by = {}
  for (const s of lastState.values()) by[s] = (by[s] ?? 0) + 1
  const blocked = by.blocked ?? 0
  channel.send(
    (blocked ? `${blocked} need you · ` : '') +
    Object.entries(by).filter(([k]) => k !== 'blocked').map(([k, v]) => `${v} ${k}`).join(' · '))
}, DIGEST_MS)
