package core

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func newTestCleaner(t *testing.T) *Cleaner {
	t.Helper()
	c, bad := NewCleaner(DefaultChrome)
	if len(bad) != 0 {
		t.Fatalf("DefaultChrome does not compile: %v", bad)
	}
	return c
}

func TestCleanStripsChromeAndKeepsContent(t *testing.T) {
	c := newTestCleaner(t)
	in := strings.Join([]string{
		"Real content line one.",
		"",
		"────────────────────────────────",
		"❯ sto",
		"",
		"  Image in clipboard · ctrl+v to paste",
		"herdr-expose  ctx 550K 55% /clear soon",
		"▸▸ auto mode on (shift+tab to cycle)",
		"> a genuine quoted line",
		"Second real line.",
	}, "\n")

	got := c.Clean(in)
	want := "Real content line one.\n\n> a genuine quoted line\nSecond real line."
	if got.Text != want {
		t.Fatalf("cleaned text:\n got %q\nwant %q", got.Text, want)
	}
	if got.Chrome != 5 {
		t.Errorf("chrome removed = %d, want 5", got.Chrome)
	}
}

// ASCII '>' is a quote marker in real output and must survive; only the TUI's
// own carets are the input line.
func TestCleanKeepsAsciiQuote(t *testing.T) {
	c := newTestCleaner(t)
	got := c.Clean("> quoted\n❯ typed")
	if got.Text != "> quoted" {
		t.Fatalf("got %q, want %q", got.Text, "> quoted")
	}
}

// A line that survives must be byte-identical bar trailing padding: cleaning
// removes lines, it never rewrites them.
func TestCleanNeverRewritesASurvivingLine(t *testing.T) {
	c := newTestCleaner(t)
	in := "  indented keeps its indent   \n\tand its tab"
	got := c.Clean(in)
	want := "  indented keeps its indent\n\tand its tab"
	if got.Text != want {
		t.Fatalf("got %q, want %q", got.Text, want)
	}
}

func TestCleanCollapsesBlankRuns(t *testing.T) {
	c := newTestCleaner(t)
	got := c.Clean("a\n\n\n\n\nb\n\n\n")
	if got.Text != "a\n\nb" {
		t.Fatalf("got %q, want %q", got.Text, "a\n\nb")
	}
	// Four blanks between a and b, one of which survives as the paragraph
	// break, so three are collapsed; the three trailing ones are all dropped.
	if got.Blank != 6 {
		t.Errorf("blank collapsed = %d, want 6", got.Blank)
	}
}

// A screen that is nothing but furniture cleans to nothing, rather than to a
// pile of blank lines that still costs a notification.
func TestCleanAllChromeYieldsEmpty(t *testing.T) {
	c := newTestCleaner(t)
	got := c.Clean("──────────────\n❯\n\nctx 10K 1%\n")
	if got.Text != "" {
		t.Fatalf("got %q, want empty", got.Text)
	}
}

// One bad pattern must not disable the rest, and must not panic: these come
// from user config.
func TestNewCleanerSkipsBadPatternsAndKeepsGoodOnes(t *testing.T) {
	c, bad := NewCleaner([]string{`^KEEPOUT`, `([unclosed`})
	if len(bad) != 1 || bad[0] != `([unclosed` {
		t.Fatalf("bad = %v, want the one uncompilable pattern", bad)
	}
	if got := c.Clean("KEEPOUT me\nkeep me").Text; got != "keep me" {
		t.Fatalf("got %q, want %q", got, "keep me")
	}
}

// No patterns at all is a valid configuration: blanks still collapse, nothing
// is stripped.
func TestCleanWithNoPatterns(t *testing.T) {
	c, _ := NewCleaner(nil)
	got := c.Clean("────────\n\n\nreal")
	if got.Text != "────────\n\nreal" {
		t.Fatalf("got %q", got.Text)
	}
	if got.Chrome != 0 {
		t.Errorf("chrome = %d, want 0", got.Chrome)
	}
}

// The timing and progress lines an agent prints around a turn. They carry a
// clock and a token count, so they change every second while saying nothing —
// the most expensive kind of furniture to let through.
func TestCleanStripsTimingAndProgressLines(t *testing.T) {
	c := newTestCleaner(t)
	for _, line := range []string{
		"✻ Sautéed for 1m 6s · done 10:10 PM",
		"⁂ Crunched for 52s · done 12:31 PM",
		"* Baked for 13s · done 4:49 PM",
		"✽ Canoodling… (6m 39s · ↓ 28.1k tokens)",
		"  271.3k tokens",
	} {
		if got := c.Clean(line).Text; got != "" {
			t.Errorf("kept %q, want it stripped (got %q)", line, got)
		}
	}
}

// Prose that happens to mention a duration or a token count is CONTENT. Too
// little cleaning is untidy; too much is lying about what the pane said.
func TestCleanKeepsProseThatMentionsTime(t *testing.T) {
	c := newTestCleaner(t)
	for _, line := range []string{
		"The build takes about 30s on this machine.",
		"I ran it for 2m and it never finished.",
		"We are done with the migration.",
	} {
		if got := c.Clean(line).Text; got != line {
			t.Errorf("stripped %q, want it kept (got %q)", line, got)
		}
	}
}

// The reason this mode exists: a status bar that ticks changes the RAW screen
// every second while saying nothing. A raw subscriber is woken; a clean one
// must not be, because after cleaning the two screens are identical.
func TestCleanSuppressesATickingCounter(t *testing.T) {
	c := newTestCleaner(t)
	before := "Agent is thinking about the plan.\nherdr-expose  ctx 550K 55% /clear soon"
	after := "Agent is thinking about the plan.\nherdr-expose  ctx 551K 55% /clear soon"

	if before == after {
		t.Fatal("fixture is wrong: the raw screens must differ")
	}
	if a, b := c.Clean(before).Text, c.Clean(after).Text; a != b {
		t.Fatalf("cleaned screens differ and should not:\n %q\n %q", a, b)
	}
}

// Code is CONTENT, not furniture, so folding it must leave a visible marker.
// A reader has to be able to tell something was there.
func TestCleanDroppingCodeLeavesAMarker(t *testing.T) {
	c := newTestCleaner(t)
	in := strings.Join([]string{
		"I changed the build script:",
		"   7 -set -euo pipefail",
		"   8 -cd \"$(dirname \"$0\")\"",
		"   9 +PORT=8765",
		"That should fix the port clash.",
	}, "\n")

	got := c.CleanDroppingCode(in)
	want := "I changed the build script:\n[3 lines of code]\nThat should fix the port clash."
	if got.Text != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got.Text, want)
	}
	if got.Code != 3 {
		t.Errorf("Code = %d, want 3", got.Code)
	}
}

// A single code-shaped line is part of the sentence around it, not a block.
func TestCleanDroppingCodeKeepsLoneLines(t *testing.T) {
	c := newTestCleaner(t)
	in := "Run /usr/local/bin/thing to start it.\nThen tell me what it printed."
	if got := c.CleanDroppingCode(in).Text; got != in {
		t.Fatalf("got %q, want it untouched", got)
	}
}

// Plain Clean never folds code: the two levels must stay distinct.
func TestCleanKeepsCodeByDefault(t *testing.T) {
	c := newTestCleaner(t)
	in := "   7 -set -euo pipefail\n   8 -cd /tmp/x/y"
	got := c.Clean(in)
	if got.Text != in {
		t.Fatalf("got %q, want it untouched", got.Text)
	}
	if got.Code != 0 {
		t.Errorf("Code = %d, want 0", got.Code)
	}
}

// Settle: a working agent is held, and the screen that settles still counts as
// new. Holding must NOT record the hash, or the answer would be deduped away
// against the half-written screen that was never sent.
func TestSettleHoldsWorkingAndReleasesOnSettle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &countingSink{}
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "s/w1:p1"
	p := h.transcript
	p.subscribe(target, sess, 2, true)

	mk := func(text string) ([3]TranscriptFrame, [3]string) {
		f := TranscriptFrame{Target: target, Text: text}
		return [3]TranscriptFrame{f, f, f}, [3]string{text, text, text}
	}

	fr, id := mk("half written")
	p.deliver(target, fr, id, "working")
	if n := sink.count("transcript"); n != 0 {
		t.Fatalf("delivered %d frames while working, want 0", n)
	}

	// The agent stops, but the screen has just changed -- it may still be
	// painting. One poll of stillness is required before it counts as settled.
	fr, id = mk("the finished answer")
	p.deliver(target, fr, id, "idle")
	if n := sink.count("transcript"); n != 0 {
		t.Fatalf("delivered %d on the first settled poll, want 0 — the screen had just changed", n)
	}

	// Unchanged on the next poll: now it is genuinely still, and it goes once.
	p.deliver(target, fr, id, "idle")
	if n := sink.count("transcript"); n != 1 {
		t.Fatalf("delivered %d once the screen was still, want 1", n)
	}

	// And it is not sent again.
	p.deliver(target, fr, id, "idle")
	if n := sink.count("transcript"); n != 1 {
		t.Fatalf("delivered %d after settling, want it deduped to 1", n)
	}
}

// The symptom this prevents, as measured on a live agent: an agent reporting
// `done` whose transcript still grew 29 -> 85 -> 86 lines over three polls.
// Each screen was legitimately different, so each was sent, and eight
// near-identical messages arrived for one answer.
func TestSettleHoldsAScreenThatIsStillPainting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &countingSink{}
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "s/w1:p1"
	p := h.transcript
	p.subscribe(target, sess, 2, true)

	// Three different screens in a row, agent already done the whole time.
	for _, text := range []string{"29 lines", "85 lines", "86 lines"} {
		f := TranscriptFrame{Target: target, Text: text}
		p.deliver(target, [3]TranscriptFrame{f, f, f}, [3]string{text, text, text}, "done")
	}
	if n := sink.count("transcript"); n != 0 {
		t.Fatalf("delivered %d while the screen was still growing, want 0", n)
	}

	// It stops growing.
	f := TranscriptFrame{Target: target, Text: "86 lines"}
	p.deliver(target, [3]TranscriptFrame{f, f, f}, [3]string{"86 lines", "86 lines", "86 lines"}, "done")
	if n := sink.count("transcript"); n != 1 {
		t.Fatalf("delivered %d once it stopped, want exactly 1", n)
	}
}

// Switching flavour on a LIVE subscription must deliver again, even though the
// pane has not changed.
//
// Dedup remembers the hash of what this subscriber last received. After a
// switch the next frame is a DIFFERENT SHAPE, so that memory is about text the
// subscriber will never see again — and leaving it in place strands the client
// on a blank view until the pane happens to change, which on an idle agent can
// be forever.
func TestSwitchingFlavourDeliversAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &countingSink{}
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "s/w1:p1"
	p := h.transcript

	// Three flavours of one unchanged screen, as the poller would build them.
	raw := "hello\n──────────\nworld"
	frames := [3]TranscriptFrame{
		{Target: target, Text: raw},
		{Target: target, Text: "hello\nworld"},
		{Target: target, Text: "hello\nworld"},
	}
	ids := [3]string{raw, "hello\nworld", "hello\nworld"}

	p.subscribe(target, sess, 0, false) // raw
	p.deliver(target, frames, ids, "idle")
	if n := sink.count("transcript"); n != 1 {
		t.Fatalf("raw delivered %d, want 1", n)
	}

	// Same screen again: deduped, as it should be.
	p.deliver(target, frames, ids, "idle")
	if n := sink.count("transcript"); n != 1 {
		t.Fatalf("unchanged screen delivered %d, want it suppressed", n)
	}

	// Switch to prose. The pane has NOT changed, but the shape has.
	p.subscribe(target, sess, 2, false)
	p.deliver(target, frames, ids, "idle")
	if n := sink.count("transcript"); n != 2 {
		t.Fatalf("after switching flavour delivered %d, want 2 — the client is stranded", n)
	}
}

// The same guarantee for the settle flag, which is a delivery policy rather
// than a text one and has its own branch.
func TestSwitchingSettleDeliversAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(NewStore(log), log)
	sink := &countingSink{}
	sess := h.NewSession(ctx, sink)
	defer sess.Close()

	const target = "s/w1:p1"
	p := h.transcript
	f := TranscriptFrame{Target: target, Text: "settled output"}
	frames := [3]TranscriptFrame{f, f, f}
	ids := [3]string{"x", "x", "x"}

	p.subscribe(target, sess, 2, false)
	p.deliver(target, frames, ids, "idle")
	if n := sink.count("transcript"); n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}

	p.subscribe(target, sess, 2, true) // same text level, settle turned on
	p.deliver(target, frames, ids, "idle")
	if n := sink.count("transcript"); n != 2 {
		t.Fatalf("after turning settle on delivered %d, want 2", n)
	}
}
