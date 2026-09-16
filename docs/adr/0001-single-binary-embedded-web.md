# 1. One Go binary with the React app embedded

Status: Accepted

## Context

herdr-expose ships as a Herdr plugin. `herdr plugin install` runs a build step
on the user's machine and then a startup hook. Anything the user has to install
separately, keep running, or point at a directory is a step where the install
fails silently and the UI never appears.

A React SPA normally needs node at build time and a static file server at run
time. That is two runtimes, two deploy paths, and a `--static-dir` flag nobody
gets right.

## Decision

`go build` produces exactly one artifact: `bin/herdr-expose`, with `web/dist`
inside it via `//go:embed all:web/dist`. Node is a build-time dependency only —
`scripts/build.sh` runs `npm ci && npm run build` and then the Go build. At run
time there is no node, no static path, no second process.

Hashed Vite asset filenames are served `immutable`; `index.html` and `sw.js` are
served `no-cache`, so an upgraded binary is picked up on the next load.

## Consequences

- One file to copy, one file to sign, one file to cross-compile (CGO_ENABLED=0).
- The web build must run before the Go build; `go build ./...` alone fails on a
  clean checkout until `web/dist` exists. `scripts/build.sh` is the only
  supported entry point, and the manifest `[[build]]` step calls it.
- Shipping a UI fix means shipping a new binary. Accepted: the binary is small
  and the API, not the UI, is the stable contract (see ADR 0006).
- Release assets are per-platform, so the build step can fall back to
  downloading a prebuilt binary for users without a Go toolchain.
