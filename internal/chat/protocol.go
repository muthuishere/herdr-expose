// Package chat defines what a chat adapter is and speaks to one.
//
// THE DIVISION OF LABOUR, which is the whole design:
//
// The PLATFORM decides what the text says. It strips the terminal's own
// furniture, folds code and diffs into "[12 lines of code]" markers, waits
// until the agent has stopped AND the screen has stopped moving, remembers
// what this reader already saw and sends only the new part, renders the menus,
// parses the commands, and formats every number, state and label in them. By
// the time anything reaches an adapter it is a FINISHED STRING.
//
// The ADAPTER moves bytes. It receives a string and delivers it to a chat
// app; it receives what a person typed and hands back the string. That is the
// entire job. An adapter does not know what a pane is, what an agent is, what
// `blocked` means, or that there is a terminal anywhere in the picture.
//
// It is drawn here rather than left to taste because it was drawn wrong twice.
// The cleaning lived in two clients and both grew the same bugs. Then the
// de-duplication was deleted as redundant and an answer arrived three times.
// Every one of those was a client deciding something the platform knew better.
// So: an adapter that formats is an adapter with a bug the next adapter will
// also have.
//
// WHY A SUBPROCESS. An adapter is a command -- "node telegram.js",
// "python3 discord.py", "./my-adapter" -- executed directly, never through a
// shell. The alternative was a JavaScript file in an embedded interpreter,
// which means an adapter can only do what the sandbox was taught to do, and
// the very first thing it needed was an HTTP client the sandbox deliberately
// did not have. A subprocess has the whole machine: node's fetch, python's
// requests, a static binary, whatever its author already knows. The cost is
// that chat needs an interpreter installed, which is a cost worth paying.
package chat

import "encoding/json"

// Protocol is newline-delimited JSON over the adapter's stdin and stdout.
//
// Chosen because it is the only wire format every language reads without a
// dependency: `JSON.parse(line)` in node, `json.loads(line)` in python, and a
// Scanner here. An adapter is a read loop and a print, and it must be
// possible to write one in an afternoon in a language nobody here chose.
//
// stderr is NOT part of the protocol. It is captured verbatim into our log,
// scrubbed of every registered secret, so an adapter can debug itself with a
// plain print without corrupting the stream it is speaking on.
const Protocol = "jsonl/1"

// Outbound is a frame the HOST sends to the adapter, on its stdin.
type Outbound struct {
	// Type is "send", "hello" or "shutdown".
	Type string `json:"type"`

	// Text is the finished message. It is already cleaned, folded, settled,
	// de-duplicated and formatted: send it as it is. Do not re-wrap it, do not
	// re-indent it, do not add a prefix, and do not truncate it except to
	// whatever hard limit your chat app enforces -- in which case say so in
	// your log, because a silently cut answer is worse than a split one.
	Text string `json:"text,omitempty"`

	// Thread is the conversation to deliver into, as the adapter last reported
	// it. Empty means "wherever you consider default" -- for a bot bound to
	// one chat, that chat.
	Thread string `json:"thread,omitempty"`

	// Choices are options the host is offering. Render them as buttons if the
	// app has buttons; otherwise ignore them entirely -- Text ALREADY contains
	// a readable rendering of the same options, because the platform formats
	// for the lowest common denominator first and lets a richer client
	// upgrade. An adapter that has no buttons is therefore not degraded, and
	// an adapter author never has to invent a text fallback.
	Choices []Choice `json:"choices,omitempty"`

	// Hello carries the handshake on Type == "hello".
	Hello *Hello `json:"hello,omitempty"`
}

// Choice is one option. Data is opaque: feed it back verbatim as the Text of
// an Inbound when the option is chosen, and the host will understand it. Never
// parse it, never construct one, never shorten it below your app's limit
// without reporting that you did -- a truncated Data is a command the host
// cannot read, which looks to the person like a button that does nothing.
type Choice struct {
	Label string `json:"label"`
	Data  string `json:"data"`
}

// Hello is the first frame the host sends, so an adapter can refuse a protocol
// it does not speak instead of misreading it.
type Hello struct {
	Protocol string `json:"protocol"`
	// AdapterID is the id from the config, for the adapter's own logging.
	AdapterID string `json:"adapter_id"`
	// Buttons tells the adapter nothing it must obey; it is here so an adapter
	// can log what the host believes about it.
	MaxTextBytes int `json:"max_text_bytes,omitempty"`
}

// Inbound is a frame the ADAPTER sends to the host, on its stdout.
type Inbound struct {
	// Type is "message" or "ready".
	Type string `json:"type"`

	// Text is exactly what the person typed, or -- when they tapped a button --
	// that Choice's Data, unmodified. Sending the Data is what makes a tap and
	// a typed command the same event to the host, so no command is implemented
	// twice and no adapter needs a parser.
	Text string `json:"text,omitempty"`

	// Thread is the conversation it came from, echoed back on later sends.
	Thread string `json:"thread,omitempty"`

	// From is a display name, used only in logs. Never required.
	From string `json:"from,omitempty"`
}

// Encode renders a frame as one protocol line, newline included.
func Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
