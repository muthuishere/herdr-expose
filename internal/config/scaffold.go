package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// The config documents itself (AMENDMENTS 11 §H1).
//
// First run writes EVERY section and EVERY key with its default value and a
// one-line comment. A key that exists but is not written is a key nobody will
// ever find, so the subsystems nobody would guess at — [share] and [log] —
// are written too, at their defaults, so their existence is discoverable
// without reading the source.
//
// The scaffold is stored as an ORDERED LIST OF BLOCKS rather than one string,
// because upgrading an existing file must be APPEND-ONLY: a new release that
// introduces [log] appends the [log] block to the end of the user's file and
// touches nothing above it. Their comments, their key order and their unknown
// keys all survive byte-for-byte. See EnsureSections.

// scaffoldHeader is written once, at the top of a brand-new file.
const scaffoldHeader = `# herdr-expose configuration — every section and every key it understands, with
# its default value. Keys you do not need can simply be deleted: missing keys
# fall back to these defaults. Keys this binary has never heard of are kept
# verbatim, and a NEW section added by a future release is APPENDED to the end
# of this file, so hand-edits above it are never rewritten or reordered.
#
# There are NO SECRETS in this file and there never will be. The server token
# and every device token live only as SHA-256 hashes in the 0600 state file,
# and the Cloudflare API token is read from $CLOUDFLARE_ALLPURPOSE_TOKEN at the
# moment it is used. This file is safe to paste into an issue.
`

// section is one top-level table of the scaffold.
type section struct {
	name string // the top-level TOML key, e.g. "log"
	body string // the block as it is written to disk, comments and all
}

// scaffoldSections is the whole config, in file order (AMENDMENTS 11 §H2).
var scaffoldSections = []section{
	{"server", `
[server]
port = 21118                        # the HTTP/WebSocket port (AMENDMENTS 6)
# There is deliberately NO 'bind' key — the exposure mode below decides the bind address.
# Loopback for local and for the tunnel modes, 0.0.0.0 for lan. A legacy 'bind'
# key is ignored on load and is never written back.
# allowed_origins = []              # extra browser Origins to accept; the mode's own origin is always allowed
`},

	{"auth", `
# Auth policy only — no credentials. A device token is required in every mode
# except local (loopback), where Origin and Host pinning replace it.
[auth]
pairing_ttl_seconds = 600           # how long a pairing code stays valid (10 minutes)
max_devices = 32                    # paired-device cap; least-recently-seen is evicted
device_ttl_days = 30                # sliding expiry of a device token
pair_attempts_per_minute = 10       # rate limit on POST /v1/pair, per remote address
handshake_attempts_per_minute = 60  # rate limit on the websocket handshake, per remote address
`},

	{"ui", `
[ui]
theme = "auto"                      # auto | light | dark
default_view = "grid"               # grid | focus
`},

	{"expose", `
# Exposure. The happy path is two lines and zero JavaScript:
#
#   cloudflare = true
#   domain = "herdr.example.com"
#
# herdr-expose then creates or reuses a named Cloudflare tunnel, mints its
# credentials through the API (so 'cloudflared login' and its browser prompt
# are never needed), upserts the proxied CNAME, runs cloudflared and restarts
# it if it dies. The API token is read from $CLOUDFLARE_ALLPURPOSE_TOKEN at the
# moment it is used: never stored here, never written to state, never logged
# and never returned by 'status'.
[expose]
cloudflare = false                  # public https on a static domain you own
domain = ""                         # REQUIRED when cloudflare = true, e.g. "herdr.example.com"
tunnel_name = "herdr-expose"        # the named Cloudflare tunnel to create or reuse
ngrok = false                       # alternative provider; domain must be a RESERVED ngrok domain
lan = false                         # bind 0.0.0.0 so the wifi can reach this machine; also the automatic fallback when cloudflared is missing
autostart = false                   # bring the tunnel up when the server starts
# adapter = "my-tunnel"             # escape hatch: run a JS adapter instead of a built-in provider
#
# [[expose.adapters]]
# id = "my-tunnel"
# script = "adapters/template.js"
# [expose.adapters.env]
# hostname = "herdr.example.com"
#
# lan = true is safe because auth is never optional and the pairing code is
# shown ONLY on this machine's screen — no HTTP endpoint mints or displays one,
# so a neighbour on the network can reach the port and get nowhere. Note that
# plain HTTP on a LAN IP is not a secure context: the web app works, but the
# PWA cannot be installed and there is no service worker.
`},

	{"share", `
# Defaults for 'herdr-expose share' — one scoped, time-boxed, self-destructing
# tunnel onto ONE session. Every share expires; there is no permanent share, a
# long one is just --days 30 (AMENDMENTS 10).
#
# FOUR RUNGS (AMENDMENTS 16). The ladder is climbed, never guessed:
#   (none)     http://<lan-ip>:<port>        this machine + this network  <- DEFAULT
#                                            (plain HTTP on a LAN IP is not a
#                                            secure context: no PWA install)
#   --quick    https://<random>.trycloudflare.com    anyone on the internet
#              no account, no zone, no DNS, no API token, nothing to clean up.
#              The hostname is NEW every time, so paired device tokens (which
#              are origin-bound) do not carry over to the next quick share.
#   --domain   https://x.you.com             the internet, on a name you own
#   --local    http://127.0.0.1:<port>       this machine only; an opt-IN for
#                                            testing, not the default — a share
#                                            nobody else can open is not a share
#
# A failed EXPLICIT request degrades LOUDLY (--quick with no cloudflared falls
# back to lan, and says why). Nothing ever escalates ABOVE what was asked for:
# a bare 'share' never becomes a tunnel because [expose] happens to have a
# domain configured. This binary runs arbitrary commands in your agent
# sessions, so the step from "this room" to "the entire internet" is a flag you
# typed, never a key in this file you forgot about.
#
# [expose] above keeps its own default of LOCAL, deliberately: the daemon
# serves whoever is sitting at this machine, a share exists to be reached from
# elsewhere. Same principle, different job.
#
# The quick tunnel is confined to shares on purpose: [expose] keeps its static
# domain so the daily driver stays installable and paired (C1).
[share]
domain_suffix = ""                  # with default_mode = "domain": 'share --name review' -> review.<suffix>
default_hours = 1                   # TTL when neither --hours nor --days is given
default_mode = "lan"                # local | lan | quick | domain — the rung a bare 'share' climbs to.
                                    # "lan" is the shipped default; change it only if you want a
                                    # different PERSONAL default. ("auto" from before AMENDMENTS 16
                                    # still loads and now means "lan".)
max_concurrent = 10                 # refuse to create more than this many live shares
`},

	{"log", `
# The daemon's log. This is how you find out what a DETACHED daemon did, so it
# is a real FILE by default, not stderr: stderr from a forked, session-leader
# process goes nowhere anybody can find.
#
# Read it back with 'herdr-expose logs [--follow] [-n N] [--json]', and a
# share's own log with 'herdr-expose logs --share <id>'.
#
# Terminal output is NEVER written here, at any level including debug: pane
# bytes are the contents of your screen and can contain anything. Neither are
# credentials — the Cloudflare API token, device tokens and pairing codes are
# logged as ids and hashes, never as values. Both are enforced in code.
[log]
level = "info"                      # debug | info | warn | error
format = "text"                     # text | json
file = ""                           # empty = <state dir>/herdr-expose.log; an explicit path overrides
max_size_mb = 10                    # rotate past this size (0 disables rotation)
keep = 3                            # rotated files retained: herdr-expose.log.1 .. .3
`},
}

// DefaultFileContents is the complete scaffold: what a first run writes and
// what 'herdr-expose config print-default' prints.
func DefaultFileContents() string {
	var b strings.Builder
	b.WriteString(scaffoldHeader)
	for _, s := range scaffoldSections {
		b.WriteString(s.body)
	}
	return b.String()
}

// MissingSections lists the scaffold sections absent from a config document,
// in scaffold order.
func MissingSections(data []byte) ([]string, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range scaffoldSections {
		if _, ok := doc[s.name]; !ok {
			out = append(out, s.name)
		}
	}
	return out, nil
}

// EnsureSections brings an existing config up to the current scaffold by
// APPENDING the blocks it is missing — and only by appending.
//
// This is the whole contract of AMENDMENTS 11: adding a section must never
// rewrite or reorder what the user has already edited. Everything before the
// append point is preserved byte for byte, including comments, key order and
// keys this binary has never heard of. It returns the names of the sections it
// added.
//
// It is best-effort by design: a read-only or root-owned config is not a reason
// to refuse to start, so callers that only want to READ the config ignore the
// error.
func EnsureSections(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	missing, err := MissingSections(data)
	if err != nil || len(missing) == 0 {
		return nil, err
	}

	var add bytes.Buffer
	add.Write(data)
	// Guarantee the appended table header starts on a line of its own, or it
	// would be swallowed by the last key of the previous table.
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		add.WriteString("\n")
	}
	add.WriteString("\n# --- added by herdr-expose " + upgradeMarker + " ---\n")
	for _, name := range missing {
		add.WriteString(bodyOf(name))
	}

	merged := add.Bytes()
	// Never append something that does not parse, and never append something
	// that changes how the existing keys are read.
	if err := verifyAppendSafe(data, merged); err != nil {
		return nil, err
	}
	if err := writeFile0600(path, merged); err != nil {
		return nil, err
	}
	return missing, nil
}

// upgradeMarker labels the appended block so it is obvious where it came from.
const upgradeMarker = "— new defaults, your edits above are untouched"

func bodyOf(name string) string {
	for _, s := range scaffoldSections {
		if s.name == name {
			return s.body
		}
	}
	return ""
}

// verifyAppendSafe re-reads both documents and refuses the write unless every
// table the user already had decodes to exactly the same thing.
func verifyAppendSafe(before, after []byte) error {
	var b, a map[string]any
	if err := toml.Unmarshal(before, &b); err != nil {
		return err
	}
	if err := toml.Unmarshal(after, &a); err != nil {
		return fmt.Errorf("appending the missing config sections would produce invalid TOML: %w", err)
	}
	for k, bv := range b {
		av, ok := a[k]
		if !ok {
			return fmt.Errorf("refusing to rewrite config: table %q would be lost", k)
		}
		if fmt.Sprintf("%#v", bv) != fmt.Sprintf("%#v", av) {
			return fmt.Errorf("refusing to rewrite config: table %q would change", k)
		}
	}
	return nil
}
