/**
 * Agent state chip. Urgency order (SPEC §8): blocked > done(unseen) > working >
 * idle. The state comes from the SERVER; we never derive it.
 */

import type { AgentState } from '../protocol/types'

const LABEL: Record<AgentState, string> = {
  blocked: 'needs you',
  done: 'done',
  working: 'working',
  idle: 'idle',
  unknown: '—',
}

export function AgentBadge({ state }: { state: AgentState }) {
  return (
    <span className={`badge badge-${state}`} data-state={state}>
      <i className="badge-dot" aria-hidden="true" />
      {LABEL[state] ?? state}
    </span>
  )
}

export function ConnectionBadge({
  link,
  rttMs,
  attempt,
}: {
  link: string
  rttMs: number | null
  attempt: number
}) {
  const text =
    link === 'open'
      ? rttMs === null
        ? 'live'
        : `${Math.round(rttMs)}ms`
      : link === 'degraded'
        ? rttMs === null
          ? 'no response'
          : `slow · ${Math.round(rttMs)}ms`
        : link === 'reconnecting'
          ? `reconnecting${attempt > 1 ? ` ·${attempt}` : ''}`
          : link === 'connecting'
            ? 'connecting'
            : 'offline'
  return (
    <span className={`conn conn-${link}`} title={`link: ${link}`}>
      <i className="conn-dot" aria-hidden="true" />
      {text}
    </span>
  )
}
