// ngrok.js — expose through ngrok. A WORKED EXAMPLE of adapters/template.js.
//
// READ THIS FIRST. ngrok was a built-in Go provider until AMENDMENTS 18 /
// ADR 0034 removed it, and the reason matters here: there is no ngrok binary
// and no ngrok account on the machine this repo is built on, so the built-in
// was unit-tested and NEVER ONCE run end to end. Code on a public surface that
// has never actually run is a liability — it rots, and the first person to use
// it finds the bug.
//
// So this file is exactly what it looks like: community-shaped example code.
// CI loads it under goja to prove it parses and exports start/status/stop, and
// that is ALL that is verified. Nothing here has been run against real ngrok.
// Treat it as a starting point you are expected to test, not as a supported
// transport. It is kept because a JS adapter is the right home for ngrok — it
// is roughly forty lines, it lives next to the tool the user already has
// installed and authenticated, and whoever runs it is the person who can
// actually verify it.
//
// Config:
//
//     [expose]
//     adapter = "ngrok"
//
//     [[expose.adapters]]
//     id = "ngrok"
//     script = "adapters/ngrok.js"
//     [expose.adapters.env]
//     # domain = "my-reserved.ngrok.app"     # omit for a random hostname
//     # authtoken_env = "NGROK_AUTHTOKEN"    # env var NAME; the value never leaks
//     # region = "in"

export function start(ctx) {
  const port = ctx.localUrl.replace(/^https?:\/\/[^:]+:?/, "") || "80";
  const args = ["http", port, "--log", "stdout", "--log-format", "logfmt"];

  if (ctx.config.domain) args.push("--domain", ctx.config.domain);
  if (ctx.config.region) args.push("--region", ctx.config.region);

  const env = {};
  const name = ctx.config.authtoken_env || "NGROK_AUTHTOKEN";
  const token = ctx.env(name);
  if (token) env.NGROK_AUTHTOKEN = token;

  const p = ctx.spawn("ngrok", args, env);

  if (ctx.config.domain) ctx.setUrl("https://" + ctx.config.domain);

  ctx.onLine(p, (line) => {
    const m = /(https:\/\/[a-zA-Z0-9.-]+\.ngrok(?:-free)?\.(?:app|io|dev))/.exec(line);
    if (m) ctx.setUrl(m[1]);
  });

  return { pid: p.pid };
}

export function status(ctx) {
  return { url: ctx.url, healthy: ctx.isAlive() && !!ctx.url };
}

export function stop(ctx) {
  ctx.kill();
}
