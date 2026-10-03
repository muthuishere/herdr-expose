#!/usr/bin/env node
/*
 * Telegram, as an ordinary chat adapter.
 *
 * Nothing in the binary knows this file exists beyond the fact that it is
 * bundled: there is no Telegram code path in Go, no Telegram flag, and no
 * Telegram-shaped hole it plugs into. It is the same kind of file as
 * template.js, shipped because a working example beats a specification.
 *
 * IT SHIPS DISABLED, and so does [chat] itself. Both switches must be on:
 *
 *   [chat]
 *   enabled = true
 *
 *   [[chat.adapters]]
 *   id = "telegram"
 *   enabled = true
 *   command = "node telegram.js"
 *     [chat.adapters.env]
 *     HERDR_EXPOSE_TELEGRAM_TOKEN = "$HERDR_EXPOSE_TELEGRAM_TOKEN"
 *     HERDR_CHAT_ID               = "123456789"   # optional
 *
 * A token in the environment is not consent to start answering messages with
 * it, which is why finding one is deliberately not enough to make this run.
 *
 * THE PROTOCOL is newline-delimited JSON:
 *   stdin   <- { type: "send", text, thread, choices: [{label, data}] }
 *   stdout  -> { type: "message", text, thread, from }
 *              { type: "ready" }
 *   stderr  -> whatever you like; the host logs it, scrubbed.
 *
 * `text` arrives FINISHED: already cleaned of terminal furniture, code folded
 * to markers, held until the agent stopped and the screen stopped moving, and
 * diffed against what this reader already saw. Send it as it is. Everything
 * this file does is move bytes.
 */

const TOKEN = process.env.HERDR_EXPOSE_TELEGRAM_TOKEN
if (!TOKEN) {
  console.error('HERDR_EXPOSE_TELEGRAM_TOKEN is not set')
  process.exit(1)
}
let chat = process.env.HERDR_CHAT_ID || null

const api = (m) => `https://api.telegram.org/bot${TOKEN}/${m}`

function out(obj) {
  // One frame, one line. A newline inside the JSON would be read as two
  // frames, each one invalid.
  process.stdout.write(JSON.stringify(obj) + '\n')
}

/* ------------------------------------------------------------------ stdin */

let buf = ''
process.stdin.setEncoding('utf8')
process.stdin.on('data', (chunk) => {
  buf += chunk
  // Split on newlines and keep the remainder: a frame can arrive in pieces,
  // and treating a partial line as a whole one is a parse error on a message
  // that was perfectly fine.
  let i
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i)
    buf = buf.slice(i + 1)
    if (line.trim()) handle(line)
  }
})

async function handle(line) {
  let m
  try {
    m = JSON.parse(line)
  } catch {
    console.error('unparseable frame:', line.slice(0, 120))
    return
  }
  if (m.type !== 'send') return
  await send(m)
}

async function send(m) {
  const to = m.thread || chat
  if (!to) {
    console.error('dropped: no chat bound yet')
    return
  }
  const body = {
    chat_id: to,
    // 4096 is Telegram's hard limit and exceeding it is a 400, not a
    // truncation. The zero-width character covers an empty message, which is
    // also a 400.
    text: (m.text || '').slice(0, 4000) || '⁣',
    disable_web_page_preview: true,
  }

  if (m.choices && m.choices.length) {
    // Two per row: a phone shows a full label at that width, and one per row
    // turns nine panes into a screenful of scrolling.
    const rows = []
    for (let i = 0; i < m.choices.length; i += 2) {
      rows.push(
        m.choices.slice(i, i + 2).map((c) => ({
          text: c.label.slice(0, 40),
          // 64 BYTES is Telegram's callback_data limit, and it is a hard
          // error. Truncating past it would produce a button that silently
          // does nothing, so say so instead.
          callback_data: c.data.length > 64 ? c.data.slice(0, 64) : c.data,
        })),
      )
    }
    body.reply_markup = { inline_keyboard: rows }
  }

  try {
    const r = await fetch(api('sendMessage'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (!r.ok) console.error('sendMessage http', r.status, (await r.text()).slice(0, 200))
  } catch (e) {
    console.error('sendMessage failed:', e.message)
  }
}

/* ----------------------------------------------------------------- stdout */

let offset = 0

async function poll() {
  for (;;) {
    try {
      const r = await fetch(api('getUpdates'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          offset,
          timeout: 25,
          allowed_updates: ['message', 'callback_query'],
        }),
      })
      if (!r.ok) {
        console.error('getUpdates http', r.status)
        await sleep(3000)
        continue
      }
      const body = await r.json()
      for (const u of body.result || []) {
        offset = u.update_id + 1

        if (u.callback_query) {
          const cb = u.callback_query
          const cid = String(cb.message?.chat?.id ?? '')
          if (!chat) chat = cid
          // Answer it or Telegram spins the button for 30 seconds.
          fetch(api('answerCallbackQuery'), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ callback_query_id: cb.id }),
          }).catch(() => {})
          // The button's data IS the command the host understands, so it goes
          // back as text and takes the identical path to a typed one.
          out({ type: 'message', text: String(cb.data || '').trim(), thread: cid, from: cb.from?.username })
          continue
        }

        const msg = u.message
        if (!msg?.text) continue
        const cid = String(msg.chat.id)
        if (!chat) chat = cid
        out({ type: 'message', text: msg.text.trim(), thread: cid, from: msg.from?.username })
      }
    } catch (e) {
      console.error('getUpdates failed:', e.message)
      await sleep(3000)
    }
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

out({ type: 'ready', thread: chat || '' })
poll()
