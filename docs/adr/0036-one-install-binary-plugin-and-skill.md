# 36. One install: the binary, the plugin AND the agent skill

Status: Accepted (adds `herdr-expose skill install|uninstall|status`; extends
the build contract of 0001 and the plugin entrypoint ids of 0025)

## Context

Three things have to be in place before anybody can use this product the way
the owner actually uses it:

1. the **binary**, built with the web app embedded in it;
2. the **plugin** registration, so Herdr has the actions, panes and startup
   hook;
3. the **agent skill** in `skill/`, so "share this session" said to Claude Code
   turns into the right `herdr-expose share` invocation.

`herdr plugin install muthuishere/herdr-expose` delivered exactly one of those
three and left the other two as folklore:

- The binary lands inside Herdr's managed checkout
  (`~/.config/herdr/plugins/github/dev.deemwar.herdr-expose-*/bin/`), which is
  on nobody's PATH — while every example in the README, in this ADR set and in
  the skill itself says `herdr-expose <verb>`. The README documented the
  `ln -sf` that fixes it, which is a way of saying the install is not finished.
- The skill was not mentioned in the README at all. `grep -i skill README.md`
  returned nothing. The primary interface to the product was discoverable only
  by listing the repository.

"Run these three commands after the one command" is not an install; it is a
README that has given up.

## Decision

**A `skill` verb in the binary, and the build step calls it.**

1. **`herdr-expose skill install | uninstall | status`**, shaped exactly like
   the existing `service` verb: converge on a desired state, say what was
   already true, refuse loudly rather than clobber, and make a second run a
   no-op instead of a repair.

2. **A symlink, never a copy.** `~/.claude/skills/herdr-share` points at this
   checkout's `skill/`. A copy forks the moment somebody runs `git pull`, and
   then the agent is reading instructions that no longer match the binary they
   drive — the worst possible way to be wrong about a tool that puts terminals
   on the internet. A symlink makes a rebuild and a pull update the skill for
   free.

3. **The source is RESOLVED, never guessed.** The same binary runs from a dev
   clone and from Herdr's managed checkout, and both exist on the owner's
   machine. `skill install` walks up from the *resolved* executable path
   looking for a directory holding both `herdr-plugin.toml` and
   `skill/SKILL.md`, so it links the checkout it was actually built from. The
   working directory is the fallback, for `go run` and test binaries.

4. **`~/.claude/skills` is created; `~/.agents/skills` is only used if it
   already exists.** Claude Code is the client the skill is written for, so
   installing there means making it work. The shared agent tree belongs to
   another runtime, and a runtime that is not set up is not one to set up from
   here.

5. **A real directory in the way is refused, not replaced**, and `uninstall`
   removes a symlink only when it resolves to a directory whose `SKILL.md`
   declares `name: herdr-share`. A hand-written skill is somebody's work; a
   symlink to a different skill that happens to share the name is not ours to
   delete. A wrong symlink — the stale path left by a checkout that moved — IS
   repointed, because that is the state a reinstall exists to fix.

6. **`scripts/build.sh` finishes the job and prints every path it touched.**
   Herdr runs that script at `plugin install`, so this is the only hook that
   makes the one-command path genuinely one command. It links
   `~/.local/bin/herdr-expose` (the PATH papercut) and runs `skill install`.
   `--no-link` / `HERDR_EXPOSE_NO_LINK=1` skips both, `--release` never links,
   and neither step can fail the build — `herdr plugin install` aborts on a
   non-zero exit, and a skill link is not a reason to lose the plugin.

7. **`hex:install-skill` is the repair verb, not the install path.** Making the
   action the *only* way to get the skill would have left the user hunting for
   it, which is the problem. It exists for the checkout that moved, the manual
   uninstall, and the machine whose `~/.claude` did not exist at build time.

### Why the build step, and not a separate `scripts/install.sh`

`herdr plugin install` runs `[[build]]`, and only `[[build]]`. An
`install.sh` would be a second name for the same work that the one documented
install path never calls — so the plugin user would still end up with a binary
off PATH and no skill, which is precisely the bug. `build.sh` was already the
install script in everything but its name.

The cost, taken deliberately: a build step writes into `$HOME`. That is only
acceptable because it is **announced** — every path is printed as it is linked,
nothing real is overwritten, `--no-link` opts out, and `skill uninstall`
reverses it.

## Consequences

- `herdr plugin install muthuishere/herdr-expose` now ends with a binary on
  PATH and a working `herdr-share` skill, and says so.
- The README documents the skill for the first time, including what you say to
  it versus what it runs.
- A moved or reinstalled checkout self-heals on the next build, because the
  link is rewritten from the resolved executable path rather than assumed.
- `skill status` is the answer to "is the thing the agent is reading the same
  thing I am editing?", which previously required `ls -la` and a squint.
