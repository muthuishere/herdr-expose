# 32. A local rotating log file, and no OpenTelemetry

Status: Accepted (SPEC AMENDMENTS 11 and 12; AMENDMENTS 11 §H3/§H4 withdrawn in
full)

## Context

A detached daemon's stderr goes nowhere. "There is no way to see what the daemon
did" was a real, repeated problem — the thing a person needs after an
unexplained reconnect loop is the one thing the design threw away.

An OTLP exporter was specified, then withdrawn: *"no otel machine, just local
log please."* Shipping a collector dependency and a trace pipeline for a
single-binary tool that one person runs on their laptop is the wrong amount of
machinery, and it drags in `go.opentelemetry.io` for no reader.

## Decision

**Default to a FILE, not stderr** — `<state>/herdr-expose.log` when `[log] file`
is empty. **Rotate in-process, with no dependency**: `max_size_mb = 10`,
`keep = 3`. This runs for weeks; an unbounded log is a disk-filling bug.

`herdr-expose logs [--follow] [-n N] [--share <id>] [--json] [--path]` reads it.
Writer and reader go through the **same path resolver**, so a log can never land
somewhere nothing reads it. `doctor` reports the log's path, size and
writability.

**Shares log to their own file** in the share's state directory, so a share's
life is auditable on its own and the log dies with the share.

**Never log pane bytes or secrets, at any level including debug.** Terminal
output is the user's screen and can contain anything; tokens are logged as ids
or hashes, never values. Enforced in code, through the existing
`internal/expose` redactor.

Log what answers *"what happened"*: start/stop with mode and bind, session
connect/disconnect, tunnel provisioning and teardown, share
create/extend/revoke/expire, pairing issued/redeemed/failed with device id, auth
rejections **with reason**, upstream reconnects.

**No `[otel]` section, no OTLP exporter, no `go.opentelemetry.io` dependency.**

## Consequences

- No distributed tracing, and no metrics scrape. `/v1/metrics` still exposes the
  latency histograms the performance budget (ADR 0014) is measured against, for
  anyone who wants to poll it.
- Rotation is ours to get right, including the case where the daemon and a
  `logs --follow` hold the file at once.
- The withdrawn OTEL design also settled a rule worth keeping in any future
  exporter: **terminal output must never be exported.** It is the user's screen
  contents, and shipping it to a collector is a data-exfiltration path, not
  telemetry.
- Credentials for any future exporter would be environment-only, never a config
  key — a config file is something people paste into issues.
