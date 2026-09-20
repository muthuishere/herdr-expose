# 33. The config file documents itself

Status: Accepted (SPEC AMENDMENTS 11 §H1/§H2; extends ADR 0009)

## Context

ADR 0009 settled *where* the config lives and that it is forward-compatible. It
did not settle what a **first run** writes. A minimal config — only the keys the
tool currently needs — means every optional subsystem is invisible: nobody finds
`[log]`, `[share]` or the device limits without reading the source.

*"Create the config automatically with empty so they know."* A key that exists
but is not written is a key nobody will ever find.

## Decision

First run writes the **COMPLETE** config: every section, every key, at its
default value, each with a one-line comment saying what it does. Optional
subsystems appear too, switched off, so their existence is discoverable without
reading the source or the docs.

`herdr-expose config print-default` prints the same scaffold to stdout (mirroring
`herdr --default-config`), and `config path` / `config show` / `config edit`
round it out.

The forward-compatibility rules from ADR 0009 still hold, and they constrain
this one: **unknown keys are preserved on rewrite**, missing keys take defaults,
and an older config loads on a newer binary. **Adding a section must never
rewrite or reorder what the user has already edited.**

Sections: `[server]`, `[auth]`, `[ui]`, `[expose]`, `[share]`, `[log]`. Notably
**there is no `bind` key** — the resolved exposure mode decides the address (ADR
0022) — and the file says so in a comment where a reader would look for it.
There are no secrets: the only secrets in this system are hashes in the state
directory.

## Consequences

- The scaffold is a second place the defaults are written down, so it is
  generated from the same constants the loader uses and tested against them.
  A drifting scaffold is worse than no scaffold.
- The file is longer than it needs to be, which is the intended trade: it is
  read far more often than it is written.
- Because it is complete and secret-free, the config is safe to paste into a bug
  report — which is the other reason credentials are environment-only (ADR 0032).
