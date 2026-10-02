/*
 * A chat adapter: the whole contract, and nothing else.
 *
 * Copy this file, implement two functions, point [[chat.adapters]] at it.
 * There is no third function, no lifecycle to learn and no Herdr concept in
 * here -- an adapter does not know what a pane is, what an agent is, or that
 * it is talking to a terminal at all.
 *
 * WHAT THE HOST ALREADY DID, so you do not:
 *   - stripped the terminal's furniture out of the text
 *   - folded code and diffs to "[12 lines of code]" markers
 *   - waited until the agent STOPPED and the screen stopped moving, so you
 *     get one message per answer instead of a dozen half-written ones
 *   - remembered what this reader has already been shown, and sent you only
 *     the new part
 *   - made sure only one of you is running per channel
 * Every one of those was written twice before it moved here, and both copies
 * grew the same bugs. Do not reimplement them; you will get a worse answer.
 *
 * THE HOST API, all of it:
 *   ctx.config       the [chat.adapters.env] table, as strings
 *   ctx.env(name)    read a process env var BY NAME. The value is registered
 *                    as a secret, so it cannot reach a log even if you try to
 *                    print it. This is how a token gets in. Never put a
 *                    token in ctx.config -- that is a plaintext credential in
 *                    a config file.
 *   ctx.http(req)    { method, url, headers, body, timeoutMs } ->
 *                    { status, headers, body }
 *   ctx.log(...)     a log line, scrubbed of every registered secret
 */

export function name() {
  return 'template'
}

/*
 * Return the messages that arrived since the last call.
 *
 * Block if your API long-polls; return [] if there is nothing. You are called
 * in a sequential loop and never re-entered while you are still inside, so
 * you do not need a lock and you cannot miss a call by being slow.
 *
 * Each message: { text, thread, from? }
 *   text    what the person typed. A tapped BUTTON must come back here as
 *           that button's `data` string -- then a tap and a typed command are
 *           the same thing to the host, and no command logic is written twice.
 *   thread  the conversation id, handed back to you in send().
 *   from    a display name, optional, only used in logs.
 */
export async function poll() {
  return []
}

/*
 * Deliver one message.
 *
 * opts.choices is [{ label, data }] when the host is offering options.
 * Render them as buttons if your app has them, and feed a tap back through
 * poll() as `data`. If it has no buttons, print them -- the text commands
 * accept the same payloads, which is why the console adapter is not a
 * degraded experience, just a plainer one.
 */
export async function send(text, thread, opts) {
  ctx.log('send:', text.slice(0, 60))
}
