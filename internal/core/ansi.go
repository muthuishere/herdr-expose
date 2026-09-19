package core

import "strings"

// StripANSI removes terminal escape sequences from text, leaving readable
// plain characters.
//
// It exists even though every transcript read already asks Herdr for
// `format: "text"` with `strip_ansi: true`. That is a request, not a
// guarantee: the field is honoured by the Herdr build we happen to be talking
// to, and a transcript frame is CONTROL-PLANE JSON that the web client renders
// as text rather than feeding to an emulator. An escape sequence that survives
// into it is not a cosmetic wart, it is unrenderable garbage in the middle of
// the one view the phone has. Stripping twice costs a scan of a few kilobytes
// once a second; trusting upstream costs the whole view.
//
// It handles CSI (ESC [ ... final), OSC (ESC ] ... BEL | ST), the two-byte
// escapes, and the single-shift/charset selectors. Anything it does not
// recognise is dropped along with its ESC rather than emitted raw.
func StripANSI(s string) string {
	if !strings.ContainsAny(s, "\x1b\r\x07") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscape(s, i)
		case c == '\r':
			// A bare CR inside a captured buffer is a cursor move, not a line
			// break; a CRLF still ends the line via its LF.
			i++
		case c == 0x07:
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// skipEscape returns the index just past the escape sequence starting at i.
func skipEscape(s string, i int) int {
	i++ // the ESC itself
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		// CSI: parameter and intermediate bytes, then a final byte 0x40-0x7e.
		i++
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
			i++
		}
		if i < len(s) {
			i++ // the final byte
		}
		return i
	case ']', 'P', 'X', '^', '_':
		// OSC / DCS / SOS / PM / APC: run to BEL or ST (ESC \).
		i++
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		return i
	case '(', ')', '*', '+', '%', '#':
		// Charset designators take one more byte.
		i++
		if i < len(s) {
			i++
		}
		return i
	default:
		// Two-byte escape (ESC M, ESC 7, ESC =, ...).
		return i + 1
	}
}
