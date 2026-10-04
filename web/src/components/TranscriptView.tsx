/**
 * TRANSCRIPT VIEW — SPEC AMENDMENTS 13. The default for a pane with an agent.
 *
 * Why this exists instead of a better terminal mirror: an agent TUI paints
 * incrementally near the bottom of its grid and repaints on SIGWINCH. Attaching
 * a browser resized the PTY, so the agent threw its screen away and redrew, and
 * you were left reading a fragment in a void — having destroyed, by opening it,
 * the history you opened it to read. Mirroring the grid harder cannot fix that.
 *
 * So this view declares `transcript` and NOTHING ELSE. In particular it never
 * calls sendResize(), because the pane's geometry is exactly what must not
 * change. Input goes through the geometry-free `command` path (agent.prompt /
 * agent.send_keys), never through the raw binary input path, which would spawn
 * a control stream and resize the pane on the first tap.
 *
 * HONESTY (J4). Herdr exposes the pane's SCREEN. There is no message history to
 * fetch, so this is not a reconstructed conversation and must never be dressed
 * up as one — no chat bubbles, no inferred roles, no invented turn boundaries.
 * The agent's text is shown as text, our chrome is visibly ours, and the footer
 * states plainly which buffer this came from and what we changed.
 */

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { PaneId } from '../protocol/types'
import { useStore } from '../store/store'
import { onReconnected, sendCommand, sendSubscribe, sendUnsubscribe } from '../net/connection'
import { declareViewport, releaseViewport, resendViewport } from '../net/viewport'
import { shapeTranscript } from './transcriptText'
import { hasRealAgent } from './agentPresence'
import { renderInline } from './inlineText'
import { imagesFrom, uploadImage, type Upload } from '../net/upload'

/**
 * The answer keys from SPEC J2, sent as NAMED KEYS through `agent.send_keys`.
 *
 * Not escape sequences on the input path: that path attaches a controlling
 * terminal at this connection's geometry, and a transcript connection
 * deliberately has none — so one tap would resize a real agent to the 20x6
 * floor. Named keys go over the control plane and touch no geometry at all.
 */
const ANSWER_KEYS: Array<{ label: string; key: string; tone?: 'yes' | 'no' }> = [
  { label: 'y', key: 'y', tone: 'yes' },
  { label: 'n', key: 'n', tone: 'no' },
  { label: '1', key: '1' },
  { label: '2', key: '2' },
  { label: '3', key: '3' },
  { label: 'esc', key: 'esc' },
  { label: '↑', key: 'up' },
  { label: '↓', key: 'down' },
  { label: '←', key: 'left' },
  { label: '→', key: 'right' },
]

const SOURCE_LABEL: Record<string, string> = {
  recent_unwrapped: 'recent output',
  recent: 'recent output',
  visible: 'visible screen',
  detection: 'the prompt region',
}

export function TranscriptView({ target }: { target: PaneId }) {
  const frame = useStore((s) => s.transcript[target])
  const pane = useStore((s) => s.panesById[target])
  const blocked = pane?.agent?.state === 'blocked'
  // Only offer a prompt box where there is genuinely an agent to prompt:
  // every plain shell pane carries an agent object with state `unknown`.
  const hasAgent = hasRealAgent(pane)

  /* --- subscribe. NO sendResize, ever. ------------------------------- */
  useEffect(() => {
    const start = () => {
      sendSubscribe([target])
      // transcript_CLEAN, not raw transcript.
      //
      // The raw screen ends with the agent's own furniture: an empty input
      // caret, the context meter, the mode banner, and a progress line that
      // redraws every second. None of it is anything the agent said, and on a
      // phone it is the last thing on screen, so it is what you land on.
      //
      // The server already knows how to remove exactly those lines, and the
      // frame reports what it took out, so this is not the client guessing --
      // it is asking for a shape the protocol defines and then saying so.
      declareViewport('transcript', { [target]: 'transcript_clean' })
    }
    start()
    const off = onReconnected(() => {
      resendViewport()
      start()
    })
    return () => {
      off()
      releaseViewport('transcript')
      sendUnsubscribe([target])
    }
  }, [target])

  const shaped = useMemo(() => shapeTranscript(frame?.text ?? ''), [frame?.text])

  return (
    <div className="tr">
      <TranscriptBody
        target={target}
        items={shaped.items}
        empty={!frame}
        blocked={blocked}
        source={frame?.source}
        truncated={!!frame?.truncated}
        collapsed={shaped.collapsed}
        codeBlocks={shaped.codeBlocks}
        cleaned={frame?.cleaned}
        rejoined={shaped.rejoined}
      />
      {blocked ? <AnswerKeys target={target} /> : null}
      {hasAgent ? <PromptBox target={target} /> : null}
    </div>
  )
}

/**
 * The scrolling text. Pinned to the bottom unless the reader has scrolled up —
 * a transcript that yanks you back to the newest line while you are reading
 * something older is worse than one that does not follow at all.
 */
function TranscriptBody({
  target,
  items,
  empty,
  blocked,
  source,
  truncated,
  collapsed,
  rejoined,
  codeBlocks,
  cleaned,
}: {
  target: PaneId
  items: ReturnType<typeof shapeTranscript>['items']
  empty: boolean
  blocked: boolean
  source?: string
  truncated: boolean
  collapsed: number
  rejoined: number
  codeBlocks: number
  cleaned?: { chrome: number; blank: number; code?: number }
}) {
  const ref = useRef<HTMLDivElement>(null)
  const pinnedRef = useRef(true)

  useLayoutEffect(() => {
    const el = ref.current
    if (!el || !pinnedRef.current) return
    el.scrollTop = el.scrollHeight
  }, [items])

  return (
    <>
      <div
        className="tr-body"
        ref={ref}
        role="log"
        aria-label="What this pane is showing"
        aria-live="polite"
        onScroll={(e) => {
          const el = e.currentTarget
          pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
        }}
      >
        {blocked ? (
          <p className="tr-blocked">
            This agent is waiting for an answer. What follows is {SOURCE_LABEL.detection}.
          </p>
        ) : null}
        {empty ? (
          <p className="tr-empty">Reading this pane&apos;s screen…</p>
        ) : items.length === 0 ? (
          <p className="tr-empty">This pane&apos;s screen is blank.</p>
        ) : (
          <div className="tr-flow">
            {items.map((it, i) =>
              it.kind === 'rule' ? (
                <hr className="tr-rule" key={i} aria-hidden="true" />
              ) : it.kind === 'blank' ? (
                <div className="tr-blank" key={i} aria-hidden="true" />
              ) : it.kind === 'code' ? (
                /* Its own scroller. The lines keep their columns and the
                   reader pans THIS block, instead of the whole screen. */
                <pre className="tr-code" key={i} tabIndex={0}>
                  {it.lines.join('\n')}
                </pre>
              ) : it.kind === 'list' ? (
                <ul className="tr-list" key={i}>
                  {it.items.map((li, j) => (
                    <li key={j}>{renderInline(li)}</li>
                  ))}
                </ul>
              ) : (
                <p className="tr-para" key={i}>
                  {renderInline(it.text)}
                </p>
              ),
            )}
          </div>
        )}
      </div>
      <Provenance
        target={target}
        source={source}
        truncated={truncated}
        collapsed={collapsed}
        rejoined={rejoined}
        codeBlocks={codeBlocks}
        cleaned={cleaned}
      />
    </>
  )
}

/**
 * The honesty line (J4). It is deliberately always on screen and deliberately
 * not dismissible: a reader must be able to tell, without asking, that they are
 * looking at a rendering of a screen and not at a conversation log we
 * reconstructed. It also names every transform we applied, so nothing we did to
 * the text is silent.
 */
function Provenance({
  target,
  source,
  truncated,
  collapsed,
  rejoined,
  codeBlocks,
  cleaned,
}: {
  target: PaneId
  source?: string
  truncated: boolean
  collapsed: number
  rejoined: number
  codeBlocks: number
  cleaned?: { chrome: number; blank: number; code?: number }
}) {
  const what = source ? (SOURCE_LABEL[source] ?? source) : 'this pane'
  // Every transform is named. The list is built rather than hard-coded so a
  // transform that did nothing this frame says nothing, and one we add later
  // cannot be left out of the sentence by accident.
  const applied = ['escape codes stripped']
  // What the SERVER removed, reported from the frame's own `cleaned` record
  // rather than inferred here. The sentence exists so nobody is handed a
  // different shape than the pane actually showed and has to work out why --
  // which means it has to account for the server's half too, not just ours.
  if (cleaned?.chrome) {
    applied.push(
      `${cleaned.chrome} line${cleaned.chrome === 1 ? '' : 's'} of the agent's own furniture removed (its input caret, context meter and mode banner)`,
    )
  }
  if (collapsed > 0) applied.push(`${collapsed} blank line${collapsed === 1 ? '' : 's'} collapsed`)
  if (rejoined > 0) {
    applied.push(`${rejoined} line${rejoined === 1 ? '' : 's'} rejoined where the pane wrapped them`)
  }
  if (codeBlocks > 0) {
    applied.push(
      `${codeBlocks} code block${codeBlocks === 1 ? '' : 's'} left unwrapped so ${
        codeBlocks === 1 ? 'it keeps its' : 'they keep their'
      } columns`,
    )
  }
  const transforms =
    applied.length === 1
      ? applied[0]
      : `${applied.slice(0, -1).join(', ')} and ${applied[applied.length - 1]}`
  return (
    <p className="tr-provenance">
      This is what <b>{target.split('/').pop()}</b> is showing on screen — {what}, via
      herdr <code>{source ?? '…'}</code>, with {transforms}.
      {truncated ? ' Older output is above what the buffer keeps.' : ''} It is not a
      reconstructed conversation log: herdr exposes the screen, not the agent&apos;s message
      history. Opening it does not resize the pane.
    </p>
  )
}

/** y / n / enter / esc / arrows / 1 2 3 — J2's set, over agent.send_keys. */
function AnswerKeys({ target }: { target: PaneId }) {
  const [busy, setBusy] = useState(false)
  const send = (key: string) => {
    if (busy) return
    setBusy(true)
    void sendCommand('agent.send_keys', { target, keys: [key] }).finally(() => setBusy(false))
  }
  return (
    <div className="tr-keys" role="group" aria-label="Answer the agent">
      {ANSWER_KEYS.map((k) => (
        <button
          key={k.label}
          type="button"
          className={`key key-sm${k.tone ? ` key-${k.tone}` : ''}`}
          onPointerDown={(e) => {
            e.preventDefault()
            send(k.key)
          }}
        >
          {k.label}
        </button>
      ))}
      <button
        type="button"
        className="key key-sm key-enter"
        onPointerDown={(e) => {
          e.preventDefault()
          send('enter')
        }}
      >
        ⏎
      </button>
    </div>
  )
}

/**
 * The prompt box. `agent.prompt` is Herdr's own submit path, so it is not a
 * keystroke race against the TUI's own input handling, and it carries no
 * geometry.
 */
function PromptBox({ target }: { target: PaneId }) {
  const [text, setText] = useState('')
  const [state, setState] = useState<'idle' | 'sending' | 'sent' | 'failed'>('idle')
  const [error, setError] = useState<string | null>(null)
  const [shots, setShots] = useState<Upload[]>([])
  const [busy, setBusy] = useState(0)
  const [dragging, setDragging] = useState(false)
  const fileRef = useRef<HTMLInputElement | null>(null)

  // An attachment is shown, not its path. A temp path is ninety characters of
  // noise in a text box; the person wants to see THAT THEY ATTACHED A THING.
  // The agent gets the path, because a path is the only form it can act on.
  const attach = (files: File[]) => {
    if (files.length === 0) return
    setError(null)
    setBusy((n) => n + files.length)
    for (const f of files) {
      void uploadImage(f, target)
        .then((u) => setShots((prev) => [...prev, u]))
        .catch((e: unknown) => {
          setState('failed')
          setError(e instanceof Error ? e.message : 'the image was not accepted')
        })
        .finally(() => setBusy((n) => n - 1))
    }
  }

  const drop = (u: Upload) => {
    URL.revokeObjectURL(u.preview)
    setShots((prev) => prev.filter((x) => x !== u))
  }

  // The result is cleared on the next edit rather than on a timer: a status
  // that vanishes by itself is a layout shift you did not ask for.
  useEffect(() => {
    if (text !== '' && (state === 'sent' || state === 'failed')) setState('idle')
  }, [text, state])

  const submit = () => {
    const typed = text.trim()
    // The paths go with the message, one per line, so an agent can read them
    // straight out of the prompt. A message that is ONLY images is still worth
    // sending: the paths are the content.
    const body = [typed, ...shots.map((s) => s.path)].filter(Boolean).join('\n')
    if (!body || busy > 0 || state === 'sending') return
    setState('sending')
    setError(null)
    void sendCommand('agent.prompt', { target, text: body }).then((r) => {
      if (r.ok) {
        setText('')
        shots.forEach((s) => URL.revokeObjectURL(s.preview))
        setShots([])
        setState('sent')
      } else {
        setState('failed')
        setError(r.error ?? 'the agent did not accept it')
      }
    })
  }

  return (
    <form
      className={`tr-prompt${dragging ? ' is-dragging' : ''}`}
      onSubmit={(e) => {
        e.preventDefault()
        submit()
      }}
      onDragOver={(e) => {
        if (imagesFrom(e.dataTransfer).length > 0 || e.dataTransfer?.types.includes('Files')) {
          e.preventDefault()
          setDragging(true)
        }
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        const files = imagesFrom(e.dataTransfer)
        setDragging(false)
        if (files.length === 0) return
        e.preventDefault()
        attach(files)
      }}
    >
      {shots.length > 0 || busy > 0 ? (
        <div className="tr-shots">
          {shots.map((u) => (
            <span className="tr-shot" key={u.path}>
              <img src={u.preview} alt="" />
              <span className="tr-shot-name" title={u.path}>
                {u.name}
                {u.machine ? ` · ${u.machine}` : ''}
              </span>
              <button
                type="button"
                className="tr-shot-x"
                aria-label={`Remove ${u.name}`}
                onClick={() => drop(u)}
              >
                ✕
              </button>
            </span>
          ))}
          {busy > 0 ? <span className="tr-shot is-busy">uploading…</span> : null}
        </div>
      ) : null}
      <input
        ref={fileRef}
        type="file"
        accept="image/*"
        multiple
        hidden
        onChange={(e) => {
          attach(Array.from(e.target.files ?? []))
          e.target.value = ''
        }}
      />
      <button
        type="button"
        className="tr-clip"
        aria-label="Attach an image"
        title="Attach an image"
        onClick={() => fileRef.current?.click()}
      >
        ⊕
      </button>
      <textarea
        className="tr-input"
        value={text}
        rows={1}
        placeholder="Send a prompt to this agent…"
        aria-label="Send a prompt to this agent"
        onChange={(e) => setText(e.target.value)}
        onPaste={(e) => {
          // A pasted screenshot arrives as a file on the clipboard. Take it and
          // let the text through untouched -- pasting text must keep working
          // exactly as it did, including multi-line, which lands as one value
          // because this is a textarea and not a terminal.
          const files = imagesFrom(e.clipboardData)
          if (files.length === 0) return
          e.preventDefault()
          attach(files)
        }}
        onKeyDown={(e) => {
          // Enter submits; shift+enter is a newline. A phone keyboard has no
          // other obvious submit, and the button is right there for the rest.
          if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault()
            submit()
          }
        }}
      />
      <button
        type="submit"
        className="tr-send"
        disabled={state === 'sending' || busy > 0 || (text.trim() === '' && shots.length === 0)}
      >
        {state === 'sending' ? '…' : 'Send'}
      </button>
      {/* Fixed-height status row: it is always in the layout, so a result
          appearing never moves the prompt box under the user's thumb. */}
      <span className={`tr-status is-${state}`} role="status">
        {state === 'sent'
          ? 'Sent to the agent.'
          : state === 'failed'
            ? `Not sent — ${error}`
            : ''}
      </span>
    </form>
  )
}
