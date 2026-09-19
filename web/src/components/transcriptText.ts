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
 *     noise on a phone and a horizontal rule is what it already was.
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

export interface TranscriptShape {
  lines: TranscriptLine[]
  /** How many blank lines were collapsed away. Reported, never hidden. */
  collapsed: number
}

export function shapeTranscript(raw: string): TranscriptShape {
  const out: TranscriptLine[] = []
  let collapsed = 0
  let blankRun = 0
  for (const rawLine of raw.split('\n')) {
    const line = rawLine.replace(/[ \t]+$/, '')
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
  return { lines: out, collapsed }
}
