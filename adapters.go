package herdrexpose

import (
	"embed"
	"io/fs"
	"sort"
)

// The bundled adapters, compiled into the binary.
//
// They are BUNDLED, not built in. Nothing in this binary knows what Telegram
// is: telegram.js is an ordinary chat adapter that ships with us because a
// working example beats a specification, and it is the same kind of file as
// the one somebody writes for Discord. Shipping it inside the binary means
// `chat` works from a clean install with no network fetch and no repository
// checkout, and the copy on disk can then be edited without a rebuild.
//
//go:embed adapters/chat/*.js
var bundledAdapters embed.FS

// BundledChatAdapters returns the bundled chat adapters as filename -> source.
//
// The names are bare basenames ("telegram.js"), which is what gets written
// into the adapters directory, so a config entry naming "telegram" and a file
// on disk called telegram.js are obviously the same thing.
func BundledChatAdapters() map[string][]byte {
	out := map[string][]byte{}
	entries, err := fs.ReadDir(bundledAdapters, "adapters/chat")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(bundledAdapters, "adapters/chat/"+e.Name())
		if err != nil {
			continue
		}
		out[e.Name()] = b
	}
	return out
}

// BundledChatAdapterNames lists the bundled adapters by id (no .js), sorted.
func BundledChatAdapterNames() []string {
	var names []string
	for f := range BundledChatAdapters() {
		names = append(names, f[:len(f)-len(".js")])
	}
	sort.Strings(names)
	return names
}
