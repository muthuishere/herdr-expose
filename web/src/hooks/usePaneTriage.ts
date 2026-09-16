/**
 * Triage, not reading.
 *
 * The pane list answers ONE question: which of my agents needs me right now?
 * Reading output is what the terminal view is for. A live-scrolling tail in a
 * list row fails both jobs — you cannot read a line that is replaced before you
 * finish it, and the motion drags the eye away from the row that matters.
 *
 * So the row content is STATE-DRIVEN, and it is quiet:
 *   blocked  -> the question. Actionable, naturally stable, given priority.
 *   working  -> NO output. A calm indicator plus elapsed time in state.
 *   else     -> the last line, which for an idle/done pane does not move.
 *
 * It is also a large render win: nothing here subscribes to the store's
 * high-frequency `runtime` map, so terminal output no longer re-renders the
 * list at the pane's output rate. Text for quiet panes is SAMPLED on a slow
 * timer and only committed when it actually differs.
 */

import { useEffect, useState } from 'react'
import { getState } from '../store/store'
import { useStore } from '../store/store'
import type { AgentState, PaneId, TreePane } from '../protocol/types'
import { ansiQuestionLine, ansiSummaryLine } from '../ansi/ansiToHtml'

/** Slow enough to read, and far below any sane output rate. */
export const QUIET_SAMPLE_MS = 4000

/* --- one shared 1s clock ------------------------------------------- */

const tickers = new Set<() => void>()
let tickTimer: number | undefined

function subscribeTick(fn: () => void): () => void {
  tickers.add(fn)
  if (tickTimer === undefined) {
    tickTimer = window.setInterval(() => {
      for (const t of tickers) t()
    }, 1000)
  }
  return () => {
    tickers.delete(fn)
    if (tickers.size === 0 && tickTimer !== undefined) {
      clearInterval(tickTimer)
      tickTimer = undefined
    }
  }
}

/**
 * Accepts unix ms or an RFC3339 string, and refuses to render a guess: an
 * unparseable timestamp yields no clock rather than NaN.
 */
export function toMillis(v: number | string | null | undefined): number | null {
  if (typeof v === 'number') return Number.isFinite(v) && v > 0 ? v : null
  if (typeof v === 'string') {
    const t = Date.parse(v)
    return Number.isFinite(t) ? t : null
  }
  return null
}

/** Ticks once a second, and only while something is actually showing a clock. */
export function useElapsed(since: number | null | undefined): string {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!since) return
    setNow(Date.now())
    return subscribeTick(() => setNow(Date.now()))
  }, [since])
  if (!since) return ''
  return formatElapsed(Math.max(0, now - since))
}

export function formatElapsed(ms: number): string {
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`
  const h = Math.floor(m / 60)
  return `${h}h ${String(m % 60).padStart(2, '0')}m`
}

/* --- sampled (never streamed) pane text ----------------------------- */

/**
 * Reads the pane's buffered text on a slow timer instead of subscribing to it.
 * Committing only on a real change means a static pane never re-renders at all.
 */
export function useQuietPaneText(target: PaneId, enabled: boolean): string {
  const [text, setText] = useState('')
  useEffect(() => {
    if (!enabled) {
      setText((prev) => (prev === '' ? prev : ''))
      return
    }
    const read = () => {
      const next = getState().runtime[target]?.text ?? ''
      setText((prev) => (prev === next ? prev : next))
    }
    read()
    const id = window.setInterval(read, QUIET_SAMPLE_MS)
    return () => clearInterval(id)
  }, [target, enabled])
  return text
}

/* --- the triage decision -------------------------------------------- */

export type TriageKind =
  | 'question' /* blocked: show the prompt, loudly */
  | 'activity' /* working: an indicator and a clock, no output */
  | 'line' /* idle/done: a static last line */
  | 'none'

export interface Triage {
  kind: TriageKind
  state: AgentState | undefined
  /** Ready to render for 'question' and 'line'. Empty otherwise. */
  text: string
  /** For 'activity': when the pane entered this state. */
  since: number | null
}

export function usePaneTriage(pane: TreePane): Triage {
  const state = pane.agent?.state
  const blocked = state === 'blocked'
  const working = state === 'working'
  // Detection changes only on an `agent` frame — it is not a hot path.
  const detection = useStore((s) => s.detection[pane.id])
  const quiet = useQuietPaneText(pane.id, !blocked && !working)

  if (blocked) {
    const text = (detection || pane.agent?.summary || '').trim()
    return {
      kind: 'question',
      state,
      text: (text ? ansiQuestionLine(text) : '') || 'Waiting for your answer.',
      since: null,
    }
  }
  if (working) {
    return { kind: 'activity', state, text: '', since: toMillis(pane.agent?.changed_at) }
  }
  const line = quiet ? ansiSummaryLine(quiet) : ''
  const text = line || pane.agent?.summary || pane.cwd || ''
  return { kind: text ? 'line' : 'none', state, text, since: null }
}
