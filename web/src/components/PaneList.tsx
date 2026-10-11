/**
 * The pane list — TWO levels, on purpose: session, then panes.
 *
 * The tree really is sessions > workspaces > tabs > panes, and an earlier
 * version rendered every level generically. That produced three plumbing rows
 * per pane, labelled with raw ids (`chromium/w1:t1`), which is unusable with
 * four sessions running. Workspace and tab ids are ROUTING information: they
 * already live inside the pane's target id, which is where they belong. The
 * screen shows what a human needs — which session, which pane, what state.
 *
 * Also true here:
 *   - A SEARCH BOX, because triage stops working at scale. Measured on the
 *     machine this was asked from: 33 panes, 2 sessions, 20 directories, and
 *     two rows whose titles are the same sentence. Finding one by scrolling is
 *     a game of memory; `herdr-expose agents QUERY` has been the only honest
 *     way to do it, and that is a terminal the phone does not have. The query
 *     searches the same fields that CLI does.
 *   - Searching REPLACES the tree with a flat result list, collapse and all.
 *     A match hidden inside a collapsed session is not a match anyone found,
 *     and re-expanding sections to reveal hits makes the list jump as you type.
 *   - Which means a search also NARROWS the viewport declaration: filtering 33
 *     panes to 3 stops streaming the other 30. The search box is cheaper than
 *     the list it replaces.
 *   - "Needs you" stays pinned at the top (blocked, then unseen-done).
 *   - Sessions collapse, and the choice is remembered. By default only the
 *     session holding the focused pane is open, so opening the app is never a
 *     wall of text.
 *   - COLLAPSED PANES ARE NOT DECLARED. SPEC §3 says declare what we RENDER, so
 *     a collapsed session costs no summary stream at all — which is the single
 *     biggest bandwidth and re-render win available here.
 */

import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  AGENT_STATE_ORDER,
  agentUrgency,
  type AgentState,
  type PaneId,
  type TreePane,
  type ViewportMode,
} from '../protocol/types'
import { collectSessions, type SessionGroup } from '../protocol/tree'
import { useStore } from '../store/store'
import { declareViewport, releaseViewport } from '../net/viewport'
import { AgentBadge } from './AgentBadge'
import { EmptyPanes } from './EmptyPanes'
import { PaneRowSub } from './PaneRowSub'
import { folderName, matchesTerms, paneHaystack, queryTerms, shortTitle } from './paneSearch'

/** Pinned at the top: the states that are actually waiting on a human. */
const NEEDS_YOU = new Set<AgentState>(['blocked', 'done'])

const COLLAPSE_KEY = 'herdr-expose.collapsed'

export function PaneList({
  onOpen,
  selected,
  stateFilter,
  onClearStateFilter,
}: {
  onOpen: (id: PaneId) => void
  selected?: PaneId | null
  /** Set by tapping a count in Stats. Narrows the list to one agent state. */
  stateFilter?: AgentState | null
  onClearStateFilter?: () => void
}) {
  const tree = useStore((s) => s.tree)
  const focusedPane = useStore((s) => s.tree.focused_pane ?? s.tree.focused ?? null)

  const sessions = useMemo(() => collectSessions(tree), [tree])
  const all = useMemo(() => sessions.flatMap((s) => s.panes), [sessions])

  const [query, setQuery] = useState('')
  const terms = useMemo(() => queryTerms(query), [query])
  const filtering = terms.length > 0 || Boolean(stateFilter)

  /** Which session each pane belongs to — a result row has to say. */
  const sessionOf = useMemo(() => {
    const m = new Map<PaneId, string>()
    for (const s of sessions) for (const p of s.panes) m.set(p.id, s.name)
    return m
  }, [sessions])

  /**
   * Matches, in the server's own order across every session. Order is NOT
   * urgency-sorted here: a result list that reorders itself while you type is
   * one where the row under your thumb is not the row you aimed at.
   */
  const matches = useMemo(() => {
    if (!filtering) return []
    return all.filter((p) => {
      if (stateFilter && (p.agent?.state ?? 'unknown') !== stateFilter) return false
      if (terms.length === 0) return true
      return matchesTerms(paneHaystack(p, sessionOf.get(p.id) ?? ''), terms)
    })
  }, [all, filtering, stateFilter, terms, sessionOf])

  /** Urgency first, then the server's order — stable, so rows do not shuffle. */
  const needsYou = useMemo(
    () =>
      all
        .filter((p) => NEEDS_YOU.has((p.agent?.state ?? 'unknown') as AgentState))
        .sort((a, b) => agentUrgency(a.agent?.state) - agentUrgency(b.agent?.state)),
    [all],
  )
  const pinned = useMemo(() => new Set(needsYou.map((p) => p.id)), [needsYou])

  // The session to leave open: whatever the user is looking at, else the one
  // the server says is focused.
  const activeSession = useMemo(() => {
    const target = selected ?? focusedPane
    if (!target) return sessions[0]?.id ?? ''
    return sessions.find((s) => s.panes.some((p) => p.id === target))?.id ?? ''
  }, [sessions, selected, focusedPane])

  const { isOpen, toggle } = useCollapse(activeSession)

  /**
   * SPEC §3: report what we actually RENDER. A pane inside a collapsed session
   * is not on screen, so it is absent from the map — which the server reads as
   * `none` and stops streaming.
   */
  const rendered = useMemo(() => {
    // While filtering, the result list IS the screen: nothing else is mounted,
    // so nothing else is declared. This is the cheap case, not the costly one.
    if (filtering) return matches.map((p) => p.id)
    const ids = new Set<PaneId>(needsYou.map((p) => p.id))
    for (const s of sessions) if (isOpen(s.id)) for (const p of s.panes) ids.add(p.id)
    return [...ids]
  }, [sessions, needsYou, isOpen, filtering, matches])

  const declaredKey = rendered.join('\x00')
  useEffect(() => {
    const targets: Record<PaneId, ViewportMode> = {}
    // Rows are summaries. The focused pane's `live` comes from the terminal
    // itself; the registry merges the two, live winning.
    for (const id of declaredKey ? declaredKey.split('\x00') : []) targets[id] = 'summary'
    declareViewport('panelist', targets)
    return () => releaseViewport('panelist')
  }, [declaredKey])

  if (all.length === 0) return <EmptyPanes />

  return (
    <div className="panelist">
      <SearchBar
        query={query}
        onQuery={setQuery}
        stateFilter={stateFilter ?? null}
        onClearStateFilter={onClearStateFilter}
        total={all.length}
      />

      {filtering ? (
        <ResultList
          matches={matches}
          sessionOf={sessionOf}
          selected={selected}
          onOpen={onOpen}
          onClear={() => {
            setQuery('')
            onClearStateFilter?.()
          }}
        />
      ) : (
        <>
      {needsYou.length > 0 ? (
        <section className="ws ws-urgent">
          <h2 className="ws-title ws-title-urgent">Needs you</h2>
          <Rows panes={needsYou} selected={selected} onOpen={onOpen} />
        </section>
      ) : null}

      {sessions.map((s) => (
        <SessionSection
          key={s.id || 'session'}
          session={s}
          open={isOpen(s.id)}
          onToggle={() => toggle(s.id)}
          skip={pinned}
          selected={selected}
          onOpen={onOpen}
        />
      ))}
        </>
      )}
    </div>
  )
}

/**
 * The search box, and the one chip that says a state filter is on.
 *
 * `type="search"` for the platform clear button and the right keyboard;
 * `autoCapitalize/autoCorrect` off because a directory name is not a sentence
 * and iOS will happily turn `herdrmove` into `Herd rmove`. The 16px font size
 * in the stylesheet is not a taste choice: below 16px, iOS Safari zooms the
 * page on focus and never zooms back out.
 */
function SearchBar({
  query,
  onQuery,
  stateFilter,
  onClearStateFilter,
  total,
}: {
  query: string
  onQuery: (v: string) => void
  stateFilter: AgentState | null
  onClearStateFilter?: () => void
  total: number
}) {
  return (
    <div className="pl-search">
      <span className="pl-search-icon" aria-hidden="true">
        ⌕
      </span>
      <input
        className="pl-search-input"
        type="search"
        value={query}
        onChange={(e) => onQuery(e.target.value)}
        placeholder={`Search ${total} panes — folder, title, session`}
        aria-label="Search panes"
        autoCapitalize="off"
        autoCorrect="off"
        spellCheck={false}
        enterKeyHint="search"
      />
      {query ? (
        <button
          type="button"
          className="pl-search-clear"
          onClick={() => onQuery('')}
          aria-label="Clear search"
        >
          ✕
        </button>
      ) : null}
      {stateFilter ? (
        <button
          type="button"
          className="pl-search-state"
          onClick={onClearStateFilter}
          aria-label={`Stop filtering by ${stateFilter}`}
        >
          {stateFilter} ✕
        </button>
      ) : null}
    </div>
  )
}

/** Flat matches, with the session named on every row. */
function ResultList({
  matches,
  sessionOf,
  selected,
  onOpen,
  onClear,
}: {
  matches: TreePane[]
  sessionOf: Map<PaneId, string>
  selected?: PaneId | null
  onOpen: (id: PaneId) => void
  onClear: () => void
}) {
  if (matches.length === 0) {
    return (
      <div className="pl-none">
        <p>Nothing matches.</p>
        <button type="button" className="pl-none-clear" onClick={onClear}>
          Show everything
        </button>
      </div>
    )
  }
  return (
    <section className="ws ws-results">
      <h2 className="ws-title">
        {matches.length} {matches.length === 1 ? 'match' : 'matches'}
      </h2>
      <ul className="rows">
        {matches.map((p) => (
          <PaneRow
            key={p.id}
            pane={p}
            session={sessionOf.get(p.id)}
            active={p.id === selected}
            onOpen={onOpen}
          />
        ))}
      </ul>
    </section>
  )
}

function SessionSection({
  session,
  open,
  onToggle,
  skip,
  selected,
  onOpen,
}: {
  session: SessionGroup
  open: boolean
  onToggle: () => void
  skip: Set<PaneId>
  selected?: PaneId | null
  onOpen: (id: PaneId) => void
}) {
  const visible = session.panes.filter((p) => !skip.has(p.id))
  // Rollup uses EVERY pane, including the pinned ones: the header answers
  // "anything happening in here?", which is still true when the urgent row has
  // been lifted to the top.
  const rollup = rollupState(session.panes)

  return (
    <section className="ws ws-session">
      <button
        className={`session-head${open ? ' is-open' : ''}`}
        onClick={onToggle}
        aria-expanded={open}
      >
        <span className={`session-caret${open ? ' is-open' : ''}`} aria-hidden="true">
          ›
        </span>
        <span className="session-name">{session.name}</span>
        {rollup ? (
          <i className={`session-dot session-dot-${rollup}`} aria-hidden="true" title={rollup} />
        ) : null}
        <span className="session-count">{session.panes.length}</span>
        {!session.connected ? <span className="chip chip-dead">offline</span> : null}
      </button>

      {open ? (
        session.areas.length > 0 ? (
          session.areas.map((area) => {
            const panes = area.panes.filter((p) => !skip.has(p.id))
            if (panes.length === 0) return null
            return (
              <div key={area.key} className="area">
                {/* A real human label, never an id — that is the whole test. */}
                <h3 className="area-title">{area.label}</h3>
                <Rows panes={panes} selected={selected} onOpen={onOpen} />
              </div>
            )
          })
        ) : visible.length > 0 ? (
          <Rows panes={visible} selected={selected} onOpen={onOpen} />
        ) : (
          <p className="session-empty">Everything here is in “Needs you”.</p>
        )
      ) : null}
    </section>
  )
}

/** Herdr's urgency order, rolled up to one dot for the session header. */
function rollupState(panes: TreePane[]): AgentState | null {
  let best: AgentState | null = null
  for (const p of panes) {
    const s = p.agent?.state
    if (!s || s === 'unknown') continue
    if (!best || agentUrgency(s) < agentUrgency(best)) best = s
    if (best === AGENT_STATE_ORDER[0]) break
  }
  return best
}

/**
 * Collapsed state, remembered per session. Only EXPLICIT choices are stored, so
 * the sensible default (open the session you are working in, collapse the rest)
 * keeps applying to sessions the user has never touched.
 */
function useCollapse(activeSession: string) {
  const [choices, setChoices] = useState<Record<string, boolean>>(() => {
    try {
      const raw = localStorage.getItem(COLLAPSE_KEY)
      const parsed = raw ? (JSON.parse(raw) as Record<string, boolean>) : {}
      return parsed && typeof parsed === 'object' ? parsed : {}
    } catch {
      return {}
    }
  })

  useEffect(() => {
    try {
      localStorage.setItem(COLLAPSE_KEY, JSON.stringify(choices))
    } catch {
      /* private mode: the choice just does not outlive the session */
    }
  }, [choices])

  const isOpen = useCallback(
    (id: string) => choices[id] ?? id === activeSession,
    [choices, activeSession],
  )
  const toggle = useCallback(
    (id: string) => setChoices((c) => ({ ...c, [id]: !(c[id] ?? id === activeSession) })),
    [activeSession],
  )
  return { isOpen, toggle }
}

function Rows({
  panes,
  selected,
  onOpen,
}: {
  panes: TreePane[]
  selected?: PaneId | null
  onOpen: (id: PaneId) => void
}) {
  return (
    <ul className="rows">
      {panes.map((p) => (
        <PaneRow key={p.id} pane={p} active={p.id === selected} onOpen={onOpen} />
      ))}
    </ul>
  )
}

export function PaneRow({
  pane,
  active,
  onOpen,
  session,
}: {
  pane: TreePane
  active?: boolean
  onOpen: (id: PaneId) => void
  /** Shown only in the flat result list, where sections no longer say it. */
  session?: string
}) {
  const state = pane.agent?.state
  // The folder is the IDENTITY. Two rows on this machine carry the same title
  // verbatim; `agents/site` and `agents/marketing-team` are what tell them
  // apart, so the last segment leads the row and the sentence follows it.
  const folder = folderName(pane.cwd)
  const name = shortTitle(pane)
  // FOLDER FIRST, session second — the opposite of the obvious order, and a
  // screenshot is what settled it. Session-first rendered
  // `deemwar-one-os · cr…`: the shared prefix survived in full and the word
  // the search actually matched was the one that got cut. The clip has to
  // land on the part that is the same on every row.
  // And `x · x` says one thing twice: a session whose own directory is the
  // session is named once.
  const place = session && session !== folder ? `${folder} · ${session}` : folder || session || ''
  // Suppressed when it would repeat the name: a shell pane whose title IS its
  // path renders both from the same word.
  const showPlace = folder !== '' && folder !== name

  return (
    <li>
      <button
        className={`row${active ? ' is-active' : ''}${state ? ` row-${state}` : ''}`}
        // A STABLE handle for the pane this row opens. Row order is deliberately
        // not stable — "Needs you" re-pins a pane the moment it blocks — and a
        // title is whatever the shell last wrote to OSC 0/2, so neither is a
        // safe way for anything outside React to find a specific pane.
        data-target={pane.id}
        onClick={() => onOpen(pane.id)}
        aria-current={active ? 'true' : undefined}
      >
        <span className="row-head">
          <span className="row-name">
            {showPlace ? (
              <span className="row-folder" title={pane.cwd || undefined}>
                {place}
              </span>
            ) : null}
            <span className="row-title" title={pane.title}>
              {name}
            </span>
          </span>
          {pane.dead ? <span className="chip chip-dead">closed</span> : null}
          {state ? <AgentBadge state={state} /> : null}
        </span>
        <PaneRowSub pane={pane} name={name} />
      </button>
    </li>
  )
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n}B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)}KB`
  return `${(n / 1024 / 1024).toFixed(1)}MB`
}
