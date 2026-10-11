/**
 * The agent's conversation, rendered from its OWN transcript rather than from
 * the screen it painted.
 *
 * WHY COLLAPSE IS THE WHOLE POINT. Measured on a real 24MB transcript from
 * this machine: 826 tool_use/tool_result pairs against 487 prose blocks, so
 * 63% of everything the agent emitted is tool traffic. Shown in full on a
 * phone that is not a conversation, it is a log file with some sentences in
 * it. Collapsed to one row each — `▸ Bash  go test ./...` — the prose becomes
 * the document again and the tool calls become what they actually are: a
 * footnote you can open.
 *
 * Expanded by DEFAULT for failures only. A reader scrolling a transcript wants
 * to know where it went wrong without tapping twenty rows to find out, and the
 * agent's own `is_error` is a fact rather than our guess at one.
 *
 * Results arrive capped at 4KB from the server (the largest measured was
 * 431,524 bytes), so expanding a row cannot drop a megabyte into the layout.
 */

import { useState } from 'react'
import type { AgentEvent } from '../net/agentlog'
import { Markdown } from './markdown'

/** A collapsed tool row, which is most of the transcript by count. */
function ToolEvent({ ev }: { ev: AgentEvent }) {
  // Failures start open; everything else starts shut. See the file comment.
  const [open, setOpen] = useState(Boolean(ev.failed))
  const hasBody = Boolean(ev.result)
  return (
    <div className={`ag-tool${ev.failed ? ' is-failed' : ''}`}>
      <button
        type="button"
        className="ag-tool-row"
        aria-expanded={open}
        // The row is the control, so the whole thing is the tap target: a
        // chevron alone is a 16px target on a surface built for thumbs.
        onClick={() => setOpen((v) => !v)}
        disabled={!hasBody}
      >
        <span className="ag-caret" aria-hidden="true">
          {hasBody ? (open ? '▾' : '▸') : '·'}
        </span>
        <span className="ag-tool-name">{ev.tool || 'tool'}</span>
        {/* The summary is allowed to be clipped: it is a label, and the full
            value is in the body one tap away. Clipping keeps one call to one
            row, which is what makes a list of 800 of them scannable. */}
        <span className="ag-tool-sum">{ev.summary}</span>
        {ev.failed ? <span className="ag-badge">failed</span> : null}
      </button>
      {open && hasBody ? (
        <div className="ag-scroll">
          <pre className="ag-result">
            <code>{ev.result}</code>
          </pre>
          {ev.result_truncated ? (
            // Said plainly, because a reader who cannot tell a short result
            // from a cut one will debug the wrong thing.
            <p className="ag-cut">output truncated — the full result stayed on the machine</p>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

export function AgentTranscript({ events }: { events: AgentEvent[] }) {
  return (
    <div className="ag">
      {events.map((ev, i) => {
        if (ev.role === 'tool') return <ToolEvent ev={ev} key={i} />
        if (!ev.text) return null
        // The person's own turns are marked, because a transcript where you
        // cannot tell the question from the answer is just text.
        return (
          <div className={`ag-msg is-${ev.role}`} key={i}>
            {ev.role === 'user' ? <span className="ag-who">you</span> : null}
            <Markdown source={ev.text} />
          </div>
        )
      })}
    </div>
  )
}
