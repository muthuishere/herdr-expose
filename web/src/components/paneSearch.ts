/**
 * Two problems that turn out to be the same problem: a 33-row list where every
 * row looks alike, and a path too long to read on a phone.
 *
 * MEASURED on this machine, which is where the complaint came from: 33 panes
 * across 2 sessions, 20 distinct working directories, and titles like
 * "Marketing agent context recycle from 83fbaeea" repeated verbatim on two
 * different rows. The thing that tells those rows apart is the DIRECTORY —
 * `marketing-team` vs `site` — and until now the list never showed it. The
 * detail view did show it, as `/Users/<me>/muthu/deemwarworkspace/agents/site`
 * truncated from the left to 34 characters, which on a 390px screen is a wall
 * of shared prefix: every path on this machine starts with the same 30
 * characters, so the one part that identifies anything was the part that got
 * cut.
 *
 * So: show the LAST segment, and make it searchable. `basename` is not a
 * compromise here — it is the identifying part. The full path is still one
 * long-press away in the `title` attribute, where it costs no pixels.
 */

import type { TreePane } from '../protocol/types'

/**
 * The last segment of a path: what a human calls the folder.
 *
 *   /Users/me/muthu/deemwarworkspace/agents/site   -> site
 *   ~/.herdr/worktrees/callbox/kidtoys             -> kidtoys
 *   /Users/me                                      -> ~
 *
 * The home directory is special-cased to `~` because its basename is the
 * account name, which identifies nothing — every pane on the machine would
 * wear the same label.
 */
export function folderName(path?: string | null): string {
  const s = (path ?? '').trim().replace(/\/+$/, '')
  if (s === '' || s === '~' || s === '/') return s === '/' ? '/' : '~'
  // `/Users/<name>` and `/home/<name>` ARE the home directory.
  if (/^\/(Users|home)\/[^/]+$/.test(s)) return '~'
  const i = s.lastIndexOf('/')
  return i < 0 ? s : s.slice(i + 1)
}

/**
 * Does this title look like a filesystem path rather than a sentence?
 *
 * It matters because a pane's title is whatever the shell last wrote to OSC
 * 0/2, and for a plain shell that is routinely the whole cwd. Such a title is
 * shown as its own last segment, for exactly the reason above.
 *
 * Deliberately narrow: it must START like a path, so a sentence that merely
 * mentions `internal/core/hub.go` keeps all of its words.
 */
export function isPathLike(title: string): boolean {
  return /^(~|\.{0,2})?\//.test(title) && title.includes('/')
}

/** What a row shows as its name: short, and never a bare pane id if avoidable. */
export function shortTitle(pane: TreePane): string {
  const t = (pane.title ?? '').trim()
  if (t === '') return folderName(pane.cwd) || pane.id
  return isPathLike(t) ? folderName(t) : t
}

/**
 * Everything a search is allowed to match, lower-cased and joined.
 *
 * The fields mirror `herdr-expose agents QUERY`, which already searches "name,
 * session, pane, cwd and title" — a query that works in the terminal should
 * find the same pane here. The full cwd stays in the haystack even though it is
 * never rendered in full: typing `deemwarworkspace` should still work.
 */
export function paneHaystack(pane: TreePane, session: string): string {
  return [
    pane.title,
    pane.id,
    session,
    pane.cwd,
    folderName(pane.cwd),
    pane.command,
    pane.agent?.kind,
    pane.agent?.state,
    pane.agent?.summary,
  ]
    .filter(Boolean)
    .join(' ')
    .toLowerCase()
}

/** Whitespace-separated terms, all of which must match. Empty = match all. */
export function queryTerms(q: string): string[] {
  return q.trim().toLowerCase().split(/\s+/).filter(Boolean)
}

/**
 * AND across terms, substring within each.
 *
 * AND rather than OR because the useful query on this machine is two weak
 * words — `marketing recycle`, `agents site` — and OR would return everything
 * that mentions either, which is the list you started with.
 */
export function matchesTerms(haystack: string, terms: string[]): boolean {
  for (const t of terms) if (!haystack.includes(t)) return false
  return true
}
