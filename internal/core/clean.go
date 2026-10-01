package core

import (
	"fmt"
	"regexp"
	"strings"
)

// Cleaning a captured SCREEN into something a chat app or a phone can read.
//
// WHY THIS IS SERVER-SIDE, when the rule everywhere else is that we expose the
// screen and invent nothing.
//
// It had been written twice — once in the web client, once in a chat bridge —
// and both copies grew the same subtle bugs. A third client would have guessed
// at it a third time. The transform is PURE: the same screen always cleans to
// the same text, with no per-client state, so there is exactly one right answer
// and no reason for every client to derive it again.
//
// What stays client-side is the other half: "what have I already shown THIS
// reader". That needs per-connection memory, and per-client buffers are what
// `seq is not a resume cursor` exists to avoid.
//
// NOTHING HERE IS ON BY DEFAULT. A subscriber asks for `format: "clean"`, and
// the frame reports what was removed, so no client is silently handed a
// different shape than it asked for.
//
// WHY CHROME IS CONFIGURABLE. `ctx 550K 55%` and `shift+tab to cycle` are
// Claude Code's status bar, not a universal truth. Baking each agent's
// furniture into this file makes the server grow knowledge of every TUI it
// might meet, and that rots the first time one of them redesigns. The patterns
// are config with sane defaults, so a new agent's banner is a config line
// rather than a release.

// DefaultChrome is the furniture a terminal draws around an agent's output.
//
// It is deliberately conservative: everything here is unambiguously the TUI
// talking about itself. When in doubt a line is CONTENT — too little cleaning
// is untidy, too much is lying about what the pane said.
var DefaultChrome = []string{
	// A rule: eight or more of the box-drawing, dash or underscore family and
	// nothing else. 119 of them wrap into six lines of noise on a phone.
	`^[\s\x{2500}-\x{257f}_=\-\x{2014}\x{2013}]{8,}$`,
	// The input line. U+276F and U+203A are the TUI's own prompt carets, and
	// what follows is what somebody is TYPING, not what the agent said. ASCII
	// '>' is deliberately absent: it is a quote marker in real output.
	`^\s*[\x{276f}\x{203a}]`,
	// A context meter. It ticks constantly, which is what makes it expensive:
	// it changes the screen while saying nothing.
	`(?i)ctx\s+[\d.]+[KM]?\s+\d+%`,
	// Mode banners.
	`(?i)\b(auto mode|bypass permissions|accept edits)\b.*\(`,
	`(?i)shift\+tab to cycle`,
	`(?i)^\s*Image in clipboard`,
	// The work-and-timing line an agent prints under a finished turn:
	// "✻ Sautéed for 1m 6s · done 10:10 PM". The adjective is randomised, so
	// this matches the SHAPE — a word, "for", a duration, then "done" — rather
	// than a vocabulary that would need chasing forever.
	`(?i)\bfor\s+(\d+h\s*)?(\d+m\s*)?\d+s\b.*·.*\bdone\b`,
	// The same line while it is still running: "✽ Canoodling… (6m 39s · ↓ 28.1k
	// tokens)". It updates every second, so it is the most expensive of all of
	// these to let through.
	`(?i)·\s*[\x{2193}v]?\s*[\d.]+k?\s*tokens`,
	`(?i)^\s*\S?\s*\w+\x{2026}\s*\(`,
	// A bare token tally on its own line. Anchored at both ends on purpose:
	// prose that mentions a token count ("it used 500 tokens to do that") is
	// content and must survive.
	`(?i)^\s*[\d.]+\s*k?\s*tokens\s*$`,
}

// Code-ish line shapes. A terminal gives one stream with prose and code in it,
// and a chat app can render only one of them well.
var (
	// A line-number gutter, with or without a diff marker. Note the two minus
	// signs: a terminal diff uses U+2212 MINUS SIGN, not ASCII hyphen, so
	// matching only '-' misses every removed line.
	reGutter = regexp.MustCompile(`^\s*\d+\s+[-+\x{2212}\x{00b1}]?\s?`)
	reDiff   = regexp.MustCompile(`^\s*[-+\x{2212}]\s?\S`)
	reBare   = regexp.MustCompile(`^\s*\d+$`)
	reShell  = regexp.MustCompile(`^\s*[$#]\s\S`)
	rePath   = regexp.MustCompile(`\S*/\S+/\S+`)
	rePunct  = regexp.MustCompile(`[{}\[\]()<>;=|$` + "`" + `~^*/\\]`)
)

// CodeRunMin is how many consecutive code-shaped lines become a block.
//
// Two is where it stops being a coincidence. One numbered sentence in a
// paragraph, or one line that mentions a path, belongs to the prose around it.
const CodeRunMin = 2

func isCodeLike(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	switch {
	case reGutter.MatchString(line), reDiff.MatchString(line),
		reBare.MatchString(line), reShell.MatchString(line), rePath.MatchString(line):
		return true
	}
	if len(t) >= 8 {
		if float64(len(rePunct.FindAllString(t, -1)))/float64(len(t)) > 0.08 {
			return true
		}
	}
	return false
}

// Cleaner strips chrome and collapses blank runs. The zero value is not usable;
// build one with NewCleaner.
type Cleaner struct {
	patterns []*regexp.Regexp
}

// NewCleaner compiles a chrome pattern set.
//
// A pattern that does not compile is SKIPPED and reported, never fatal: these
// come from user config, and one bad regex in a list must not take the server
// down or silently disable the whole list.
func NewCleaner(patterns []string) (*Cleaner, []string) {
	c := &Cleaner{}
	var bad []string
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			bad = append(bad, p)
			continue
		}
		c.patterns = append(c.patterns, re)
	}
	return c, bad
}

// CleanResult is the cleaned text and an account of what was taken out, so the
// frame can SAY what it did rather than presenting the result as the screen.
type CleanResult struct {
	Text string
	// Chrome is how many lines matched a chrome pattern.
	Chrome int
	// Blank is how many blank lines were collapsed away.
	Blank int
	// Code is how many lines of code, diff or aligned output were folded into
	// markers. Unlike chrome this is real CONTENT, which is why it is replaced
	// by a visible marker rather than silently dropped.
	Code int
}

// Clean removes chrome lines, trims trailing whitespace, collapses runs of
// blank lines to one, and trims leading and trailing blanks.
//
// It never reorders, never rewrites a line's text, and never joins lines. A
// line that survives is byte-identical to what the pane showed, minus trailing
// whitespace the grid padded it with.
// Clean with DropCode folds code into markers as well as stripping chrome.
//
// This is LOSSY ON PURPOSE and only for a reader. A diff reflowed into a chat
// message is unreadable -- a path breaks mid-token, a line-number gutter walks
// out of alignment -- and twenty lines of it buries the sentence that said why
// it happened. So a run becomes "[12 lines of code]": the reader is told
// something was there, and can open the pane to see it.
//
// Nothing here guesses at MEANING. A run qualifies on shape alone -- gutters,
// diff markers, shell prompts, paths, punctuation density -- and never on what
// the code is about.
func (c *Cleaner) CleanDroppingCode(text string) CleanResult {
	return c.clean(text, true)
}

func (c *Cleaner) Clean(text string) CleanResult {
	return c.clean(text, false)
}

func (c *Cleaner) clean(text string, dropCode bool) CleanResult {
	var out []string
	res := CleanResult{}
	blankRun := 0
	codeRun := 0

	// Fold the pending code run into one marker, or release it as prose if it
	// never reached the length that makes it a block.
	flushCode := func(held []string) []string {
		if codeRun == 0 {
			return held
		}
		if codeRun >= CodeRunMin {
			res.Code += codeRun
			noun := "lines"
			if codeRun == 1 {
				noun = "line"
			}
			out = append(out, fmt.Sprintf("[%d %s of code]", codeRun, noun))
		} else {
			out = append(out, held...)
		}
		codeRun = 0
		return held[:0]
	}

	var held []string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t")

		if c.isChrome(line) {
			res.Chrome++
			continue
		}

		if dropCode {
			if isCodeLike(line) {
				if blankRun > 0 && codeRun > 0 {
					// A blank inside a diff is part of the diff.
					blankRun = 0
				}
				codeRun++
				held = append(held, line)
				continue
			}
			held = flushCode(held)
		}
		if line == "" {
			blankRun++
			continue
		}
		if blankRun > 0 {
			// One blank survives as a paragraph break; the rest are counted.
			if len(out) > 0 {
				out = append(out, "")
				res.Blank += blankRun - 1
			} else {
				res.Blank += blankRun
			}
			blankRun = 0
		}
		out = append(out, line)
	}
	if dropCode {
		held = flushCode(held)
	}
	_ = held
	// Trailing blanks are the void at the bottom of a grid, not content.
	res.Blank += blankRun
	res.Text = strings.Join(out, "\n")
	return res
}

func (c *Cleaner) isChrome(line string) bool {
	if line == "" {
		return false
	}
	for _, re := range c.patterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}
