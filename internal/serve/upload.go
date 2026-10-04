package serve

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Image uploads: how a phone gets a screenshot in front of an agent.
//
// The bytes do NOT travel the prompt channel. A prompt is text an agent reads;
// an image is a file an agent opens, and the only thing that belongs in the
// prompt box is the PATH. So the bytes go over their own authenticated HTTP
// endpoint, the server writes the file on the machine that owns the pane, and
// the path comes back for the client to insert. Nothing is auto-submitted:
// what to say about the image is the person's business.
//
// What is refused, and why each one:
//
//   - Anything but an image, decided by MAGIC BYTES. An extension is a claim
//     the uploader makes; the first few bytes are what the file IS. The server
//     never trusts the name it was given -- it does not use it at all.
//   - Anything over MaxUploadBytes, measured as it is read rather than taken
//     from Content-Length, which is also just a claim.
//   - A session name that could escape its directory. The name comes from the
//     client, so it is reduced to a safe token before it is ever joined to a
//     path.
//
// Files are 0600 inside a 0700 directory, named by the server from a timestamp
// and random bytes, and swept after UploadTTL. Nothing here ever makes a file
// executable, and nothing ever runs one.

const (
	// MaxUploadBytes caps one upload. A phone screenshot is ~2-6MB; 20 leaves
	// room for a photo without inviting someone to fill the disk.
	MaxUploadBytes = 20 << 20
	// UploadTTL is how long an uploaded file survives. It is a hand-off to an
	// agent, not storage: a day is generous for "look at this".
	UploadTTL = 24 * time.Hour
	// UploadDirName is the directory under the system temp dir.
	UploadDirName = "herdr-expose-uploads"
)

// ErrNotAnImage is returned when the bytes are not a recognised image.
var ErrNotAnImage = errors.New("not an image")

// ErrTooLarge is returned when an upload exceeds MaxUploadBytes.
var ErrTooLarge = fmt.Errorf("image is larger than %d bytes", MaxUploadBytes)

// imageKinds maps a magic-byte signature to its MIME type and extension.
//
// http.DetectContentType knows png/jpeg/gif/bmp/webp but reports no extension
// and does not know heic or avif, which is what an iPhone camera roll holds.
// So the table is explicit: it is short, it is the security boundary, and a
// reader can check every entry against the format's spec.
var imageKinds = []struct {
	mime, ext string
	match     func([]byte) bool
}{
	{"image/png", "png", func(b []byte) bool { return has(b, "\x89PNG\r\n\x1a\n") }},
	{"image/jpeg", "jpg", func(b []byte) bool { return has(b, "\xff\xd8\xff") }},
	{"image/gif", "gif", func(b []byte) bool { return has(b, "GIF87a") || has(b, "GIF89a") }},
	{"image/bmp", "bmp", func(b []byte) bool { return has(b, "BM") }},
	{"image/tiff", "tiff", func(b []byte) bool { return has(b, "II*\x00") || has(b, "MM\x00*") }},
	// RIFF container: "RIFF" .... "WEBP".
	{"image/webp", "webp", func(b []byte) bool {
		return len(b) >= 12 && has(b, "RIFF") && string(b[8:12]) == "WEBP"
	}},
	// ISO-BMFF brands, at offset 4 after the box size.
	{"image/heic", "heic", func(b []byte) bool { return ftyp(b, "heic", "heix", "hevc", "mif1", "heim") }},
	{"image/avif", "avif", func(b []byte) bool { return ftyp(b, "avif", "avis") }},
}

func has(b []byte, sig string) bool {
	return len(b) >= len(sig) && string(b[:len(sig)]) == sig
}

func ftyp(b []byte, brands ...string) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	for _, br := range brands {
		if string(b[8:12]) == br {
			return true
		}
	}
	return false
}

// SniffImage reports the MIME type and extension of an image from its leading
// bytes, and whether it is an image at all. The caller's claimed filename and
// Content-Type are deliberately not consulted.
func SniffImage(b []byte) (mime, ext string, ok bool) {
	for _, k := range imageKinds {
		if k.match(b) {
			return k.mime, k.ext, true
		}
	}
	return "", "", false
}

// safeSegment reduces a client-supplied name to one path segment that cannot
// escape its parent: letters, digits, dot, dash and underscore, with any run of
// anything else collapsed to a single dash, and "." and ".." made impossible.
func safeSegment(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	// Leading dots go too. "../../etc" already reduces to the harmless literal
	// "..-..-etc", but a name starting with a dot is a hidden directory, and a
	// session should not be able to ask for one by accident.
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "unknown"
	}
	return out
}

// UploadDir is the base directory uploads are written under.
func UploadDir() string { return filepath.Join(os.TempDir(), UploadDirName) }

// SaveUpload validates body as an image and writes it under base, in a
// subdirectory named for the session. It returns the ABSOLUTE path on THIS
// machine, which is the only thing the client is told.
func SaveUpload(base, session string, body []byte) (path, mime string, err error) {
	if len(body) > MaxUploadBytes {
		return "", "", ErrTooLarge
	}
	mime, ext, ok := SniffImage(body)
	if !ok {
		return "", "", ErrNotAnImage
	}
	dir := filepath.Join(base, safeSegment(session))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("%s-%s.%s",
		time.Now().UTC().Format("20060102-150405"), hex.EncodeToString(r[:]), ext)
	p := filepath.Join(dir, name)
	// O_EXCL: the name is ours and must never land on an existing file.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(p)
		return "", "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(p)
		return "", "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return abs, mime, nil
}

// SweepUploads deletes uploads older than ttl, and any session directory left
// empty. It never touches anything outside base.
func SweepUploads(base string, ttl time.Duration) int {
	cutoff := time.Now().Add(-ttl)
	removed := 0
	dirs, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		sub := filepath.Join(base, d.Name())
		ents, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		left := 0
		for _, e := range ents {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
				if os.Remove(filepath.Join(sub, e.Name())) == nil {
					removed++
					continue
				}
			}
			left++
		}
		if left == 0 {
			_ = os.Remove(sub)
		}
	}
	return removed
}
