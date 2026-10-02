/*
 * Telegram, as an ordinary chat adapter.
 *
 * Nothing in the binary knows this file exists beyond the fact that it is
 * bundled: there is no Telegram code path in Go, no Telegram flag, and no
 * Telegram-shaped hole this plugs into. It is the same kind of file as
 * template.js, shipped because a working example beats a specification.
 *
 * IT SHIPS DISABLED, and so does [chat] itself. Both switches are off:
 *
 *   [chat]
 *   enabled = true
 *
 *   [[chat.adapters]]
 *   id = "telegram"
 *   enabled = true
 *     [chat.adapters.env]
 *     token_env = "HERDR_EXPOSE_TELEGRAM_TOKEN"
 *     chat_id   = "123456789"     # optional: bind to one chat up front
 *
 * A token in the environment is not consent to start answering messages with
 * it, which is why finding one is not enough to make this run.
 *
 * The token is read with ctx.env(name) and never appears in ctx.config, so it
 * is not written in the config file, not printed in a log line, and not put
 * into a URL this adapter logs. ctx.env registers it with the host's redactor,
 * so even a mistake here cannot leak it.
 */

function api(token, method) {
  return 'https://api.telegram.org/bot' + token + '/' + method
}

// getUpdates long-polls with an offset: Telegram holds the request open until
// something arrives, then every update up to `offset` is acknowledged by
// asking for offset+1. Losing the offset replays the backlog, which is why it
// lives here across calls rather than being recomputed.
let offset = 0
let chat = null

export function name() {
  return 'telegram'
}

export async function poll() {
  const token = ctx.env(ctx.config.token_env || 'HERDR_EXPOSE_TELEGRAM_TOKEN')
  if (!token) {
    ctx.log('no token: set', ctx.config.token_env || 'HERDR_EXPOSE_TELEGRAM_TOKEN')
    return []
  }
  if (chat === null && ctx.config.chat_id) chat = String(ctx.config.chat_id)

  // 25s is deliberately under the host's call budget for poll(). A long poll
  // that outlives its budget is killed mid-request, and the updates it was
  // holding are redelivered -- correct, but it looks like a stall.
  const res = ctx.http({
    method: 'POST',
    url: api(token, 'getUpdates'),
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ offset: offset, timeout: 25, allowed_updates: ['message', 'callback_query'] }),
    timeoutMs: 30000,
  })
  if (res.status !== 200) {
    ctx.log('getUpdates http', res.status)
    return []
  }

  const out = []
  const body = JSON.parse(res.body)
  for (const u of body.result || []) {
    offset = u.update_id + 1

    // A tapped button. Its callback_data IS the command the host understands,
    // so it goes back as `text` and takes the identical path to a typed one.
    if (u.callback_query) {
      const cb = u.callback_query
      const cid = String((cb.message && cb.message.chat && cb.message.chat.id) || '')
      if (chat === null) chat = cid
      // Answer the callback or Telegram spins the button for 30s.
      ctx.http({
        method: 'POST', url: api(token, 'answerCallbackQuery'),
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ callback_query_id: cb.id }), timeoutMs: 5000,
      })
      out.push({ text: String(cb.data || '').trim(), thread: cid,
                 from: cb.from && cb.from.username })
      continue
    }

    const m = u.message
    if (!m || !m.text) continue
    const cid = String(m.chat.id)
    if (chat === null) chat = cid
    out.push({ text: m.text.trim(), thread: cid, from: m.from && m.from.username })
  }
  return out
}

export async function send(text, thread, opts) {
  const token = ctx.env(ctx.config.token_env || 'HERDR_EXPOSE_TELEGRAM_TOKEN')
  if (!token) return
  const to = thread || chat
  if (!to) { ctx.log('dropped: no chat bound yet'); return }

  const body = {
    chat_id: to,
    // 4096 is Telegram's hard limit and it is a 400, not a truncation. The
    // zero-width character is for an empty message, which is also a 400.
    text: text.slice(0, 4000) || '⁣',
    disable_web_page_preview: true,
  }

  if (opts && opts.choices && opts.choices.length) {
    // Two per row: a phone shows a full label at that width, and one per row
    // turns nine panes into a screenful of scrolling.
    const rows = []
    for (let i = 0; i < opts.choices.length; i += 2) {
      rows.push(opts.choices.slice(i, i + 2).map(function (c) {
        return {
          text: c.label.slice(0, 40),
          // 64 BYTES is Telegram's callback_data limit and exceeding it is a
          // hard error, not a truncation.
          callback_data: c.data.slice(0, 64),
        }
      }))
    }
    body.reply_markup = { inline_keyboard: rows }
  }

  const res = ctx.http({
    method: 'POST', url: api(token, 'sendMessage'),
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body), timeoutMs: 15000,
  })
  if (res.status !== 200) ctx.log('sendMessage http', res.status)
}
