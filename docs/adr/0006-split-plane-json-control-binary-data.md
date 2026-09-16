# 6. Split plane: JSON control frames, binary data frames

Status: Accepted (supersedes the earlier "JSON over WebSocket for v1" draft)

## Context

Two earlier drafts were both wrong.

Protobuf everywhere buys schema enforcement across independently-built clients,
at the cost of codegen in the loop of every field change in two languages, and
an opaque wire nobody can read in devtools.

JSON everywhere is readable, but it puts **base64 on the hot path**: terminal
bytes inflate ~33% before compression, and every single frame costs a JSON parse
on a phone, over cellular, at 60 frames a second during a burst of agent output.
This is a Go, performance-first product; that overhead is precisely the thing we
exist to not have.

Control messages are a different problem entirely. They are rare, structurally
rich, and change often during development. JSON is the right answer for them and
a bad answer for terminal bytes.

## Decision

Split the plane. One WebSocket, two frame kinds, distinguished by the WebSocket
opcode itself — no negotiation, no capability handshake, no fallback.

**Control plane — JSON, WebSocket TEXT frames.**
`hello` `subscribe` `unsubscribe` `viewport` `command` `ping` up;
`welcome` `tree` `agent` `result` `pong` `gap` `closed` down. Shape as SPEC §3.

**Data plane — WebSocket BINARY frames, fixed header:**

```
 0        1                9          11                    N
 +--------+----------------+----------+----------+----------+
 | type u8| seq u64 BE     | tlen u16 | target   | payload  |
 +--------+----------------+----------+----------+----------+
 type: 1=frame  2=snapshot  3=gap (payload = u64 BE bytes_dropped)
```

`target` is ASCII, `tlen` bytes long. `payload` is **raw ANSI bytes** straight
from the pane. Client input is binary too:
`[type=16][tlen u16][target][raw bytes]` — no JSON encode on the keystroke path.

Base64 exists at exactly **one** place in this system: decoding Herdr's NDJSON
in `internal/upstream`. After that it is `[]byte` all the way to the socket, with
zero re-encoding. LIVE streams are per connection (ADR 0007), so each frame is
encoded once for the connection that owns it and never copied again on the way
out.

Per-message deflate is enabled. Terminal output compresses ~10x, and that is
what makes a tunnel usable on cellular.

### Why a fixed header and not protobuf

Any language parses this in about ten lines — one byte, a big-endian u64, a
u16, a slice, a slice. No codegen step, no dependency, no build plugin, nothing
to keep in sync. The mobile client (ADR 0015) is cheap to write *because* the
data plane is trivial, and the control plane stays legible in devtools where it
is worth the most. Protobuf would tax both planes to fix a problem — cross-team
schema drift — that a frozen 11-byte header does not have.

## Consequences

- Zero base64 on the wire, and one less parse per frame on the client.
- Two code paths on both sides instead of one. The binary one is ~50 lines and
  never changes; the JSON one absorbs all the churn.
- The data plane is frozen by design. A new data frame type means a new `type`
  byte, not a new header layout.
- `seq` is monotonic per connection and carried in the binary header for
  ordering and debugging only — it is **not** a resume cursor (ADR 0016).
- Debugging terminal bytes needs a hex dump rather than devtools' JSON view.
  Worth it; `docs/api.md` carries a worked hex example so nobody has to guess.

## Alternatives considered

The Node reference implementation (`herdr-remote`) also reaches for binary
output frames, but as a **negotiated capability** (`binary_frame_v2`) with a v1
framing and a JSON path still live alongside it, accepting all of them on
receipt. That is the correct call for a deployed relay that must not break
older hosts. We have no installed base, so we take the one-shot freedom: a
single mandatory framing, no negotiation, no fallback to maintain, and no
ambiguity for a client author to get wrong.
