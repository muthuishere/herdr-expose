// Package herdrexpose holds the embedded web bundle.
//
// It lives at the module root because go:embed patterns resolve relative to the
// package directory, and only a root package can reach web/dist.
package herdrexpose

import (
	"embed"
	"io/fs"
)

// The React PWA, compiled into the binary: one artifact, no node at runtime,
// no static paths to configure (ADR-0001).
//
// web/dist is gitignored — build output, not source — so a committed
// web/dist/.gitkeep keeps this embed compiling from a clean checkout. A binary
// built without scripts/build.sh then yields a nil FS, which internal/serve
// renders as "web UI not built" instead of a wall of 404s.
//
//go:embed all:web/dist
var webDist embed.FS

// WebFS returns the embedded bundle, or nil when the web app was never built.
func WebFS() fs.FS {
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
