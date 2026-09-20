//go:build !windows

package main

import "os"

// linkDir points link at src. A symlink is the whole story on Unix.
func linkDir(src, link string) error { return os.Symlink(src, link) }

// isDirLink reports whether what Lstat found is a link this tool could have
// made, as opposed to a real directory that must never be touched.
func isDirLink(fi os.FileInfo, _ string) bool { return fi.Mode()&os.ModeSymlink != 0 }

// readDirLink returns what the link points at.
func readDirLink(link string) (string, error) { return os.Readlink(link) }
