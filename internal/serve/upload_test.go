package serve

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func png() []byte  { return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...) }
func jpeg() []byte { return append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{0}, 64)...) }

// The extension is a claim the uploader makes; the bytes are what the file IS.
// A shell script called screenshot.png must be refused, and the name must not
// even be consulted -- so this passes a perfectly innocent name with hostile
// content and expects a refusal.
func TestOnlyRealImagesAreAccepted(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body []byte
		ok   bool
		mime string
	}{
		{"png", png(), true, "image/png"},
		{"jpeg", jpeg(), true, "image/jpeg"},
		{"gif", append([]byte("GIF89a"), make([]byte, 32)...), true, "image/gif"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 32)...), true, "image/webp"},
		{"heic", append([]byte("\x00\x00\x00\x18ftypheic"), make([]byte, 32)...), true, "image/heic"},
		{"avif", append([]byte("\x00\x00\x00\x18ftypavif"), make([]byte, 32)...), true, "image/avif"},
		{"shell script", []byte("#!/bin/sh\nrm -rf ~\n"), false, ""},
		{"elf binary", []byte("\x7fELF\x02\x01\x01\x00padding"), false, ""},
		{"html", []byte("<html><script>alert(1)</script>"), false, ""},
		{"empty", nil, false, ""},
		{"mp4, a media file but not an image", append([]byte("\x00\x00\x00\x18ftypmp42"), make([]byte, 32)...), false, ""},
	} {
		_, _, ok := SniffImage(tc.body)
		if ok != tc.ok {
			t.Fatalf("%s: accepted=%v, want %v", tc.name, ok, tc.ok)
		}
		if !tc.ok {
			if _, _, err := SaveUpload(dir, "s", tc.body); err == nil {
				t.Fatalf("%s: SaveUpload accepted it", tc.name)
			}
		}
	}
}

// A session name arrives from the client, so it must not be able to steer the
// write out of the upload directory.
func TestASessionNameCannotEscapeTheUploadDirectory(t *testing.T) {
	base := t.TempDir()
	for _, bad := range []string{
		"../../etc", "..", ".", "/etc/passwd", "a/../../b", "", "...",
		"x\x00y", "~/.ssh", "C:\\Windows\\System32",
	} {
		p, _, err := SaveUpload(base, bad, png())
		if err != nil {
			t.Fatalf("%q: %v", bad, err)
		}
		rel, err := filepath.Rel(base, p)
		// A LITERAL ".." segment is the escape; "..-..-etc" merely starts with
		// the same two characters and is an ordinary directory name.
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("%q escaped the base: %s", bad, p)
		}
		if strings.HasPrefix(filepath.Base(filepath.Dir(p)), ".") {
			t.Fatalf("%q produced a hidden directory: %s", bad, p)
		}
	}
}

// 0600 in a 0700 directory, never executable, and named by the server.
func TestUploadsAreNotReadableByOthersAndNeverExecutable(t *testing.T) {
	base := t.TempDir()
	p, mime, err := SaveUpload(base, "work", png())
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q", mime)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("path %q is not absolute; an agent cannot open it", p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", di.Mode().Perm())
	}
	if !strings.HasSuffix(p, ".png") {
		t.Fatalf("path %q should carry the sniffed extension", p)
	}
}

// Two uploads in the same second must not collide: the name carries random
// bytes, and the create is O_EXCL so a collision would error rather than
// silently overwrite somebody's screenshot.
func TestTwoUploadsInTheSameSecondDoNotCollide(t *testing.T) {
	base := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, _, err := SaveUpload(base, "work", png())
		if err != nil {
			t.Fatal(err)
		}
		if seen[p] {
			t.Fatalf("duplicate path %s", p)
		}
		seen[p] = true
	}
}

func TestOversizeIsRefused(t *testing.T) {
	base := t.TempDir()
	big := append(png(), bytes.Repeat([]byte{7}, MaxUploadBytes)...)
	if _, _, err := SaveUpload(base, "work", big); err != ErrTooLarge {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// A hand-off, not storage: yesterday's screenshot is gone, today's is not.
func TestSweepRemovesOldUploadsAndKeepsFresh(t *testing.T) {
	base := t.TempDir()
	fresh, _, err := SaveUpload(base, "work", png())
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := SaveUpload(base, "work", png())
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * UploadTTL)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if n := SweepUploads(base, UploadTTL); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the old upload survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("the fresh upload was swept")
	}
}
