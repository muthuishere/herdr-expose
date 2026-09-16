package main

import "io/fs"

// webFS returns the embedded web bundle, or nil when none was built in.
//
// This file is the seam for workstream D: it owns the build and replaces this
// with a go:embed of web/dist. internal/serve accepts a nil fs.FS and serves a
// plain "web UI not built" page, so the API is usable either way.
func webFS() fs.FS { return nil }
