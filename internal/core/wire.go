// Package core owns the authoritative session tree, the terminal fanout hub,
// the viewport scheduler and per-connection seen state.
package core

import (
	"encoding/binary"
	"errors"

	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Binary data-plane framing (SPEC amendment A1).
//
// Server -> client:
//
//	 0        1                9          11                      N
//	 +--------+----------------+----------+-----------+-----------+
//	 | type u8| seq u64 BE     | tlen u16 | target    | payload   |
//	 +--------+----------------+----------+-----------+-----------+
//
//	type 1 = frame     payload = raw ANSI bytes
//	type 2 = snapshot  payload = raw ANSI bytes (full repaint)
//	type 3 = gap       payload = u64 BE bytes_dropped
//
// Client -> server:
//
//	0        1          3                  N
//	+--------+----------+---------+--------+
//	| type=16| tlen u16 | target  | bytes  |
//	+--------+----------+---------+--------+
//
// target is ASCII (a Herdr pane id). All integers are big-endian.
const (
	TypeFrame    byte = 1
	TypeSnapshot byte = 2
	TypeGap      byte = 3

	TypeInput byte = 16

	// ServerHeaderLen is the fixed part before target: type + seq + tlen.
	ServerHeaderLen = 1 + 8 + 2
	// ClientHeaderLen is the fixed part before target: type + tlen.
	ClientHeaderLen = 1 + 2
)

// ErrShortFrame means a binary frame was truncated.
var ErrShortFrame = errors.New("core: short binary frame")

// encodeFrame builds a complete server binary frame in a pooled buffer.
// The result is shared byte-for-byte by every subscriber and by the replay
// ring: no per-client copy exists anywhere on this path.
func encodeFrame(typ byte, seq uint64, target string, payload []byte) *upstream.Buf {
	total := ServerHeaderLen + len(target) + len(payload)
	b := upstream.GetBuf(total)
	out := b.B[:total]
	out[0] = typ
	binary.BigEndian.PutUint64(out[1:9], seq)
	binary.BigEndian.PutUint16(out[9:11], uint16(len(target)))
	n := copy(out[ServerHeaderLen:], target)
	copy(out[ServerHeaderLen+n:], payload)
	b.B = out
	return b
}

// encodeGap builds a type-3 gap frame.
func encodeGap(seq uint64, target string, dropped uint64) *upstream.Buf {
	var p [8]byte
	binary.BigEndian.PutUint64(p[:], dropped)
	return encodeFrame(TypeGap, seq, target, p[:])
}

// DecodeClientFrame parses a client binary frame, returning the target and a
// subslice of buf (no copy).
func DecodeClientFrame(buf []byte) (typ byte, target string, payload []byte, err error) {
	if len(buf) < ClientHeaderLen {
		return 0, "", nil, ErrShortFrame
	}
	typ = buf[0]
	tlen := int(binary.BigEndian.Uint16(buf[1:3]))
	if len(buf) < ClientHeaderLen+tlen {
		return 0, "", nil, ErrShortFrame
	}
	target = string(buf[ClientHeaderLen : ClientHeaderLen+tlen])
	payload = buf[ClientHeaderLen+tlen:]
	return typ, target, payload, nil
}

// FrameHeader is the parsed server header, exported for the test client and
// for anyone writing a Swift/Kotlin client against docs/api.md.
type FrameHeader struct {
	Type   byte
	Seq    uint64
	Target string
}

// DecodeServerFrame parses a server binary frame. payload aliases buf.
func DecodeServerFrame(buf []byte) (h FrameHeader, payload []byte, err error) {
	if len(buf) < ServerHeaderLen {
		return h, nil, ErrShortFrame
	}
	h.Type = buf[0]
	h.Seq = binary.BigEndian.Uint64(buf[1:9])
	tlen := int(binary.BigEndian.Uint16(buf[9:11]))
	if len(buf) < ServerHeaderLen+tlen {
		return h, nil, ErrShortFrame
	}
	h.Target = string(buf[ServerHeaderLen : ServerHeaderLen+tlen])
	return h, buf[ServerHeaderLen+tlen:], nil
}

// HeaderLenFor is the number of bytes a server frame reserves in front of the
// payload for a given target. Terminal streams decode base64 straight past this
// offset so the header can be stamped in place, with no copy.
func HeaderLenFor(target string) int { return ServerHeaderLen + len(target) }

// stampHeader writes the server header into the reserved prefix of b.
// b must be at least HeaderLenFor(target) long.
func stampHeader(b []byte, typ byte, seq uint64, target string) {
	b[0] = typ
	binary.BigEndian.PutUint64(b[1:9], seq)
	binary.BigEndian.PutUint16(b[9:11], uint16(len(target)))
	copy(b[ServerHeaderLen:], target)
}
