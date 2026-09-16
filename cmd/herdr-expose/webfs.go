package main

import (
	"io/fs"

	herdrexpose "github.com/muthuishere/herdr-expose"
)

// webFS returns the React PWA embedded at the module root, or nil when the web
// app was never built (internal/serve then renders a "web UI not built" page).
func webFS() fs.FS { return herdrexpose.WebFS() }
