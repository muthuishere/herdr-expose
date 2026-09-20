// template.js — copy this to write your own herdr-expose tunnel adapter.
//
// THIS IS THE EXTENSION POINT. There is exactly ONE built-in transport:
//
//     [expose]
//     cloudflare = true
//     domain = "herdr.example.com"
//
// which does the whole job — creates/reuses the named tunnel, writes the DNS
// record, runs cloudflared, restarts it if it dies.
//
// EVERYTHING ELSE IS AN ADAPTER, and an adapter is the supported answer, not a
// consolation prize: ngrok, tailscale funnel, a corporate reverse proxy, an
// SSH -R to your own box, a homelab ingress. An adapter written here is a full
// provider to the host — it declares a footprint, its URL is verified before
// it is published, it is supervised and restarted, and its Stop and Destroy
// are held to the same idempotency rules as the built-in. herdr-expose used to
// carry a second built-in provider (ngrok) and removed it in AMENDMENTS 18 /
// ADR 0034, because a transport nobody here could run end to end rots in
// place; a transport YOU run is a transport that gets exercised. See
// adapters/ngrok.js for a worked example of exactly that.
//
// WHAT YOU GET, AND WHAT YOU OWE
// ------------------------------
// You get: process supervision, line scanning, URL verification, secret
// handling by env-var NAME, and redaction of anything you register.
// You owe: honesty about what your adapter CREATES. If it provisions
// something in an account, destroy() is how it comes back out; if it creates
// nothing, say so and destroy() is a no-op.
//
// HOW IT RUNS
// -----------
// The script is executed on goja, embedded in the Go binary. There is NO node
// at runtime, and the VM is deliberately bare:
//
//   * no require / import, no fs, no fetch, no XMLHttpRequest, no sockets,
//     no setTimeout. If you need the network, spawn a real tool.
//   * the host kills any call that runs longer than ~20s, so no busy loops.
//   * a throw, a panic or a hang in here cannot take the server down.
//
// You export three functions. Only start() is mandatory.
//
// THE ctx HOST API
// ----------------
//   ctx.spawn(cmd, args, [env])  run a binary. `cmd` is resolved to an absolute
//                                path (PATH plus the usual install dirs).
//                                Returns { pid }. The optional third argument
//                                is a map of extra environment variables —
//                                USE IT FOR SECRETS, never args, because argv
//                                is visible to every process on the machine.
//   ctx.onLine(proc, fn)         call fn(line) for each stdout/stderr line.
//   ctx.setUrl(s)                report the public URL. This is what the UI and
//                                `herdr-expose status` show. Call it as soon as
//                                you know the URL.
//   ctx.url                      the URL you last reported (live getter).
//   ctx.localUrl                 "http://127.0.0.1:<port>" — what to expose.
//   ctx.isAlive()                true while a process you spawned is running.
//   ctx.kill()                   kill everything you spawned.
//   ctx.log(s)                   log a line (secrets are scrubbed).
//   ctx.config                   this adapter's [expose.adapters.env] table.
//   ctx.env(name)                read a process env var BY NAME. The VALUE is
//                                yours to pass to a spawned tool — it is never
//                                logged, never written to state and never
//                                returned by status(), even if you try.
//
// CONFIG THAT SELECTS THIS ADAPTER
// --------------------------------
//     [expose]
//     adapter = "my-tunnel"          # a named adapter beats the built-ins
//
//     [[expose.adapters]]
//     id = "my-tunnel"
//     script = "adapters/template.js"
//     [expose.adapters.env]
//     hostname = "herdr.example.com"
//     token_env = "MY_TUNNEL_TOKEN"  # the NAME of the env var, not the value

// start() is called once. Spawn your tool, watch its output, report the URL.
// Return whatever you like; it is logged for debugging only.
export function start(ctx) {
  const host = ctx.config.hostname;           // may be undefined
  const args = ["--local", ctx.localUrl];
  if (host) args.push("--hostname", host);

  // Secrets go in the environment, never in args.
  const env = {};
  if (ctx.config.token_env) {
    const secret = ctx.env(ctx.config.token_env);
    if (secret) env.MY_TUNNEL_TOKEN = secret;
  }

  const p = ctx.spawn("my-tunnel-tool", args, env);

  ctx.onLine(p, (line) => {
    // Scrape whatever your tool prints when the tunnel comes up.
    const m = /(https:\/\/[^\s"']+)/.exec(line);
    if (m) ctx.setUrl(m[1]);
  });

  // If the hostname is already known there is no need to scrape at all:
  // if (host) ctx.setUrl("https://" + host);

  return { pid: p.pid };
}

// status() is polled. Keep it cheap and side-effect free. Never put a
// credential in the return value — the host would strip it, but do not try.
export function status(ctx) {
  return { url: ctx.url, healthy: ctx.isAlive() && !!ctx.url };
}

// stop() MUST be idempotent: it can be called twice, or after the process has
// already died. ctx.kill() is safe in all of those cases. The host also kills
// anything you spawned after this returns (or throws, or hangs), so a leak is
// impossible — but clean up anyway.
export function stop(ctx) {
  ctx.kill();
}
