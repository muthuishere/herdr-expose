# 15. The API is the product; the web UI is its first client

Status: Accepted

## Context

The stated intent is: "today a web UI built in, tomorrow a mobile app just
reuses this exposed API." Those are two very different projects depending on
when you decide it.

If the API is treated as an internal detail of the web app, it accretes
web-shaped assumptions — fields the React store happens to want, behaviour that
is only correct because one particular client does one particular thing first —
and the mobile app becomes a rewrite plus a compatibility layer. If the API is
treated as the product from day one, the web UI is simply the first client and
the second one is a weekend.

## Decision

The HTTP + WebSocket surface at `/v1/*` is a **public, versioned contract**.

- `docs/api.md` must be sufficient to build a Swift or Kotlin client **with zero
  access to this repository's source**: every endpoint, every control frame,
  every binary frame documented byte-for-byte with a worked hex example, the
  pairing flow, and seq/resume/gap semantics.
- The version lives in the path. `/v1` keeps its promise for as long as it is
  served; a breaking change is `/v2`, served alongside.
- Additive changes only within a version: new frame types, new optional fields.
  Clients must ignore unknown frame types and unknown JSON fields, and this is
  stated as a client requirement in `docs/api.md` so the promise is symmetric.
- No endpoint or frame exists because "the web app needs it." If it is in the
  API it is documented, and if it is documented it is supported.
- The web UI gets no privileged path. It authenticates like any other client.

## Consequences

- `docs/api.md` is updated in the same change as the code, not afterwards. An
  undocumented frame is an unfinished change.
- Some churn is slower: renaming a field is now a compatibility question rather
  than a find-and-replace across two directories.
- The web client cannot take shortcuts through server internals, which keeps the
  server honest about what it actually exposes.
- The first real test of this ADR is the mobile client. If it needs anything not
  already in `docs/api.md`, that is a defect in this decision's execution.
