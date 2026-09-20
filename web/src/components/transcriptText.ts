/**
 * Turning a captured SCREEN into something readable at 375px.
 *
 * Every transform here is WHITESPACE OR TYPOGRAPHY. Nothing in this file infers
 * meaning: it does not split messages, does not label roles, does not detect
 * turns, and does not drop content it dislikes. That restraint is the point —
 * Herdr exposes the pane's screen, not the agent's message history, so any
 * structure we "recovered" would be invented, and an invented structure that
 * renders as confident chat bubbles is worse than the raw grid it replaced.
 *
 * What it does, and what the UI says it does:
 *   - trims trailing whitespace from each line (the grid pads to its width);
 *   - collapses a run of blank lines to one (an agent TUI parks its prompt at
 *     the bottom of the grid, so a capture is mostly void);
 *   - recognises a line that is ONLY a repeated rule character and renders it
 *     as a rule, because 119 box-drawing characters wrap into six lines of
 *     noise on a phone and a horizontal rule is what it already was;
 *   - rejoins lines the AGENT'S OWN TUI hard-wrapped at the pane width, so the
 *     paragraph wraps ONCE, at the viewport, instead of twice. Without this a
 *     120-column paragraph read at 375px breaks mid-sentence on the pane's
 *     boundary and then again on ours, and every second line carries a stray
 *     hanging indent. The join is evidence-led and reported, never assumed:
 *     see `detectWrapColumn` and `isContinuation`.
 *
 * Wrapping itself is NOT done here. It is CSS (`white-space: pre-wrap`), so the
 * text reflows to the VIEWPORT as the viewport changes — including a rotation
 * or the soft keyboard opening — with no round trip and no PTY involved.
 */

export type TranscriptLine =
  | { kind: 'text'; text: string }
  | { kind: 'blank' }
  | { kind: 'rule' }

/** Characters that, alone and repeated, mean "the TUI drew a divider". */
const RULE_CHARS = new Set(['─', '━', '═', '-', '_', '=', '⎯', '‾', '▁', '·', '•'])
/** Short runs are content ("---" in prose); long ones are chrome. */
const RULE_MIN = 8

function isRule(line: string): boolean {
  const t = line.trim()
  if (t.length < RULE_MIN) return false
  const first = t[0]
  if (!RULE_CHARS.has(first)) return false
  for (const ch of t) if (ch !== first) return false
  return true
}

/**
 * Rejoining the pane's own wrap.
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
const REFLOW_MIN_FULL_LINES = 3
/**
 * How ragged a greedy wrapper's right edge gets: it breaks BEFORE the word that
 * would overflow, so a "full" line can stop a whole word short of the width.
 * Measured at 8 on a real 120-column Claude Code pane; 12 covers a long word.
 * This tolerance is for DETECTING the column only — the join test itself uses
 * the exact observed maximum.
 */
const REFLOW_RAGGED = 12
/** Bound on one joined paragraph, so a bad guess cannot eat the screen. */
const REFLOW_MAX_RUN = 60
/** Below this, a "width" is a column of short labels, not a wrapped paragraph. */
const REFLOW_MIN_WIDTH = 40

/** Lines that OPEN a block. Joining across one would destroy real structure. */
const BLOCK_START = /^(?:[-*+\u2022\u25aa\u25e6]\s|\d+[.)]\s|[>\u203a\u276f\u00bb\u2192#|])/

function indentOf(s: string): string {
  const m = /^[ \t]*/.exec(s)
  return m ? m[0] : ''
}

/** Diff markers at column 0, or box drawing anywhere: never prose. */
function looksLikeCode(s: string): boolean {
  return /^[+-]/.test(s) || /[\u2500-\u257f]/.test(s)
}

function detectWrapColumn(lines: string[]): number {
  let max = 0
  for (const l of lines) if (l.length > max) max = l.length
  if (max < REFLOW_MIN_WIDTH) return 0
  let full = 0
  for (const l of lines) if (l.length >= max - REFLOW_RAGGED) full++
  return full >= REFLOW_MIN_FULL_LINES ? max : 0
}

/**
 * True when `next` is the rest of `prev`, broken by the pane's width.
 *
 * `prev` is the last PHYSICAL line consumed, not the paragraph accumulated so
 * far — measuring the accumulation would make every line look full.
 */
function isContinuation(prev: string, next: string, wrapAt: number): boolean {
  if (prev === '' || next === '') return false
  if (isRule(prev) || isRule(next)) return false
  if (looksLikeCode(prev) || looksLikeCode(next)) return false
  const ind = indentOf(prev)
  if (indentOf(next) !== ind) return false
  const body = next.slice(ind.length)
  if (BLOCK_START.test(body)) return false
  const firstWord = /^\S+/.exec(body)?.[0] ?? ''
  if (firstWord === '') return false
  // The wrapper broke here only if the next word could not have fitted.
  return prev.length + 1 + firstWord.length > wrapAt
}

function reflow(lines: string[]): { lines: string[]; rejoined: number } {
  const wrapAt = detectWrapColumn(lines)
  if (wrapAt === 0) return { lines, rejoined: 0 }
  const out: string[] = []
  let rejoined = 0
  let run = 0
  let lastPhysical: string | null = null
  for (const line of lines) {
    if (
      lastPhysical !== null &&
      run < REFLOW_MAX_RUN &&
      isContinuation(lastPhysical, line, wrapAt)
    ) {
      out[out.length - 1] += ' ' + line.trimStart()
      rejoined++
      run++
    } else {
      out.push(line)
      run = 0
    }
    lastPhysical = line
  }
  return { lines: out, rejoined }
}

export interface TranscriptShape {
  lines: TranscriptLine[]
  /** How many blank lines were collapsed away. Reported, never hidden. */
  collapsed: number
  /** How many pane-wrapped lines were rejoined. Reported, never hidden. */
  rejoined: number
}

export function shapeTranscript(raw: string): TranscriptShape {
  const out: TranscriptLine[] = []
  let collapsed = 0
  let blankRun = 0
  // Right-trim first: the grid pads every row to its width, and a padded row
  // would read as "full" to the wrap detector no matter what it holds.
  const trimmed = raw.split('\n').map((l) => l.replace(/[ \t]+$/, ''))
  const { lines: physical, rejoined } = reflow(trimmed)
  for (const line of physical) {
    if (line === '') {
      blankRun++
      continue
    }
    if (blankRun > 0) {
      // One blank survives as a paragraph break; the rest are counted.
      if (out.length > 0) out.push({ kind: 'blank' })
      collapsed += blankRun - (out.length > 0 ? 1 : 0)
      blankRun = 0
    }
    out.push(isRule(line) ? { kind: 'rule' } : { kind: 'text', text: line })
  }
  // Trailing blanks are dropped entirely: they are the void, not content.
  collapsed += blankRun
  return { lines: out, collapsed, rejoined }
}
