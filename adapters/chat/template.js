#!/usr/bin/env node
/*
 * A chat adapter: the whole contract, and nothing else.
 *
 * Copy this, make it talk to your chat app, point [[chat.adapters]] at it with
 * `command = "node my-adapter.js"`. Any runtime works -- bun, python3, a Go
 * binary -- because the host runs a COMMAND and speaks newline-delimited JSON
 * to it, which every language reads with no dependency.
 *
 *   stdin   <- { type: "send", text, thread, choices: [{label, data}] }
 *   stdout  -> { type: "ready" }
 *              { type: "message", text, thread, from }
 *   stderr  -> your own debugging; the host logs it, scrubbed of secrets.
 *
 * WHAT THE HOST ALREADY DID, so you do not:
 *   - stripped the terminal's furniture out of the text
 *   - folded code and diffs into "[12 lines of code]" markers
 *   - waited until the agent STOPPED and the screen stopped moving, so you get
 *     one message per answer instead of a dozen half-written ones
 *   - sent you only what this reader has not already seen
 *   - kept menus, commands and state, so you need none of them
 * Each of those was written twice before it moved into the host, and both
 * copies grew the same bugs. `text` arrives finished: send it as it is.
 *
 * `choices` are buttons IF your app has them. If it does not, ignore them --
 * `text` already lists the same options and the same strings work when typed,
 * so a plain adapter is not a degraded one. When somebody taps a button, send
 * that choice's `data` back as `text`, unmodified: that is what makes a tap and
 * a typed command the same event, so no command is implemented twice.
 *
 * Secrets come from the ENVIRONMENT, never from argv (which `ps` shows) and
 * never from the config file (which gets backed up). Config holds "$NAME"; the
 * host expands it and hands you the value here.
 */

const TOKEN = process.env.MY_SERVICE_TOKEN
if (!TOKEN) {
  console.error('MY_SERVICE_TOKEN is not set')
  process.exit(1)
}

function out(obj) {
  // One frame, one line: a newline inside the JSON is read as two frames.
  process.stdout.write(JSON.stringify(obj) + '\n')
}

/* Read frames from the host. A frame can arrive in pieces, so keep the
 * remainder rather than treating a partial line as a whole one. */
let buf = ''
process.stdin.setEncoding('utf8')
process.stdin.on('data', (chunk) => {
  buf += chunk
  let i
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i)
    buf = buf.slice(i + 1)
    if (!line.trim()) continue
    let m
    try {
      m = JSON.parse(line)
    } catch {
      console.error('unparseable frame:', line.slice(0, 120))
      continue
    }
    if (m.type === 'send') deliver(m)
  }
})

async function deliver(m) {
  // Send m.text to your chat app, in m.thread. Render m.choices as buttons if
  // you have them. That is the entire job.
  console.error('would send:', (m.text || '').slice(0, 80))
}

/* Report what arrives from your chat app. Long-poll if your API supports it;
 * you are never called concurrently with yourself. */
async function listen() {
  // out({ type: 'message', text: 'what they typed', thread: 'conversation id' })
}

out({ type: 'ready' })
listen()
