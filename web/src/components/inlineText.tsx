/**
 * The small amount of inline markup that actually survives a terminal.
 *
 * An agent TUI renders most markdown to ANSI before it paints — bold becomes
 * SGR 1, not `**bold**` — and we strip ANSI, so the markers are usually gone
 * before we ever see them. Measured across three live Claude Code panes: one
 * `**bold**`, zero headings, zero fences, zero inline backticks.
 *
 * So this is deliberately TINY. It handles the two markers that do sometimes
 * reach us intact, and it does nothing else: no links, no italics, no
 * headings, no tables. Everything it does not recognise stays exactly as it
 * was typed, visible, including the markers themselves.
 *
 * It builds React nodes rather than HTML. There is no `dangerouslySetInnerHTML`
 * anywhere in this app and there must not be: this text is an untrusted agent's
 * output being shown to whoever holds the share link.
 */

import type { ReactNode } from 'react'

/** `**bold**` or `` `code` ``. Non-greedy, and never spanning a line. */
const INLINE = /(\*\*(?=\S)([^*\n]+?)(?<=\S)\*\*|`(?=\S)([^`\n]+?)(?<=\S)`)/g

export function renderInline(text: string): ReactNode {
  // Fast path: most lines have neither marker, and this runs per line per frame.
  if (text.indexOf('**') === -1 && text.indexOf('`') === -1) return text

  const out: ReactNode[] = []
  let last = 0
  let m: RegExpExecArray | null
  INLINE.lastIndex = 0
  let key = 0
  while ((m = INLINE.exec(text)) !== null) {
    if (m.index > last) out.push(text.slice(last, m.index))
    if (m[2] !== undefined) out.push(<strong key={key++}>{m[2]}</strong>)
    else out.push(<code key={key++}>{m[3]}</code>)
    last = m.index + m[0].length
  }
  if (last === 0) return text
  if (last < text.length) out.push(text.slice(last))
  return out
}
