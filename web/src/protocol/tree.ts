/**
 * Generic tree walkers.
 *
 * The server owns the tree's SHAPE and it is about to grow a level: sessions[]
 * -> workspaces[] -> tabs[] -> panes[]. Nothing in the UI should have to change
 * for that, so nothing here names a level. The rules are only:
 *
 *   - an array of objects under any key is a list of GROUPS to render
 *   - except `panes`, which is the leaf list
 *
 * That renders whatever the server sends, in the order it sent it (SPEC §8:
 * structure is never derived client-side).
 */

import type { PaneId, TreeData, TreePane, TreeSession } from './types'

export interface TreeGroup {
  /** Stable React key; includes the path so ids only need to be locally unique. */
  key: string
  title: string
  panes: TreePane[]
  groups: TreeGroup[]
}

/** Deep enough for sessions>workspaces>tabs>panes with room to spare. */
const MAX_DEPTH = 8

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function panesOf(node: Record<string, unknown>): TreePane[] {
  const raw = node.panes
  if (!Array.isArray(raw)) return []
  return raw.filter((p): p is TreePane => isRecord(p) && typeof p.id === 'string')
}

export function collectGroups(root: unknown, depth = 0, path = ''): TreeGroup[] {
  if (!isRecord(root) || depth > MAX_DEPTH) return []
  const out: TreeGroup[] = []
  for (const [key, value] of Object.entries(root)) {
    if (key === 'panes' || !Array.isArray(value)) continue
    value.forEach((node, i) => {
      if (!isRecord(node)) return
      const id = typeof node.id === 'string' ? node.id : String(i)
      const childPath = `${path}/${key}:${id}`
      out.push({
        key: childPath,
        title: typeof node.title === 'string' ? node.title : id,
        panes: panesOf(node),
        groups: collectGroups(node, depth + 1, childPath),
      })
    })
  }
  return out
}

/** Every pane in the tree, in the server's own order. */
export function collectPanes(root: unknown, depth = 0): TreePane[] {
  if (!isRecord(root) || depth > MAX_DEPTH) return []
  const out: TreePane[] = []
  for (const [key, value] of Object.entries(root)) {
    if (!Array.isArray(value)) continue
    if (key === 'panes') {
      out.push(...panesOf(root))
      continue
    }
    for (const node of value) out.push(...collectPanes(node, depth + 1))
  }
  return out
}

export function paneIds(root: unknown): PaneId[] {
  return collectPanes(root).map((p) => p.id)
}

/* ------------------------------------------------------------------ */
/* The two levels a HUMAN needs: session, then panes                   */
/* ------------------------------------------------------------------ */

export interface SessionGroup {
  id: string
  /** Human name. Never an internal id if anything better exists. */
  name: string
  running: boolean
  connected: boolean
  focusedPane?: PaneId
  /** Every pane in the session, flattened. Tabs are plumbing, not UI. */
  panes: TreePane[]
  /**
   * Optional sub-groups, present ONLY when a workspace carries a real human
   * label that adds information. Routing levels (workspace/tab ids like
   * `chromium/w1:t1`) are never shown — they live in the target id, which is
   * where routing information belongs.
   */
  areas: { key: string; label: string; panes: TreePane[] }[]
}

/** The last path segment of a namespaced id: `chromium/w1` -> `w1`. */
function localId(id: string): string {
  const i = id.lastIndexOf('/')
  return i < 0 ? id : id.slice(i + 1)
}

/**
 * Session groups, tolerant of both shapes: the multi-session tree (sessions[])
 * and the older single-session one (workspaces[] at the root, rendered as one
 * unnamed session).
 */
export function collectSessions(tree: TreeData | undefined): SessionGroup[] {
  if (!tree) return []
  const sessions: TreeSession[] = Array.isArray(tree.sessions)
    ? tree.sessions
    : Array.isArray(tree.workspaces)
      ? [{ id: '', name: '', workspaces: tree.workspaces }]
      : []

  return sessions.map((s) => {
    const name = (s.name || s.id || '').trim()
    const workspaces = Array.isArray(s.workspaces) ? s.workspaces : []
    const areas: SessionGroup['areas'] = []
    const panes: TreePane[] = []

    for (const ws of workspaces) {
      const wsPanes = collectPanes(ws)
      if (wsPanes.length === 0) continue
      panes.push(...wsPanes)
      const label = (ws.label || ws.title || '').trim()
      // A label only earns a subheading when it says something the session
      // header does not, and only when there is more than one to tell apart.
      const informative =
        label !== '' &&
        label !== name &&
        label !== localId(ws.id ?? '') &&
        label !== (ws.id ?? '') &&
        !/^\d+$/.test(label)
      if (informative) areas.push({ key: ws.id ?? label, label, panes: wsPanes })
    }

    return {
      id: s.id ?? '',
      name: name || 'session',
      running: s.running !== false,
      connected: s.connected !== false,
      focusedPane: s.focused_pane,
      panes,
      // All-or-nothing: a partial split would hide panes under no heading.
      areas: workspaces.length > 1 && areas.length === workspaces.length ? areas : [],
    }
  })
}

/**
 * Replace one pane wherever it sits, without touching the structure around it.
 * Shape-agnostic for the same reason as the walkers above.
 */
export function patchPane<T>(node: T, pane: TreePane, depth = 0): T {
  if (!isRecord(node) || depth > MAX_DEPTH) return node
  let changed = false
  const out: Record<string, unknown> = { ...node }
  for (const [key, value] of Object.entries(node)) {
    if (!Array.isArray(value)) continue
    if (key === 'panes') {
      const next = value.map((p) =>
        isRecord(p) && p.id === pane.id ? pane : p,
      )
      if (next.some((p, i) => p !== value[i])) {
        out[key] = next
        changed = true
      }
      continue
    }
    const next = value.map((child) => patchPane(child, pane, depth + 1))
    if (next.some((c, i) => c !== value[i])) {
      out[key] = next
      changed = true
    }
  }
  return (changed ? (out as unknown as T) : node)
}
