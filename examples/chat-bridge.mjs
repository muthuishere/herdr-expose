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
/*
 * ALL OF THE HARD PART IS IN THE PLUGIN.
 *
 * This file used to carry chrome patterns, a blank collapser, a fuzzy overlap
 * diff, a minimum-new-lines floor, a per-pane quiet timer and a reply window.
 * Every one of those is something the next adapter -- Teams, Discord, Slack --
 * would have written again and got wrong in its own way.
 *
 * They are one subscription now:
 *
 *   transcript_settled = prose text, delivered ONCE, when the agent stops.
 *
 * A working agent redraws constantly and none of it is an answer. The server
 * holds output while the agent is mid-turn and releases the screen when it
 * settles -- idle, done, or blocked on a question. So an adapter is back to
 * what it should always have been: show what arrives, send what is typed.
 */

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
    async send(text, _thread, opts) {
      // No buttons in a terminal, so choices degrade to the numbered list the
      // text commands already understand. Same contract, poorer renderer.
      const extra = opts?.choices?.length
        ? '\n' + opts.choices.map((c) => `  [${c.data}] ${c.label}`).join('\n')
        : ''
      console.log('\n' + text + extra + '\n')
    },
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

        // A tapped button. Telegram shows a spinner until it is answered, so
        // answer first and then treat the payload exactly like typed text --
        // which keeps ONE command path instead of two.
        if (u.callback_query) {
          const cb = u.callback_query
          fetch(api('answerCallbackQuery'), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ callback_query_id: cb.id }),
          }).catch(() => {})
          const cid = String(cb.message?.chat?.id ?? '')
          if (!chat) chat = cid
          out.push({ text: cb.data, thread: cid, from: cb.from?.username })
          continue
        }

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
    async send(text, thread, opts) {
      const to = thread ?? chat
      if (!to) { console.error('[out] dropped (no chat bound yet)'); return }
      console.error(`[out] ${to}: ${text.slice(0, 60).replace(/\n/g, ' ')}`)
      const body = {
        chat_id: to,
        // 4096 is Telegram's hard limit; truncate rather than get a 400.
        text: text.slice(0, 4000) || '\u2063',
        disable_web_page_preview: true,
      }
      if (opts?.choices?.length) {
        // Two per row: a phone shows a full label at that width, and one per
        // row turns nine panes into a screenful of scrolling.
        const rows = []
        for (let i = 0; i < opts.choices.length; i += 2) {
          rows.push(opts.choices.slice(i, i + 2).map((c) => ({
            // 64 bytes is Telegram's callback_data limit, and it is a HARD
            // error, not a truncation.
            text: c.label.slice(0, 40),
            callback_data: c.data.slice(0, 64),
          })))
        }
        body.reply_markup = { inline_keyboard: rows }
      }
      await fetch(api('sendMessage'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
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

/*
 * ONE BRIDGE AT A TIME, per channel.
 *
 * Two bridges on one Telegram token both long-poll getUpdates, both receive
 * the same update, and every command runs twice -- which looked exactly like a
 * code bug and cost an hour chasing one. A lock file keyed on the channel makes
 * it impossible rather than merely unlikely.
 *
 * A stale lock from a crashed run is reclaimed: the pid is checked, and only a
 * LIVE owner refuses. A lock nobody can clear is worse than no lock.
 */
import { openSync, writeSync, closeSync, readFileSync, unlinkSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const lockPath = join(tmpdir(), `herdr-chat-bridge.${args.channel ?? 'console'}.lock`)
function claimLock() {
  try {
    const fd = openSync(lockPath, 'wx')
    writeSync(fd, String(process.pid))
    closeSync(fd)
  } catch (e) {
    if (e.code !== 'EEXIST') throw e
    const owner = Number(readFileSync(lockPath, 'utf8').trim())
    try {
      process.kill(owner, 0)   // signal 0: does this pid exist?
      console.error(`another ${args.channel ?? 'console'} bridge is already running (pid ${owner}).`)
      console.error(`stop it first, or: kill ${owner}`)
      process.exit(1)
    } catch {
      console.error(`reclaiming stale lock from pid ${owner}`)
      unlinkSync(lockPath)
      return claimLock()
    }
  }
  const release = () => { try { unlinkSync(lockPath) } catch {} }
  process.on('exit', release)
  for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    process.on(sig, () => { release(); process.exit(0) })
  }
}
claimLock()

const make = CHANNELS[args.channel ?? 'console']
if (!make) throw new Error(`unknown --channel. have: ${Object.keys(CHANNELS).join(', ')}`)
const channel = make()

const tok = await pairToken()
const ws = new WebSocket(BASE.replace(/^http/, 'ws') + '/v1/stream',
  tok ? { headers: { Authorization: `Bearer ${tok}` } } : undefined)

let seq = 0
const send = (f) => ws.readyState === 1 && ws.send(JSON.stringify({ seq: ++seq, ...f }))
/*
 * The parameter is `target`, NOT `pane_id`.
 *
 * Both look equally plausible -- api.md uses pane_id for agent.read and
 * pane.send_text -- and the wrong one fails SILENTLY: the command is accepted,
 * nothing is delivered, and the only symptom is an answer that never arrives.
 * Herdr's own CLI takes <TARGET>, which is the thing to check against.
 */
const call = (method, params) => send({ type: 'command', data: { id: `c-${seq + 1}`, method, params } })

/** pane id -> last state we told anyone about. The digest is a DIFF, not a dump. */
const lastState = new Map()
let panes = []
let following = null     // pane id this chat is driving
let muted = false
let dirty = false        // something changed since the last digest

/* --- what has THIS reader already seen --------------------------------------
 *
 * The server decides whether a screen is worth sending: it strips furniture,
 * folds code, and holds everything until the agent stops and the screen stops
 * moving. What it cannot know is what this particular chat has already been
 * shown, because that is per-connection state and a server that keeps it is a
 * server growing a buffer per viewer.
 *
 * So the last mile is here. Measured on a live pane: two settled frames thirty
 * seconds apart, 5660 and 1872 characters, genuinely different screens -- the
 * server was right to send both -- but they opened with the same lines, so in
 * Telegram they read as the same answer arriving twice.
 */

const OVERLAP_SAME = 0.8
const OVERLAP_MIN = 3

/**
 * The lines at the end of `next` that were not already at the end of `prev`.
 *
 * The match is deliberately fuzzy. An exact comparison is useless against a
 * terminal: one repainted character -- a changed elapsed time, a moved cursor,
 * a re-rendered badge -- breaks the overlap, so the whole screen counts as new
 * and gets sent again. Eighty percent of the lines matching means it is the
 * same block of output with some of it repainted.
 *
 * Largest overlap first, so the answer is the LONGEST match rather than the
 * first coincidental one; a floor of three lines stops a pair of blanks
 * matching everything.
 */
function newLines(prev, next) {
  const n = next.split('\n')
  if (!prev) return n
  const o = prev.split('\n')
  for (let k = Math.min(o.length, n.length); k >= OVERLAP_MIN; k--) {
    const a = o.slice(o.length - k)
    const b = n.slice(0, k)
    let same = 0
    for (let i = 0; i < k; i++) if (a[i] === b[i]) same++
    if (same / k >= OVERLAP_SAME) return n.slice(k)
  }
  return n
}

/** target -> the full screen text we last showed for it. */
const shown = new Map()

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

const sessionOf = (id) => id.split('/')[0]
const sessions = () => [...new Set(panes.map((p) => sessionOf(p.id)))]

/** One line per pane, for a channel that cannot draw buttons. */
function listing() {
  if (!panes.length) return 'No panes.'
  return panes
    .map((p, i) => `${String(i + 1).padStart(2)}. ${label(p).padEnd(7)} ${short(p.id)}  ${(p.title ?? '').slice(0, 34)}`)
    .join('\n')
}

ws.onopen = () => channel.send(
  `herdr bridge up on ${BASE}${tok ? '' : ' (local)'}\n` +
  `/ls  /follow N  /say ...  /key y  /mute  /help`)

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
        channel.send(`NEEDS YOU — ${short(p.id)}\n${(p.title ?? '').slice(0, 60)}`,
          null, { choices: [{ label: 'open', data: `p:${p.id}` }] })
        send({ type: 'subscribe', data: { targets: { [p.id]: 'transcript_settled' } } })
      }
    }
  }

  // Log EVERY command result, not just failures. A command that is accepted
  // and does nothing looks identical to one that worked -- which is how the
  // wrong parameter name survived three rounds of testing.
  if (m.type === 'result') console.error(`[cmd] ${JSON.stringify(m.data).slice(0, 200)}`)

  if (m.type === 'transcript') {
    if (muted) return
    const t = m.data.target
    /*
     * Nothing reaches here that was not worth sending. The server held this
     * while the agent was mid-turn, released it when the agent stopped,
     * stripped the terminal's furniture and folded its code into markers.
     * There is nothing left for a chat adapter to decide.
     */
    const text = m.data.text.trimEnd()
    if (!text) return

    // Show only what this chat has not already been shown.
    const prev = shown.get(t)
    const fresh = newLines(prev, text).filter((l) => l.trim())
    shown.set(t, text)
    if (prev && fresh.length === 0) return

    const body = (prev ? fresh : text.split('\n')).slice(-40).join('\n')
    if (!body.trim()) return
    channel.send(`${short(t)}\n\n${body}`, null, { choices: paneActions(t) })
  }
}

ws.onerror = (e) => channel.send(`socket error: ${e.message ?? e}`)
ws.onclose = () => channel.send('bridge disconnected')

/* ------------------------------------------------------------------ commands */

/** Buttons offered once you are on a pane. */
function paneActions(id) {
  return [
    { label: 'y', data: `k:y` },
    { label: 'n', data: `k:n` },
    { label: 'enter', data: `k:enter` },
    { label: 'esc', data: `k:esc` },
    { label: 'back', data: `s:${sessionOf(id)}` },
  ]
}

/** The top of the menu: which session. One session skips straight to panes. */
function showSessions(reply) {
  const ss = sessions()
  if (ss.length === 0) return reply('No panes in view.')
  if (ss.length === 1) return showPanes(ss[0], reply)
  const counts = Object.fromEntries(ss.map((n) => [n, panes.filter((p) => sessionOf(p.id) === n)]))
  return reply(
    `${ss.length} sessions`,
    ss.map((n) => {
      const ps = counts[n]
      const blocked = ps.filter((p) => label(p) === 'blocked').length
      return { label: `${n} (${ps.length}${blocked ? ` · ${blocked}!` : ''})`, data: `s:${n}` }
    }))
}

/** The panes inside one session, as buttons. */
function showPanes(name, reply) {
  const ps = panes.filter((p) => sessionOf(p.id) === name)
  if (!ps.length) return reply(`${name} has no panes.`)
  return reply(
    `${name} — ${ps.length} pane(s)` + (sessions().length === 1 ? '\n(this is all the share can see)' : ''),
    ps.map((p) => ({
      label: `${label(p) === 'blocked' ? '! ' : ''}${short(p.id).replace(name + '/', '')} ${label(p)}`,
      data: `p:${p.id}`,
    })))
}

async function handle(text, thread) {
  const [cmd, ...rest] = text.split(/\s+/)
  const arg = rest.join(' ')
  const reply = (s, choices) => channel.send(s, thread, choices ? { choices } : undefined)

  // Button payloads. A tapped button and a typed command run the same code.
  if (text.startsWith('s:')) return showPanes(text.slice(2), reply)
  if (text.startsWith('k:')) return handle(`/key ${text.slice(2)}`, thread)
  if (text.startsWith('p:')) {
    const id = text.slice(2)
    if (!panes.some((p) => p.id === id)) return reply('that pane is gone. /ls')
    following = id
    shown.delete(id)   // a deliberate open shows the screen, not a diff of it
    send({ type: 'subscribe', data: { targets: { [id]: 'transcript_settled' } } })
    return reply(`following ${short(id)} — fetching its screen…`)
  }

  switch (cmd) {
    case '/help':
      return reply('/ls  /follow N|id  /say TEXT  /key K  /mute  /unmute  /who')
    case '/ls':
    case '/start':
      return showSessions(reply)
    case '/who':
      return reply(following ? `following ${following}` : 'following nothing — /ls then /follow N')
    case '/follow': {
      const n = Number(arg)
      const p = Number.isFinite(n) && n >= 1 ? panes[n - 1] : panes.find((x) => x.id.endsWith(arg))
      if (!p) return reply(`no such pane. /ls first.`)
      following = p.id
      send({ type: 'subscribe', data: { targets: { [p.id]: 'transcript_settled' } } })
      return reply(`following ${short(p.id)} (${label(p)}). /say to prompt it, /key to answer it.`)
    }
    case '/say':
      if (!following) return reply('/follow one first.')
      if (!arg) return reply('/say what?')
      call('agent.prompt', { target: following, text: arg })
      return reply(`sent to ${short(following)} — watching for the reply`)
    case '/key':
      if (!following) return reply('/follow one first.')
      call('agent.send_keys', { target: following, keys: [arg || 'enter'] })
      return reply(`${arg || 'enter'} -> ${short(following)}`)
    case '/mute': muted = true; return reply('muted')
    case '/unmute': muted = false; return reply('unmuted')
    default:
      // Bare text is a prompt for whatever you are following. That is the
      // thing people actually want to type, so it is not behind a command.
      if (!cmd.startsWith('/') && following) {
        call('agent.prompt', { target: following, text })
          return reply(`sent to ${short(following)} — watching for the reply`)
      }
      /*
       * Anything else, including the "hi" everybody opens with. Answering a
       * greeting with "unknown. /help" is a dead end -- the first message to a
       * bot is the moment to show what it can do, so this answers with the
       * actual panes rather than a menu about panes.
       */
      return showSessions(reply)
  }
}

/* ----------------------------------------------------------------- the loops */

/*
 * A SEQUENTIAL loop, not setInterval. This matters and it bit:
 *
 * Telegram's getUpdates is a LONG poll -- the request parks for up to 25s
 * waiting for something to arrive. setInterval does not wait for an async body,
 * so a 100ms timer fired ~250 overlapping requests while the first was still
 * parked. Every one of them carried the SAME offset, because offset only
 * advances when a response is handled, so Telegram handed the same update to
 * each of them. One "/follow 1" became three, and one typed prompt was
 * delivered to a live agent three times.
 *
 * Awaiting the poll before scheduling the next one makes overlap impossible by
 * construction rather than by a guard flag somebody can forget.
 */
async function pump() {
  for (;;) {
    try {
      for (const msg of await channel.poll()) {
        // stderr, always: a bridge you cannot see receiving is a bridge you
        // cannot debug. The token is never part of this line.
        console.error(`[in] ${msg.thread}${msg.from ? ' @' + msg.from : ''}: ${msg.text}`)
        try { await handle(msg.text, msg.thread) } catch (e) { channel.send(`error: ${e.message}`) }
      }
    } catch (e) {
      console.error('[poll] ' + (e.message ?? e))
    }
    // A long-polling channel returns only when it has something or it timed
    // out, so it needs no delay; a local one would spin without this.
    if (args.channel !== 'telegram') await new Promise((r) => setTimeout(r, 400))
  }
}
pump()

// The digest. Quiet by construction: it sends nothing unless a state actually
// changed, so a machine full of idle panes produces no traffic at all.
/*
 * --digest 0 means OFF.
 *
 * It used to mean setInterval(fn, 0), which is a tick every event loop turn --
 * so asking for silence produced the most traffic the thing could emit. The
 * guard is here rather than in the caller because a zero interval is a
 * perfectly ordinary thing to type and must not be a footgun.
 */
if (DIGEST_MS > 0) setInterval(() => {
  if (!dirty || muted) return
  dirty = false
  const by = {}
  for (const s of lastState.values()) by[s] = (by[s] ?? 0) + 1
  const blocked = by.blocked ?? 0
  channel.send(
    (blocked ? `${blocked} need you · ` : '') +
    Object.entries(by).filter(([k]) => k !== 'blocked').map(([k, v]) => `${v} ${k}`).join(' · '))
}, DIGEST_MS)
