/**
 * The empty state, shared by the phone list and the desktop grid.
 *
 * "Waiting for the tree from herdr-expose" is only true when we are CONNECTED
 * and the server has not sent one yet. Saying it while unauthenticated or
 * offline is a lie that sends people looking for a Herdr bug that isn't there —
 * so the copy is derived from the real link state (SPEC §8: say it plainly).
 */

import { useStore } from '../store/store'

export function EmptyPanes() {
  const link = useStore((s) => s.link)
  const authRequired = useStore((s) => s.authRequired)
  const unreachable = useStore((s) => s.unreachable)
  const lastError = useStore((s) => s.lastError)

  let title = 'No panes yet'
  let sub = 'Waiting for the tree from herdr-expose.'

  if (authRequired) {
    title = 'Not paired'
    sub = 'This device has no valid token, so the server is not sending anything.'
  } else if (unreachable) {
    title = "Can't reach herdr-expose"
    sub = lastError ?? 'The server stopped answering. Check that it is still running.'
  } else if (link === 'offline') {
    title = 'Disconnected'
    sub = lastError ?? 'No live connection, so there is nothing to show.'
  } else if (link === 'reconnecting' || link === 'connecting') {
    title = link === 'connecting' ? 'Connecting…' : 'Reconnecting…'
    sub = 'The pane list appears as soon as the link is up.'
  }

  return (
    <div className="empty">
      <p className="empty-title">{title}</p>
      <p className="empty-sub">{sub}</p>
    </div>
  )
}
