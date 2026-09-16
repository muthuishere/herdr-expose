/**
 * SUMMARY rendering — pre-rendered ANSI-as-HTML. SPEC §8: never an xterm per
 * tile. This is the whole reason the grid stays responsive with twenty panes.
 */

import { useMemo } from 'react'
import { ansiToHtml } from '../ansi/ansiToHtml'

export function AnsiBlock({
  text,
  maxLines = 12,
  maxCols = 120,
  className = '',
}: {
  text: string
  maxLines?: number
  maxCols?: number
  className?: string
}) {
  const html = useMemo(
    () => ansiToHtml(text, { maxLines, maxCols }),
    [text, maxLines, maxCols],
  )
  return (
    <pre
      className={`ansi ${className}`}
      // ansiToHtml escapes everything it emits; only its own spans survive.
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
}
