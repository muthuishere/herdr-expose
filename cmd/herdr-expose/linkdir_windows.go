//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// linkDir points link at src.
//
// os.Symlink on Windows needs SeCreateSymbolicLinkPrivilege, which an ordinary
// user does NOT have unless Developer Mode is switched on. On a stock box
// `herdr-expose skill install` therefore died with "A required privilege is not
// held by the client" — measured, not theorised — and the agent skill simply
// could not be installed.
//
// A DIRECTORY JUNCTION is the answer: it is a reparse point that behaves like a
// symlink for directories, it is what `mklink /J` makes, and it requires no
// privilege at all. Go has no junction API, so this shells out to the one tool
// that is on every Windows machine.
//
// A real symlink is still preferred when we are allowed one — it is what the
// Unix path creates, it works for files as well as directories, and Developer
// Mode boxes are common among the people who run this. So: try the symlink,
// fall back to the junction, and only then give up.
func linkDir(src, link string) error {
	if err := os.Symlink(src, link); err == nil {
		return nil
	} else if !isPrivilegeError(err) {
		return err
	}
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, src)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("neither a symlink (needs Administrator or Developer Mode) nor a "+
			"directory junction could be created: mklink /J: %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isPrivilegeError reports the specific "you are not allowed to make symlinks"
// failure, so a genuine problem (the path is taken, the disk is full) is still
// returned rather than being retried as a junction.
func isPrivilegeError(err error) bool {
	return errorsIs(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}

func errorsIs(err error, target syscall.Errno) bool {
	var le *os.LinkError
	if e, ok := err.(*os.LinkError); ok {
		le = e
	}
	if le != nil {
		if errno, ok := le.Err.(syscall.Errno); ok {
			return errno == target
		}
	}
	if errno, ok := err.(syscall.Errno); ok {
		return errno == target
	}
	return false
}

// isDirLink reports whether what Lstat found is a link this tool could have
// made.
//
// On Unix that is simply ModeSymlink. On Windows it is NOT: Go reports a
// directory JUNCTION -- which is what linkDir falls back to, and therefore what
// a normal unprivileged install actually creates -- as a plain directory, with
// no ModeSymlink bit. Measured on a live box: `skill status` called its own
// freshly created link "a real directory, not a link from this tool", a second
// `skill install` refused as though somebody else's skill were in the way, and
// `skill uninstall` declined to remove it. The link worked; only our ability to
// RECOGNISE it was broken.
//
// So ask the filesystem for the attribute that actually distinguishes them.
func isDirLink(fi os.FileInfo, link string) bool {
	if fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	p, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// readDirLink returns what the link points at. os.Readlink understands a
// junction's reparse point; EvalSymlinks is the belt-and-braces fallback.
func readDirLink(link string) (string, error) {
	if p, err := os.Readlink(link); err == nil && p != "" {
		return p, nil
	}
	return filepath.EvalSymlinks(link)
}
