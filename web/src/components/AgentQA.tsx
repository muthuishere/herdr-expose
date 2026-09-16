/**
 * Blocked-agent Q&A — SPEC §8 / architecture.md.
 *
 * When an agent is `blocked`, the server sends the `agent.read --source
 * detection` text. We render it VERBATIM as ANSI-to-HTML and offer a fixed set
 * of answer keys. Deliberately GENERIC across agent kinds: no per-agent parsing,
 * ever — that is the maintenance trap this design exists to avoid.
 */

import type { PaneId } from '../protocol/types'
import { useStore } from '../store/store'
import { AnsiBlock } from './SummaryTile'

export const QA_QUICK_KEYS = [
  { label: 'y', seq: 'y', tone: 'yes' as const },
  { label: 'n', seq: 'n', tone: 'no' as const },
  { label: '1', seq: '1' },
  { label: '2', seq: '2' },
  { label: '3', seq: '3' },
]

export function AgentQA({ target }: { target: PaneId }) {
  const detection = useStore((s) => s.detection[target])
  const pane = useStore((s) => s.panesById[target])
  if (pane?.agent?.state !== 'blocked') return null

  return (
    <aside className="qa" role="region" aria-label="Agent is waiting for you">
      <header className="qa-head">
        <span className="qa-dot" aria-hidden="true" />
        <span className="qa-title">
          {pane.agent.kind ? `${pane.agent.kind} ` : ''}needs an answer
        </span>
      </header>
      {detection ? (
        <AnsiBlock text={detection} maxLines={14} maxCols={200} className="qa-body" />
      ) : (
        <p className="qa-body qa-empty">{pane.agent.summary ?? 'Waiting for your input.'}</p>
      )}
    </aside>
  )
}
