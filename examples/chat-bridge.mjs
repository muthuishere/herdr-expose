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
 * The floor between two screen sends for the SAME pane.
 *
 * A followed pane produces a transcript frame whenever its screen changes,
 * which for a working agent is constantly. Forwarding each one turns the chat
 * into the mirror this bridge exists not to be, and Telegram will rate-limit
 * you into a backlog that arrives after it stopped being true.
 */
const QUIET_MS = Number(args.quiet ?? 30000)
/*
 * How long after you SEND something the bridge stays eager.
 *
 * Asking a question and getting nothing back is the failure that makes the
 * whole thing feel broken, and the quiet gate caused it: a prompt went out,
 * the agent answered, and the answer was suppressed because a screen had been
 * sent moments earlier when the pane was opened.
 *
 * Sending implies wanting the reply, so a prompt or a keypress opens a window
 * in which the next CHANGED screen comes straight through. Change detection
 * still applies -- this never resends an unchanged screen -- and once the
 * window closes the 30s floor governs again.
 */
const REPLY_MS = Number(args.reply ?? 120000)
/*
 * How many NEW lines a screen must have gained before it is worth sending.
 *
 * "It changed" is far too twitchy for a terminal: a spinner frame, a token
 * counter ticking, a clock in a status bar -- all of those change the screen
 * every second while saying nothing. Counting the lines that are actually new
 * is the difference between "something happened" and "a character moved".
 */
const MIN_NEW_LINES = Number(args.minlines ?? 10)

/*
 * TUI chrome: everything a terminal draws that is furniture, not content.
 *
 * This matters twice over. It is unreadable on a phone -- a 119-character
 * divider wraps into six lines of nothing -- and, worse, it CHANGES. A token
 * counter ticking from 550K to 551K, a spinner, a clock: each one makes the
 * screen "new" and would push a notification that carries no information.
 *
 * So chrome is stripped BEFORE anything is compared, which is the only order
 * that works: strip after comparing and the counter still votes.
 */
const CHROME = [
  /^[\s\u2500-\u257f_=\-\u2014\u2013]{8,}$/,          // rules and box drawing
  // The input line. U+276F / U+203A are the TUI's own prompt carets, and what
  // follows is what somebody is TYPING, not what the agent said. ASCII '>' is
  // deliberately not here: it is a quote marker in real output.
  /^\s*[\u276f\u203a]/,
  /ctx\s+[\d.]+[KM]?\s+\d+%/i,                          // context meter
  /\b(auto mode|bypass permissions|accept edits)\b.*\(/i, // mode banners
  /shift\+tab to cycle/i,
  /^\s*Image in clipboard/i,
  /^\s*\u2191\s*\d+\s*(tokens|lines)/i,
]

/** Strip chrome and collapse blank runs. Content only. */
function clean(text) {
  const out = []
  for (const raw of text.split('\n')) {
    const line = raw.replace(/[ \t]+$/, '')
    if (CHROME.some((re) => re.test(line))) continue
    if (line === '') {
      if (out.length && out[out.length - 1] === '') continue
      out.push('')
      continue
    }
    out.push(line)
  }
  while (out.length && out[0] === '') out.shift()
  while (out.length && out[out.length - 1] === '') out.pop()
  return out.join('\n')
}

/**
 * How many lines at the end of `next` were not already in `prev`.
 *
 * A terminal SCROLLS, so the new screen is usually the old one shifted up with
 * fresh lines at the bottom. Finding the longest overlap between the tail of
 * the old and the head of the new gives the genuinely new remainder; with no
 * overlap at all the screen was replaced wholesale, which is as new as it
 * gets.
 */
const OVERLAP_SAME = Number(args.overlap ?? 0.8)
const OVERLAP_MIN = 3

function newLines(prev, next) {
  const n = next.split('\n')
  if (!prev) return n
  const o = prev.split('\n')

  /*
   * Find the largest block at the end of the old screen that is ALSO the start
   * of the new one, and call everything after it new.
   *
   * The match is deliberately fuzzy. An exact comparison looked right and was
   * useless in practice: a terminal redraws its lines, and one repainted
   * character -- a changed elapsed time, a moved cursor, a re-rendered badge --
   * made the overlap fail, so the whole screen counted as new and got sent
   * again. Eighty percent of the lines matching means it is the same block of
   * output with some of it repainted.
   *
   * Largest k first, so the answer is the LONGEST overlap rather than the first
   * coincidental one. A floor of three lines stops a pair of blanks matching
   * everything.
   */
  for (let k = Math.min(o.length, n.length); k >= OVERLAP_MIN; k--) {
    const a = o.slice(o.length - k)
    const b = n.slice(0, k)
    let same = 0
    for (let i = 0; i < k; i++) if (a[i] === b[i]) same++
    if (same / k >= OVERLAP_SAME) return n.slice(k)
  }
  // No overlap: the screen was replaced wholesale, which is as new as it gets.
  return n
}

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
let showOnce = null      // pane whose next transcript frame is shown unasked
/** target -> { at, text } of the last screen actually sent for it. */
const lastSent = new Map()
/** target -> epoch ms until which we are expecting an answer we asked for. */
const awaitingReply = new Map()

/** Called whenever WE put something into a pane. */
function expectReply(target) {
  awaitingReply.set(target, Date.now() + REPLY_MS)
}

/**
 * May we send this screen now?
 *
 * Two gates, and BOTH must pass: the text has to have changed, and QUIET_MS
 * has to have elapsed since the last one. Unchanged output is the common case
 * -- an idle pane re-reports the same screen -- and sending it again says
 * nothing while costing a notification on somebody's phone.
 *
 * `force` is for a screen the user just asked for by tapping a pane. An
 * explicit request is not spam, but it still RECORDS, so the next automatic
 * send is measured from it.
 */
function maySend(target, text, force = false) {
  const prev = lastSent.get(target)
  const now = Date.now()
  const eager = now < (awaitingReply.get(target) ?? 0)
  if (!force) {
    // Unchanged is never worth a notification, in any mode.
    if (prev && prev.text === text) return false
    // Nor is a screen that only twitched. This applies even while waiting on a
    // reply: a spinner is not an answer.
    if (prev && newLines(prev.text, text).filter((l) => l.trim()).length < MIN_NEW_LINES) return false
    // The floor applies only when we are NOT waiting on a reply we asked for.
    if (!eager && prev && now - prev.at < QUIET_MS) return false
  }
  lastSent.set(target, { at: now, text })
  return true
}
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
        channel.send(`NEEDS YOU — ${short(p.id)}\n${(p.title ?? '').slice(0, 60)}`,
          null, { choices: [{ label: 'open', data: `p:${p.id}` }] })
        send({ type: 'subscribe', data: { targets: { [p.id]: 'transcript' } } })
      }
    }
  }

  if (m.type === 'transcript') {
    const t = m.data.target
    const screen = clean(m.data.text)
    // Just followed: show the screen ONCE, unprompted. "Follow it and then
    // wait for it to say something" is a dead end when the pane is idle --
    // which is most of them, most of the time.
    if (showOnce === t) {
      showOnce = null
      if (!maySend(t, screen, true)) return
      const tail = screen.split('\n').slice(-20).join('\n')
      channel.send(`${short(t)} — last ${Math.min(20, tail.split('\n').length)} lines\n\n${tail}`,
        null, { choices: paneActions(t) })
      return
    }
    // Only forward a screen when it was ASKED for: a blocked agent's question,
    // or an explicit /tail. Otherwise this becomes the mirror we refused to be.
    // Forward a screen when it was asked for: a live /tail, an agent that is
    // blocked, or an answer to something we just sent.
    const wanted = Date.now() < tailUntil && t === following
    const isBlocked = lastState.get(t) === 'blocked'
    const answering = Date.now() < (awaitingReply.get(t) ?? 0)
    if (!muted && (wanted || isBlocked || answering)) {
      const prev = lastSent.get(t)?.text
      if (maySend(t, screen)) {
        /*
         * Send only what is NEW. Re-sending a screen that is mostly lines the
         * reader already has is how a chat becomes unreadable -- they have to
         * find the new part themselves, every time.
         *
         * A blocked agent is the exception that proves it: its question may
         * have arrived in an earlier frame, so the tail goes with it. Being
         * told an agent needs you without being told what it asked is useless.
         */
        const delta = newLines(prev, screen).filter((l) => l.trim())
        const body = isBlocked
          ? screen.split('\n').filter((l) => l.trim()).slice(-14).join('\n')
          : delta.slice(-40).join('\n')
        if (body) channel.send(`${short(t)}\n\n${body}`, null, { choices: paneActions(t) })
      }
    }
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
    { label: 'watch 60s', data: `t:60` },
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
  if (text.startsWith('t:')) return handle(`/tail ${text.slice(2)}`, thread)
  if (text.startsWith('k:')) return handle(`/key ${text.slice(2)}`, thread)
  if (text.startsWith('p:')) {
    const id = text.slice(2)
    if (!panes.some((p) => p.id === id)) return reply('that pane is gone. /ls')
    following = id
    showOnce = id
    send({ type: 'subscribe', data: { targets: { [id]: 'transcript' } } })
    return reply(`following ${short(id)} — fetching its screen…`)
  }

  switch (cmd) {
    case '/help':
      return reply('/ls  /follow N|id  /say TEXT  /key K  /tail SEC  /mute  /unmute  /who')
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
      send({ type: 'subscribe', data: { targets: { [p.id]: 'transcript' } } })
      return reply(`following ${short(p.id)} (${label(p)}). /say to prompt it, /key to answer it.`)
    }
    case '/say':
      if (!following) return reply('/follow one first.')
      if (!arg) return reply('/say what?')
      call('agent.prompt', { pane_id: following, text: arg })
      expectReply(following)
      return reply(`sent to ${short(following)} — watching for the reply`)
    case '/key':
      if (!following) return reply('/follow one first.')
      call('agent.send_keys', { pane_id: following, keys: [arg || 'enter'] })
      expectReply(following)
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
        expectReply(following)
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
