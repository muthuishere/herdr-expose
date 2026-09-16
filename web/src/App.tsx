/**
 * App shell.
 *
 * MOBILE IS THE BASE CASE. The phone layout is what this component renders;
 * desktop is a second branch chosen by WIDTH ONLY at 900px, evaluated
 * synchronously on first render so a phone never flashes the desktop header
 * (SPEC B8).
 */

import { useCallback, useEffect, useState } from 'react'
import type { PaneId } from './protocol/types'
import { useStore, setFocused } from './store/store'
import { useIsMobile } from './hooks/useViewport'
import { PaneList } from './components/PaneList'
import { PaneView } from './components/PaneView'
import { ConnectionBadge } from './components/AgentBadge'
import { DesktopGrid } from './components/DesktopGrid'

export function App() {
  const isMobile = useIsMobile()
  const link = useStore((s) => s.link)
  const rttMs = useStore((s) => s.rttMs)
  const attempt = useStore((s) => s.attempt)
  const lastError = useStore((s) => s.lastError)
  const focused = useStore((s) => s.focused)
  const panesById = useStore((s) => s.panesById)

  const [open, setOpen] = useState<PaneId | null>(null)

  const openPane = useCallback((id: PaneId) => {
    setOpen(id)
    setFocused(id)
  }, [])

  const closePane = useCallback(() => {
    setOpen(null)
    setFocused(null)
  }, [])

  // A pane the server dropped must not leave us staring at a dead terminal.
  useEffect(() => {
    if (open && Object.keys(panesById).length > 0 && !panesById[open]) closePane()
  }, [open, panesById, closePane])

  // Hardware back / swipe-back closes the pane instead of leaving the app.
  useEffect(() => {
    if (!open) return
    window.history.pushState({ pane: open }, '')
    const onPop = () => closePane()
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [open, closePane])

  const disconnected = link === 'offline' || link === 'reconnecting'

  if (isMobile) {
    return (
      <div className="app app-mobile">
        {open ? (
          <PaneView target={open} onBack={() => window.history.back()} showBack />
        ) : (
          <>
            <header className="appbar">
              <span className="brand">herdr</span>
              <ConnectionBadge link={link} rttMs={rttMs} attempt={attempt} />
            </header>
            <main className="scroll">
              {disconnected ? <Disconnected link={link} error={lastError} /> : null}
              <PaneList onOpen={openPane} selected={focused} />
            </main>
          </>
        )}
      </div>
    )
  }

  return (
    <div className="app app-desktop">
      <aside className="sidebar">
        <header className="appbar">
          <span className="brand">herdr</span>
          <ConnectionBadge link={link} rttMs={rttMs} attempt={attempt} />
        </header>
        <div className="scroll">
          <PaneList onOpen={openPane} selected={open} />
        </div>
      </aside>
      <main className="main">
        {disconnected ? <Disconnected link={link} error={lastError} /> : null}
        {open ? (
          <PaneView target={open} onBack={closePane} />
        ) : (
          <DesktopGrid onOpen={openPane} />
        )}
      </main>
    </div>
  )
}

/**
 * The only "offline" surface we ship. A terminal is inherently online — we say
 * so plainly rather than faking offline capability (SPEC §8).
 */
function Disconnected({ link, error }: { link: string; error: string | null }) {
  return (
    <div className="disconnected" role="status">
      <span className="disconnected-dot" aria-hidden="true" />
      <div>
        <b>{link === 'offline' ? 'Disconnected' : 'Reconnecting…'}</b>
        <span className="disconnected-sub">
          {error ?? 'A terminal needs a live connection. Showing the last state received.'}
        </span>
      </div>
    </div>
  )
}
