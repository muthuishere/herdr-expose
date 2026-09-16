// ngrok.js — expose through ngrok.
//
// Built in too (`ngrok = true` under [expose], with an optional `domain`).
// This adapter is the escape hatch for flags the built-in does not expose,
// e.g. an edge, a custom region or basic auth.
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
