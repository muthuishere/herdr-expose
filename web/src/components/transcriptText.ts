/**
 * Turning a captured SCREEN into something readable at 375px.
 *
 * Every transform here is WHITESPACE, TYPOGRAPHY OR LAYOUT. Nothing in this
 * file infers MEANING: it does not split messages, does not label roles, does
 * not detect turns, and does not drop content it dislikes. That restraint is
 * the point — Herdr exposes the pane's screen, not the agent's message
 * history, so any structure we "recovered" would be invented, and an invented
 * structure that renders as confident chat bubbles is worse than the raw grid
 * it replaced.
 *
 * What it does, and what the UI says it does:
 *   - trims trailing whitespace from each line (the grid pads to its width);
 *   - collapses a run of blank lines to one (an agent TUI parks its prompt at
 *     the bottom of the grid, so a capture is mostly void);
 *   - recognises a line that is ONLY a repeated rule character and renders it
 *     as a rule, because 119 box-drawing characters wrap into six lines of
 *     noise on a phone and a horizontal rule is what it already was;
 *   - separates CODE from PROSE and lets each have the layout it needs (see
 *     "two kinds of line" below);
 *   - rejoins lines the AGENT'S OWN TUI hard-wrapped at the pane width, so a
 *     paragraph wraps ONCE, at the viewport, instead of twice.
 *
 * TWO KINDS OF LINE, AND WHY IT MATTERS.
 *
 * A terminal gives us one stream with two kinds of content in it, and they
 * want opposite things. Prose wants to reflow to the reader's screen. Code,
 * diffs and anything with a line-number gutter want to KEEP THEIR COLUMNS —
 * wrap them and `p.goto('file:///Users/...')` breaks mid-path, the gutter
 * marches out of line, and a diff stops being readable as a diff.
 *
 * Wrapping everything is what the first version did, and a git diff on a phone
 * came out as a ragged column of fragments. So code-like runs are rendered
 * unwrapped in their own horizontally scrollable block, and prose reflows
 * around them. Classification is by SHAPE — gutters, diff markers, box
 * drawing, punctuation density — never by guessing what the text is about,
 * and a run has to be at least two lines before it counts, so one numbered
 * sentence in a paragraph stays in the paragraph.
 *
 * Prose wrapping itself is still CSS, so it reflows to the viewport as the
 * viewport changes — a rotation, the soft keyboard opening — with no round
 * trip and no PTY involved.
 */

export type TranscriptItem =
  | { kind: "text"; text: string }
  | { kind: "blank" }
  | { kind: "rule" }
  /** A run of lines that must not be wrapped. Rendered scrollable. */
  | { kind: "code"; lines: string[] }
  /** Consecutive bullet lines, rendered as a real list. */
  | { kind: "list"; items: string[] };

/** Characters that, alone and repeated, mean "the TUI drew a divider". */
const RULE_CHARS = new Set([
  "─",
  "━",
  "═",
  "-",
  "_",
  "=",
  "⎯",
  "‾",
  "▁",
  "·",
  "•",
]);
/** Short runs are content ("---" in prose); long ones are chrome. */
const RULE_MIN = 8;

function isRule(line: string): boolean {
  const t = line.trim();
  if (t.length < RULE_MIN) return false;
  const first = t[0];
  if (!RULE_CHARS.has(first)) return false;
  for (const ch of t) if (ch !== first) return false;
  return true;
}

/* --- telling code from prose ---------------------------------------------
 *
 * Every test below is about the SHAPE of a line. None of them reads the words.
 */

/** Box drawing and block elements: a TUI frame, never a sentence. */
const BOX_DRAWING = /[─-╿▀-▟]/;

/**
 * A line-number gutter, with or without a diff marker after it.
 *
 * Note the two minus signs. A terminal diff from Claude Code uses U+2212 MINUS
 * SIGN, not ASCII hyphen, so matching only `-` misses every removed line —
 * which is exactly how a diff ends up classified as prose and wrapped.
 */
const GUTTER = /^\s*\d+\s+[-+−±]?\s?/;
/** A bare diff marker at the start of the line. */
const DIFF_MARKER = /^\s*[-+−]\s?\S/;
/**
 * A gutter line with nothing after it — a blank line INSIDE a diff still has
 * its number. Without this the bare number reads as prose and splits one code
 * block into two, with a stray "757" paragraph between them.
 */
const BARE_GUTTER = /^\s*\d+$/;
/**
 * Aligned COLUMNS: two or more runs of 2+ spaces between non-spaces. A version
 * table padded into columns is prose by punctuation but reads as nonsense once
 * reflowed, so alignment alone is enough to keep a run unwrapped.
 */
const COLUMNS = /\S {2,}\S[\s\S]*?\S {2,}\S/;
/** A shell prompt, and a path with at least two separators. */
const SHELL_PROMPT = /^\s*[$#>]\s\S/;
const PATHISH = /\S*\/\S+\/\S+/;
/** Punctuation that is common in code and rare in a sentence. */
const CODE_PUNCT = /[{}[\]()<>;=|$`~^*/\\]/g;

function punctDensity(s: string): number {
  const t = s.trim();
  if (t.length < 8) return 0;
  const n = (t.match(CODE_PUNCT) || []).length;
  return n / t.length;
}

/** Does this line want to keep its columns? */
function isCodeLike(line: string): boolean {
  if (line.trim() === "") return false;
  if (isRule(line)) return false;
  if (BOX_DRAWING.test(line)) return true;
  if (GUTTER.test(line)) return true;
  if (DIFF_MARKER.test(line)) return true;
  if (BARE_GUTTER.test(line)) return true;
  if (SHELL_PROMPT.test(line)) return true;
  if (PATHISH.test(line)) return true;
  if (COLUMNS.test(line)) return true;
  // A line dense in brackets and operators is code even with no gutter.
  if (punctDensity(line) > 0.08) return true;
  return false;
}

/**
 * A code RUN must be at least this many lines.
 *
 * One line is never enough. "3. Run the migration" has a gutter shape, and a
 * single sentence containing a path has the punctuation density; both belong
 * to the paragraph around them. Two consecutive code-shaped lines is the point
 * where it stops being a coincidence.
 */
const CODE_RUN_MIN = 2;

/* --- rejoining the pane's own wrap ---------------------------------------
 *
 * A TUI wraps greedily at its width, which leaves a signature we can read back:
 * a cluster of lines that all stop within a character or two of the same
 * column. `detectWrapColumn` looks for that cluster and refuses to reflow at all
 * unless it finds one, so text that was never wrapped is left exactly as it is.
 *
 * Both failure directions are deliberately safe. Over-estimate the column and
 * the "did the next word fit?" test gets HARDER, so we join less. Under-estimate
 * it badly and the evidence guard (REFLOW_MIN_FULL_LINES) has already refused.
 *
 * Widths here are code-unit counts, not display cells, so a CJK- or emoji-heavy
 * pane measures narrow. That too fails toward joining less.
 */

/** How many lines must stop at the same column before we believe it is a wrap. */
const REFLOW_MIN_FULL_LINES = 3;
/**
 * How ragged a greedy wrapper's right edge gets: it breaks BEFORE the word that
 * would overflow, so a "full" line can stop a whole word short of the width.
 * Measured at 8 on a real 120-column Claude Code pane; 12 covers a long word.
 * This tolerance is for DETECTING the column only — the join test itself uses
 * the exact observed maximum.
 */
const REFLOW_RAGGED = 12;
/** Bound on one joined paragraph, so a bad guess cannot eat the screen. */
const REFLOW_MAX_RUN = 60;
/** Below this, a "width" is a column of short labels, not a wrapped paragraph. */
const REFLOW_MIN_WIDTH = 40;

/** Lines that OPEN a block. Joining across one would destroy real structure. */
const BLOCK_START = /^(?:[-*+•▪◦]\s|\d+[.)]\s|[>›❯»→#|])/;

/**
 * A bullet the TUI drew. Claude Code emits "- ", "• " and "* " at various
 * indents; the marker is stripped and the text becomes a real list item, so a
 * phone gets a hanging indent instead of a second line that starts under the
 * dash.
 */
const BULLET = /^(\s*)[-*\u2022\u25aa\u25e6\u2013\u2014]\s+(\S.*)$/;

function indentOf(s: string): string {
  const m = /^[ \t]*/.exec(s);
  return m ? m[0] : "";
}

/**
 * Measured over PROSE ONLY.
 *
 * A code block is not wrapped by the TUI — a 200-character line in a diff stays
 * 200 characters — so including those lines pushes the measured maximum well
 * past the real prose width. The join test then asks whether the next word
 * would have fitted in 200 columns, the answer is always yes, and NOTHING gets
 * rejoined. That is exactly how a paragraph ending "Next: your go-ahead" kept
 * its continuation as a separate one-line paragraph.
 */
function detectWrapColumn(all: string[]): number {
  const lines = all.filter((l) => l !== "" && !isCodeLike(l));
  let max = 0;
  for (const l of lines) if (l.length > max) max = l.length;
  if (max < REFLOW_MIN_WIDTH) return 0;
  let full = 0;
  for (const l of lines) if (l.length >= max - REFLOW_RAGGED) full++;
  return full >= REFLOW_MIN_FULL_LINES ? max : 0;
}

/**
 * The exact column the pane hard-cuts at, or 0 when there is no such column.
 *
 * Measured over EVERY line, code included, because the whole point is the cut
 * that lands inside something the prose measurement deliberately ignores. The
 * evidence demanded is exactness: at least HARD_MIN_LINES lines stopping at the
 * very same column, not merely near it. Ragged right edges are ordinary text;
 * a column several lines stop at precisely is a terminal width.
 */
const HARD_MIN_LINES = 2;

function detectHardColumn(all: string[]): number {
  let max = 0;
  for (const l of all) if (!isRule(l) && l.length > max) max = l.length;
  if (max < REFLOW_MIN_WIDTH) return 0;
  let exact = 0;
  for (const l of all) if (!isRule(l) && l.length === max) exact++;
  return exact >= HARD_MIN_LINES ? max : 0;
}

/**
 * True when `next` is the rest of `prev`, broken by the pane's width.
 *
 * `prev` is the last PHYSICAL line consumed, not the paragraph accumulated so
 * far — measuring the accumulation would make every line look full.
 */
function isContinuation(
  prev: string,
  next: string,
  wrapAt: number,
  prevLen = prev.length,
): boolean {
  if (prev === "" || next === "") return false;
  if (isRule(prev) || isRule(next)) return false;
  if (isCodeLike(prev) || isCodeLike(next)) return false;
  /*
   * Indent. A TUI wraps a paragraph with a HANGING INDENT, so the second line
   * is often indented a little further than the first -- "...Next: your
   * go-ahead" followed by "  on adding Mermaid export to ADR 36". Requiring
   * exactly equal indents refused those joins and left a one-word orphan
   * paragraph on screen.
   *
   * So a continuation may be indented the same, or up to a small hanging
   * indent further. It may never be indented LESS: that is the paragraph
   * ending and something at an outer level starting.
   */
  const ind = indentOf(prev);
  const delta = indentOf(next).length - ind.length;
  if (delta < 0 || delta > 4) return false;
  // Slice NEXT's own indent, not PREV's. With a hanging indent next is
  // indented further, so slicing prev's shorter indent left leading spaces,
  // `^\S+` found no word, and every hanging-indent continuation was refused.
  const body = next.slice(indentOf(next).length);
  if (BLOCK_START.test(body)) return false;
  const firstWord = /^\S+/.exec(body)?.[0] ?? "";
  if (firstWord === "") return false;
  // The wrapper broke here only if the next word could not have fitted.
  return prevLen + 1 + firstWord.length > wrapAt;
}

/**
 * Undo the pane's HARD wrap, before anything is classified.
 *
 * A terminal cuts at its width and the cut lands wherever it lands, including
 * the middle of a word: "...a seller-ops acknowledged obj" / "ection from
 * marketplace-08)". That is not a line break the agent wrote, it is an artefact
 * of the pane being 165 columns wide, and every later decision -- is this code,
 * is this a list, does this paragraph continue -- is wrong if it runs on the
 * halves instead of the whole.
 *
 * It MUST happen here rather than in the join step below. A run of code-like
 * lines becomes a code block, and a code block ends the current paragraph, so
 * by the time the continuation is reached there is nothing left to continue:
 * it was emitted as its own paragraph beginning in the middle of a word. On
 * screen that does not read as a layout choice, it reads as lost data.
 *
 * Only an exact-width stop is treated this way, and only when the width was
 * confirmed by several lines stopping at precisely the same column. A line that
 * plainly opens something new still interrupts, because a wrap that happens to
 * land before a bullet must not swallow the bullet.
 */
function unwrapHardCuts(
  lines: string[],
  hardAt: number,
): { lines: string[]; tail: number[] } {
  if (hardAt <= 0) return { lines, tail: lines.map((l) => l.length) };
  const out: string[] = [];
  // tail[i] is the length of the LAST physical row that went into out[i].
  //
  // The soft-wrap test below asks "was this line full enough that the next word
  // could not have fitted?", and the answer is about the row the pane actually
  // drew, not about the repaired line, which is longer than any row by
  // construction. Without this a repaired line looked permanently full and
  // swallowed the paragraph that followed it.
  const tail: number[] = [];
  for (const line of lines) {
    const prev = out[out.length - 1];
    if (
      prev !== undefined &&
      tail[tail.length - 1] >= hardAt &&
      !isRule(prev) &&
      line !== "" &&
      !isRule(line) &&
      !BLOCK_START.test(line.slice(indentOf(line).length))
    ) {
      out[out.length - 1] = prev + seam(prev, line) + line;
      tail[tail.length - 1] = line.length;
      continue;
    }
    out.push(line);
    tail.push(line.length);
  }
  return { lines: out, tail };
}

/**
 * How to put the two halves back together.
 *
 * A cut at the pane width can land inside a word -- "obj" / "ection" -- and
 * inserting a space there would read as "obj ection", which is the same damage
 * in a different shape. So: a space only when a side already had whitespace at
 * the seam.
 */
function seam(prev: string, next: string): string {
  return /\s$/.test(prev) || /^\s/.test(next) ? " " : "";
}

export interface TranscriptShape {
  items: TranscriptItem[];
  /** How many blank lines were collapsed away. Reported, never hidden. */
  collapsed: number;
  /** How many pane-wrapped lines were rejoined. Reported, never hidden. */
  rejoined: number;
  /** How many runs were kept unwrapped as code. Reported, never hidden. */
  codeBlocks: number;
}

export function shapeTranscript(raw: string): TranscriptShape {
  // Right-trim first: the grid pads every row to its width, and a padded row
  // would read as "full" to the wrap detector no matter what it holds.
  const rawLines = raw.split("\n").map((l) => l.replace(/[ \t]+$/, ""));

  // The wrap column is measured over the WHOLE capture, before anything is
  // split up: the more lines it sees, the better the evidence.
  // BOTH columns are measured on the rows as the pane emitted them. Measuring
  // the soft column after the repair destroys the evidence it is read from --
  // the cluster of lines stopping near one column is exactly what the repair
  // joins away -- and the detector then found nothing and reflowed nothing.
  const wrapAt = detectWrapColumn(rawLines);
  const { lines, tail } = unwrapHardCuts(rawLines, detectHardColumn(rawLines));

  const items: TranscriptItem[] = [];
  let collapsed = 0;
  let rejoined = 0;
  let codeBlocks = 0;
  let blankRun = 0;
  // The last physical prose line consumed, for the continuation test.
  let lastProse: string | null = null;
  // The length of the last PHYSICAL row behind lastProse; see unwrapHardCuts.
  let lastProseLen = 0;
  // How many joins the current paragraph has taken, so a bad wrap-column guess
  // cannot swallow the whole screen into one line.
  let joinRun = 0;

  /** Emit the pending blank run as at most one separator. */
  function flushBlanks() {
    if (blankRun === 0) return;
    if (items.length > 0) {
      items.push({ kind: "blank" });
      collapsed += blankRun - 1;
    } else {
      collapsed += blankRun;
    }
    blankRun = 0;
    lastProse = null;
    joinRun = 0;
  }

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];

    if (line === "") {
      blankRun++;
      continue;
    }

    // A code RUN: scan ahead while lines keep their code shape. A single blank
    // inside a run is kept, because a diff with a gap is still one diff.
    if (isCodeLike(line)) {
      let j = i;
      const run: string[] = [];
      while (j < lines.length) {
        const l = lines[j];
        if (isCodeLike(l)) {
          run.push(l);
          j++;
          continue;
        }
        // Look past a single blank: if code resumes, the blank is part of it.
        if (l === "" && j + 1 < lines.length && isCodeLike(lines[j + 1])) {
          run.push("");
          j++;
          continue;
        }
        break;
      }
      if (run.length >= CODE_RUN_MIN) {
        flushBlanks();
        items.push({ kind: "code", lines: run });
        codeBlocks++;
        lastProse = null;
        joinRun = 0;
        i = j - 1;
        continue;
      }
      // Too short to be a block: fall through and treat it as ordinary text.
    }

    flushBlanks();

    if (isRule(line)) {
      items.push({ kind: "rule" });
      lastProse = null;
      joinRun = 0;
      continue;
    }

    // A bullet. Consecutive bullets become ONE list, and a bullet's own
    // wrapped continuation lines fold back into it.
    const bullet = BULLET.exec(line);
    if (bullet) {
      const prevItem = items[items.length - 1];
      if (prevItem && prevItem.kind === "list") prevItem.items.push(bullet[2]);
      else items.push({ kind: "list", items: [bullet[2]] });
      lastProse = line;
      lastProseLen = tail[i];
      joinRun = 0;
      continue;
    }
    {
      // Continuation of the bullet above: indented, and we are inside a list.
      const prevItem = items[items.length - 1];
      if (
        wrapAt > 0 &&
        prevItem &&
        prevItem.kind === "list" &&
        lastProse !== null &&
        joinRun < REFLOW_MAX_RUN &&
        indentOf(line).length > 0 &&
        isContinuation(lastProse, line, wrapAt, lastProseLen)
      ) {
        prevItem.items[prevItem.items.length - 1] += " " + line.trimStart();
        rejoined++;
        joinRun++;
        lastProse = line;
        lastProseLen = tail[i];
        continue;
      }
    }

    // Prose. Join it to the previous line if the pane wrapped it there.
    const prev = items[items.length - 1];
    if (
      wrapAt > 0 &&
      prev &&
      prev.kind === "text" &&
      lastProse !== null &&
      joinRun < REFLOW_MAX_RUN &&
      isContinuation(lastProse, line, wrapAt, lastProseLen)
    ) {
      prev.text += " " + line.trimStart();
      rejoined++;
      joinRun++;
    } else {
      items.push({ kind: "text", text: line });
      joinRun = 0;
    }
    lastProse = line;
    lastProseLen = tail[i];
  }

  // Trailing blanks are dropped entirely: they are the void, not content.
  collapsed += blankRun;
  return { items, collapsed, rejoined, codeBlocks };
}
