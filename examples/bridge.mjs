#!/usr/bin/env node
/**
 * A complete herdr-expose client, in one file, with no dependencies.
 *
 * This is the thing to copy when you are wiring herdr-expose to a chat app, a
 * bot, a dashboard, or anything else that is not the bundled web UI. It lists
 * sessions and panes, follows one pane, prints what the agent is showing, and
 * can answer it.
 *
 *   node examples/bridge.mjs                      # local daemon, no auth
 *   node examples/bridge.mjs --url http://HOST:PORT --code ABC123
 *   node examples/bridge.mjs --follow openjevx/w1:p1 --say "run the tests"
 *   node examples/bridge.mjs --follow openjevx/w1:p1 --key y
 *
 * Needs Node 22+ for the global WebSocket. There is no npm install.
 *
 * THE PART PEOPLE GET WRONG, up front:
 *
 *   - `subscribe` takes an OBJECT, not an array. `{ targets: { "s/w1:p1":
 *     "transcript" } }`. An array parses into an empty map and you will sit
 *     there receiving nothing, with no error.
 *   - `seq` is NOT a resume cursor. There is no replay. On reconnect you
 *     re-subscribe; you never ask for "everything since N".
 *   - Binary frames are terminal bytes. A text client ignores them.
 */

const args = Object.fromEntries(
  process.argv.slice(2).flatMap((a, i, all) =>
    a.startsWith('--') ? [[a.slice(2), all[i + 1]?.startsWith('--') ? true : all[i + 1] ?? true]] : [],
  ),
)

const BASE = (args.url ?? 'http://127.0.0.1:21118').replace(/\/$/, '')
const WS = BASE.replace(/^http/, 'ws') + '/v1/stream'

/*
 * AUTH, and why local needs none.
 *
 * On loopback the listener IS the grant: reaching 127.0.0.1 already means you
 * are on the machine, so there is nothing further to prove. That is why a bot
 * on the same box connects with no token and no setup.
 *
 * A SHARE is different. It is on a LAN address or a public hostname, so it is
 * pairing-gated: `herdr-expose share pair <id>` prints a six-character code at
 * the machine (never over the tunnel), you redeem it ONCE here for a device
 * token, and you keep the token. The token expires with the share, by design —
 * a bridge should not outlive the thing it was given access to.
 */
async function token() {
  if (!args.code) return null
  const r = await fetch(`${BASE}/v1/pair`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code: args.code, name: args.name ?? 'bridge.mjs' }),
  })
  if (!r.ok) throw new Error(`pair failed: ${r.status} ${await r.text()}`)
  const j = await r.json()
  console.error(`paired; token expires ${j.expires_at}`)
  return j.token
}

/** Walk the tree, whatever shape it has, and return every pane. */
function panesOf(node, out = []) {
  if (!node || typeof node !== 'object') return out
  for (const p of node.panes ?? []) out.push(p)
  for (const key of ['sessions', 'workspaces', 'tabs', 'children']) {
    for (const child of node[key] ?? []) panesOf(child, out)
  }
  return out
}

const tok = await token()
const ws = new WebSocket(WS, tok ? { headers: { Authorization: `Bearer ${tok}` } } : undefined)

let seq = 0
const send = (frame) => ws.send(JSON.stringify({ seq: ++seq, ...frame }))

/** Call a Herdr method. `agent.prompt` and `agent.send_keys` are the useful two. */
const call = (method, params) =>
  send({ type: 'command', data: { id: `c-${seq + 1}`, method, params } })

let following = args.follow ?? null
let acted = false

ws.onopen = () => console.error(`connected to ${WS}${tok ? ' (token)' : ' (loopback, no auth)'}`)
ws.onerror = (e) => console.error('socket error:', e.message ?? String(e))
ws.onclose = () => console.error('closed')

ws.onmessage = (ev) => {
  // Binary = raw terminal bytes for a live pane. Not our business here.
  if (typeof ev.data !== 'string') return
  const m = JSON.parse(ev.data)

  /*
   * `tree` is the whole picture and it arrives unprompted whenever anything
   * changes: sessions, their panes, and each pane's agent state. A share's
   * token sees only the panes inside that share's scope -- the filtering is
   * done in the store, not in the UI, so you cannot ask your way out of it.
   */
  if (m.type === 'tree') {
    const panes = panesOf(m.data)
    if (!following) {
      console.log(`\n${panes.length} pane(s):`)
      for (const p of panes) {
        const state = p.agent?.state && p.agent.state !== 'unknown' ? p.agent.state : 'shell'
        console.log(`  ${p.id.padEnd(26)} ${state.padEnd(8)} ${p.title ?? ''}`)
      }
      // `blocked` is the one state worth waking somebody for.
      const blocked = panes.filter((p) => p.agent?.state === 'blocked')
      if (blocked.length) console.log(`\nNEEDS YOU: ${blocked.map((p) => p.id).join(', ')}`)
      ws.close()
      return
    }
    if (!acted) {
      acted = true
      send({ type: 'subscribe', data: { targets: { [following]: 'transcript' } } })
      if (args.say) call('agent.prompt', { pane_id: following, text: String(args.say) })
      if (args.key) call('agent.send_keys', { pane_id: following, keys: [String(args.key)] })
    }
  }

  /*
   * `transcript` is what the pane is SHOWING -- the screen, with escape codes
   * stripped. It is not a conversation log: herdr exposes the screen, not the
   * agent's message history, and there is no reconstructed turn structure in
   * it. Render it as text and say where it came from.
   */
  if (m.type === 'transcript') {
    console.log(`\n--- ${m.data.target} (${m.data.source}) ---`)
    console.log(m.data.text.trimEnd())
    if (!args.watch) ws.close()
  }

  if (m.type === 'result' && m.data?.error) console.error('command failed:', m.data.error)
}

// Without --watch this is a one-shot: print and leave.
if (!args.watch) setTimeout(() => process.exit(0), Number(args.timeout ?? 15000))
