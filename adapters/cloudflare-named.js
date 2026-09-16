// cloudflare-named.js — a named Cloudflare tunnel on a hostname you own.
//
// The stable-hostname pattern Muthu already runs on the deemwar zone
// (cryptoremote.deemwar.com and friends): one long-lived named tunnel, a
// proxied CNAME to <tunnel-id>.cfargotunnel.com, and `cloudflared tunnel run`.
//
// PREFER THE BUILT-IN. In Go, `cloudflare = true` + `domain = "..."` also
// creates the tunnel and writes the DNS record for you through the Cloudflare
// API. This adapter assumes the tunnel and the DNS record ALREADY EXIST —
// either created by the built-in provider once, or by hand:
//
//     cloudflared tunnel create herdr-expose
//     cloudflared tunnel route dns herdr-expose herdr.example.com
//
// Config:
//
//     [expose]
//     adapter = "cloudflare-named"
//
//     [[expose.adapters]]
//     id = "cloudflare-named"
//     script = "adapters/cloudflare-named.js"
//     [expose.adapters.env]
//     tunnel = "herdr-expose"            # tunnel name or UUID
//     hostname = "herdr.example.com"     # the stable URL
//     # token_env = "CLOUDFLARE_TUNNEL_TOKEN"  # optional: run token by env NAME

export function start(ctx) {
  const tunnel = ctx.config.tunnel;
  const hostname = ctx.config.hostname;
  if (!hostname) throw new Error("cloudflare-named: set env.hostname to the stable hostname");

  const args = ["tunnel", "--no-autoupdate"];
  const env = {};

  // Two ways in. A run token needs no local credentials file and no tunnel
  // name; it is a secret, so it travels in the environment, never in argv.
  const tokenEnvName = ctx.config.token_env;
  const token = tokenEnvName ? ctx.env(tokenEnvName) : undefined;
  if (token) {
    env.TUNNEL_TOKEN = token;
    args.push("run");
  } else {
    if (!tunnel) throw new Error("cloudflare-named: set env.tunnel (or env.token_env)");
    args.push("--url", ctx.localUrl, "run", tunnel);
  }

  const p = ctx.spawn("cloudflared", args, env);

  // The hostname is known up front — report it immediately so the UI has a URL
  // without waiting for cloudflared to say anything.
  ctx.setUrl("https://" + hostname);

  ctx.onLine(p, (line) => {
    if (/Registered tunnel connection|Connection .* registered/.test(line)) {
      ctx.log("connector registered");
    }
  });

  return { pid: p.pid };
}

export function status(ctx) {
  return { url: ctx.url, healthy: ctx.isAlive() };
}

export function stop(ctx) {
  ctx.kill();
}
