# 9. One config path, TOML outside the repo, forward-compatible

Status: Accepted (amends the earlier `$HERDR_PLUGIN_CONFIG_DIR`-first draft)

## Context

Herdr hands plugins a config dir via `$HERDR_PLUGIN_CONFIG_DIR`, and an earlier
draft preferred it. That variable is **only set when Herdr launches the plugin**.
Start `herdr-expose status` from a shell and it is unset; run the same binary
from a Herdr pane and it is set — so the tool reads two different config files
depending on how it was started, and the user's settings appear to randomly
revert. The Node reference hit this and documented it.

Separately: config that lives in the repo gets committed, and config containing
a token gets committed and then pushed.

## Decision

- Config is TOML at **`~/.config/herdr-expose/config.toml`, unconditionally**.
  `$HERDR_PLUGIN_CONFIG_DIR` is deliberately **not** honoured. One binary, one
  config, regardless of launch path.
- `$HERDR_PLUGIN_STATE_DIR` is still used for the pidfile and other runtime
  state — that is genuinely per-installation and has no "which one am I reading"
  ambiguity.
- Created with sane defaults on first run if absent; hot-reloaded on SIGHUP.
- **Forward-compatible by construction**: unknown keys are preserved on rewrite,
  missing keys take defaults. A newer version's settings survive a round trip
  through an older binary.
- **No secrets in config.** Tokens live as SHA-256 hashes in 0600 state (ADR
  0017), never in `config.toml`, never in the repo, never in a log, never in
  `/v1/config`. The Cloudflare API token is read from the environment by name at
  point of use (ADR 0005).
- `.gitignore` covers `*.local.toml` and any local config copy.

## Consequences

- "Where is my config" has exactly one answer, printable by `herdr-expose
  status`, and it does not depend on who started the process.
- We diverge from the plugin convention Herdr offers. Deliberate, and the reason
  is written down here so it is not "fixed" later.
- Round-tripping TOML while preserving unknown keys needs a document-preserving
  approach, not a naive struct marshal. Real constraint on the config layer.
