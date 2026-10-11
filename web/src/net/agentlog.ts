/**
 * The agent's own transcript — `GET /v1/agentlog` (ADR 0040).
 *
 * Fetched on demand rather than pushed on the stream. The stream is the fanout
 * path and its budget is for live screen bytes; pushing tens of KB of markdown
 * and tool output to every subscriber on every poll would spend that budget on
 * history nobody is reading. This is asked for when somebody opens a pane,
 * which is when they want it.
 */

import { getToken } from './auth'

export type AgentEvent = {
  role: 'user' | 'assistant' | 'tool'
  /** Markdown SOURCE for user and assistant. Rendering is this side's job. */
  text?: string
  /** Tool name, for role === 'tool'. */
  tool?: string
  /** The one line a collapsed tool row shows: the command, the path. */
  summary?: string
  /** What the tool returned, already capped server-side at 4KB. */
  result?: string
  result_truncated?: boolean
  failed?: boolean
  at?: string
}

export type AgentLog = {
  /** False means "no transcript here" — fall back to the pane, do not error. */
  available: boolean
  events: AgentEvent[]
}

const EMPTY: AgentLog = { available: false, events: [] }

/**
 * `session` is the agent's session id, which the UI already has as
 * `pane.agent.id`. The server resolves it back to the pane that owns it and
 * applies this instance's scope, so an id alone grants nothing.
 */
export async function fetchAgentLog(session: string, limit = 60): Promise<AgentLog> {
  if (!session) return EMPTY
  const token = getToken()
  try {
    const res = await fetch(`/v1/agentlog?session=${encodeURIComponent(session)}&limit=${limit}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
      credentials: 'omit',
    })
    if (!res.ok) return EMPTY
    const body = (await res.json()) as AgentLog
    return {
      available: Boolean(body?.available),
      events: Array.isArray(body?.events) ? body.events : [],
    }
  } catch {
    // A failed fetch is NOT an error state worth showing. The pane transcript
    // is still there and still correct; this view is the nicer rendering of
    // the same conversation, so its absence degrades to the pane rather than
    // to a message about a URL the reader cannot do anything about.
    return EMPTY
  }
}
